// Package serverconn connects to managed servers over SSH.
//
// It is a thin driver over internal/sshconn: it translates a serverassets
// asset (plus its jump host) into an sshconn.Config, and it answers the
// prompts sshconn raises while dialing — host-key, password, key passphrase
// — non-interactively, from a CredentialProvider the caller supplies. It
// never dials SSH itself and never reads the vault: the provider is how a
// password or passphrase reaches a prompt, and no credential value is ever
// written to a Config, a log line, or a returned error.
//
// Fingerprint policy is deny-by-default. A fingerprint that matches the
// asset's stored HostKeyFingerprint — the key the user already confirmed
// and saved — is trusted via TrustHostKey(true), which also persists it to
// the known-hosts file so later connections verify without prompting. An
// unknown fingerprint is reported as FingerprintUnknown and a changed one
// as FingerprintChanged; in both cases the key is rejected, never trusted,
// and nothing is written to the known-hosts file. Trusting a new key stays
// a deliberate user action in the UI, which records the fingerprint on the
// asset first.
package serverconn

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"motty/internal/serverassets"
	"motty/internal/sshconn"
)

// Timeouts for a connection test. The TCP stage and the SSH handshake stage
// have separate budgets (Motty_Linux.md §5.4): 5s for the TCP dial, 10s for
// a handshake that has not yet reached authentication. The overall test is
// bounded by the caller's ctx.
const (
	// TCPTimeout bounds the phase-1 reachability dial.
	TCPTimeout = 5 * time.Second
	// DialTimeout bounds each TCP leg sshconn opens (direct, or one hop of
	// a jump chain). It matches the handshake budget below.
	DialTimeout = 10 * time.Second
	// pollInterval is how often the test loop reads conn.Status().
	pollInterval = 25 * time.Millisecond
)

// handshakeStallTimeout is how long a handshake may sit in PhaseConnecting
// without reaching authentication or a connection before the test calls it
// SSHUnavailable. A variable (not a const) so tests can shorten it.
var handshakeStallTimeout = 10 * time.Second

// CredentialProvider supplies the secrets a connection test needs, keyed by
// asset UUID (a server ID or a jump-host ID). The implementation reads the
// vault: password under "server-credential:<id>", key passphrase under
// "server-passphrase:<id>" (see serverassets.PasswordCredentialName /
// KeyPassphraseName). found=false means no secret is stored; the provider
// must never return a secret in an error, and this package never logs or
// returns one either.
type CredentialProvider interface {
	Password(id string) (password string, found bool, err error)
	KeyPassphrase(id string) (passphrase string, found bool, err error)
}

// TestStage is which stage of a connection test a result reached or failed in.
type TestStage string

const (
	// StageTCP is the reachability dial of the entry address.
	StageTCP TestStage = "tcp"
	// StageSSH is the SSH handshake (key exchange and host-key verification).
	StageSSH TestStage = "ssh"
	// StageAuth is authentication (password, key, or keyboard-interactive).
	StageAuth TestStage = "auth"
)

// FailureKind classifies why a connection test failed. KindNone means no
// failure (OK is true).
type FailureKind string

const (
	KindNone               FailureKind = ""
	KindNetworkUnreachable FailureKind = "network_unreachable"
	KindSSHUnavailable     FailureKind = "ssh_unavailable"
	KindAuthFailed         FailureKind = "auth_failed"
	KindFingerprintUnknown FailureKind = "fingerprint_unknown"
	KindFingerprintChanged FailureKind = "fingerprint_changed"
	KindJumpHostFailed     FailureKind = "jump_host_failed"
	KindTimeout            FailureKind = "timeout"
	KindInvalidConfig      FailureKind = "invalid_config"
)

// TestResult is the outcome of one connection test.
type TestResult struct {
	ServerID string
	// Stage is the stage the test reached: the failing stage, or StageAuth
	// on success (authentication completed).
	Stage TestStage
	OK    bool
	Kind  FailureKind
	// Fingerprint is the SHA256 fingerprint the server presented when the
	// test stopped on a host-key prompt; FingerprintType is its key
	// algorithm (for example "ssh-ed25519").
	Fingerprint     string
	FingerprintType string
	// FingerprintChanged reports that the presented key differs from a key
	// of the same type already trusted for that address.
	FingerprintChanged bool
	// KnownFingerprints lists the trusted fingerprints of the same key
	// type for the address, when the key changed.
	KnownFingerprints []string
	// JumpHost reports that the failure happened on the jump-host hop
	// rather than on the target server.
	JumpHost bool
	// ServerVersion is the target's SSH identification string on success.
	ServerVersion string
	// Error carries the underlying failure, if any. It never contains a
	// credential value.
	Error error
	// Duration is the wall time the whole test took.
	Duration time.Duration
}

// --- config construction ---

// endpoint normalises one dial endpoint: an empty port means 22. sshconn
// keys its per-endpoint Authentication map by the normalised endpoint, so
// the map keys built here must be normalised the same way.
func endpoint(host string, port int, user string) (sshconn.Endpoint, error) {
	host = strings.TrimSpace(host)
	if host == "" {
		return sshconn.Endpoint{}, errors.New("serverconn: no host")
	}
	if port == 0 {
		port = 22
	}
	if port < 1 || port > 65535 {
		return sshconn.Endpoint{}, fmt.Errorf("serverconn: invalid port %d", port)
	}
	if strings.TrimSpace(user) == "" {
		return sshconn.Endpoint{}, errors.New("serverconn: no user")
	}
	return sshconn.Endpoint{Host: host, Port: uint16(port), User: strings.TrimSpace(user)}, nil
}

// authenticationFor narrows the SSH methods one endpoint may use, following
// workbench's authenticationFor: a password asset offers only
// keyboard-interactive and password and no identity files, so a stray local
// key is never offered; a key asset offers only publickey (plus
// keyboard-interactive for the server's own follow-ups) and requires the
// publickey to succeed first, so it can never fall back to a password.
// The agent is never used: asset connections are fully described by the
// asset, and a developer's local agent keys must not leak into them.
func authenticationFor(method, identityFile string) (sshconn.Authentication, error) {
	switch strings.ToLower(strings.TrimSpace(method)) {
	case serverassets.AuthPassword:
		return sshconn.Authentication{
			Methods:       sshconn.AuthKeyboardInteractive | sshconn.AuthPassword,
			IdentityFiles: []string{},
		}, nil
	case serverassets.AuthKey:
		authentication := sshconn.Authentication{
			Methods:          sshconn.AuthPublicKey | sshconn.AuthKeyboardInteractive,
			RequirePublicKey: true,
		}
		// IdentityFiles is set only when the asset names a key file: nil
		// keeps sshconn's default identities, an empty slice would disable
		// them entirely.
		if strings.TrimSpace(identityFile) != "" {
			authentication.IdentityFiles = []string{strings.TrimSpace(identityFile)}
		}
		return authentication, nil
	default:
		return sshconn.Authentication{}, fmt.Errorf("serverconn: unknown auth method %q", method)
	}
}

// BuildSSHConfig assembles the sshconn.Config for one asset, the way
// workbench's sshConfigFor does for a saved host: target endpoint, per-endpoint
// authentication overrides, and the jump chain.
//
//   - Direct connections have an empty ProxyJumps. With a jump host,
//     ProxyJumps holds exactly that one hop (first-stage scope is a single
//     jump) and Authentication carries an entry for the jump endpoint too,
//     so each hop authenticates under its own asset's method.
//   - KnownHostsPath is required: sshconn refuses to dial without it.
//   - The returned Config holds no credential value. Passwords and key
//     passphrases are supplied later, at the prompt, through a
//     CredentialProvider (see TestConnection); a later terminal open reuses
//     this same Config and answers its own prompts the same way.
//   - NoProbes is set: a connection built for testing (and later for
//     collection) must not open the optional command sessions sshconn would
//     otherwise use to detect the remote system. No command runs on the
//     server for a connection test.
//   - creds is accepted for signature compatibility with the test flow and
//     intentionally unused here: nothing secret belongs in a Config.
func BuildSSHConfig(server serverassets.Server, jump *serverassets.JumpHost, creds CredentialProvider, knownHostsPath string) (sshconn.Config, error) {
	if strings.TrimSpace(knownHostsPath) == "" {
		return sshconn.Config{}, errors.New("serverconn: known-hosts path is required")
	}
	address := strings.TrimSpace(server.Address)
	if address == "" {
		address = strings.TrimSpace(server.Hostname)
	}
	target, err := endpoint(address, server.Port, server.User)
	if err != nil {
		return sshconn.Config{}, fmt.Errorf("serverconn: server %s: %w", server.ID, err)
	}
	targetAuth, err := authenticationFor(server.AuthMethod, server.IdentityFile)
	if err != nil {
		return sshconn.Config{}, fmt.Errorf("serverconn: server %s: %w", server.ID, err)
	}
	config := sshconn.Config{
		Host:           target.Host,
		Port:           target.Port,
		User:           target.User,
		KnownHostsPath: knownHostsPath,
		Agent:          false,
		DialTimeout:    DialTimeout,
		NoProbes:       true,
		Authentication: map[sshconn.Endpoint]sshconn.Authentication{target: targetAuth},
	}
	if jump != nil {
		jumpEndpoint, err := endpoint(jump.Address, jump.Port, jump.User)
		if err != nil {
			return sshconn.Config{}, fmt.Errorf("serverconn: jump host %s: %w", jump.ID, err)
		}
		jumpAuth, err := authenticationFor(jump.AuthMethod, jump.IdentityFile)
		if err != nil {
			return sshconn.Config{}, fmt.Errorf("serverconn: jump host %s: %w", jump.ID, err)
		}
		config.ProxyJumps = []sshconn.Endpoint{jumpEndpoint}
		config.Authentication[jumpEndpoint] = jumpAuth
	}
	return config, nil
}

// --- failure classification ---

// classifyFailure maps a terminal sshconn Status to a FailureKind. It is a
// pure function of the status (plus the caller's ctx error) so the mapping
// is unit-testable with synthetic inputs. Precedence, most specific first:
//
//  1. caller context ended -> Timeout (the caller owns the overall budget);
//  2. host key changed -> FingerprintChanged (also the prompt-time path in
//     TestConnection, which is authoritative; this covers a future sshconn
//     that reports the change as an error instead of a prompt);
//  3. any failure on a jump-host hop -> JumpHostFailed: the hop, not the
//     failure mode, is what the operator has to fix;
//  4. authentication failures (ErrTooManyAuthAttempts is checked
//     separately: only sshconn's wrapped handshake error Is-matches
//     ErrAuthFailed; the bare sentinel does not) -> AuthFailed;
//  5. anything else during the handshake -> SSHUnavailable.
//
// Note ErrHostKeyRejected is mapped to FingerprintUnknown: it only arises
// when this package itself rejected a key at a prompt, and the prompt-time
// path already reported the precise kind; reaching here means the precise
// kind was lost, and "unknown" is the safe side of that ambiguity.
func classifyFailure(status sshconn.Status, ctxErr error) FailureKind {
	switch {
	case ctxErr != nil:
		return KindTimeout
	case errors.Is(status.Err, sshconn.ErrHostKeyChanged):
		return KindFingerprintChanged
	case status.EndpointRole == sshconn.EndpointProxyJump:
		return KindJumpHostFailed
	case errors.Is(status.Err, sshconn.ErrAuthFailed) || errors.Is(status.Err, sshconn.ErrTooManyAuthAttempts):
		return KindAuthFailed
	case errors.Is(status.Err, sshconn.ErrCanceled):
		// This package cancels only when a credential is missing or a
		// prompt cannot be answered non-interactively; both are
		// authentication failures.
		return KindAuthFailed
	case errors.Is(status.Err, sshconn.ErrHostKeyRejected):
		return KindFingerprintUnknown
	case status.Err != nil:
		return KindSSHUnavailable
	default:
		return KindNone
	}
}

// --- connection test ---

// dialConn is the shared non-interactive dial behind TestConnection and
// Dial. It tests one server in three stages:
//
//  1. TCP: dial the entry address (the jump host when one is configured —
//     the client never dials the target directly then — otherwise the
//     target) with a 5s timeout. Failure is NetworkUnreachable, or
//     JumpHostFailed when the unreachable entry point is the jump host.
//  2. SSH handshake: sshconn.Dial with the Config from BuildSSHConfig. A
//     handshake stuck in PhaseConnecting for 10s without reaching
//     authentication is SSHUnavailable.
//  3. Authentication: password and key-passphrase prompts are answered from
//     creds, keyed by the asset UUID of the hop being authenticated
//     (Status.EndpointRole tells which hop). A host-key prompt is answered
//     with trust only when the presented fingerprint equals the fingerprint
//     already stored on that hop's asset (the user-confirmed key); the trust
//     is then persisted to known_hosts. An unknown fingerprint ends the test
//     as FingerprintUnknown, a changed one as FingerprintChanged, both via
//     an immediate RejectHostKey, and nothing is written to known_hosts.
//
// The dial opens no command session and runs no command on the server
// (BuildSSHConfig sets NoProbes). The overall deadline is the caller's ctx;
// on ctx end the result is Timeout. On success the live Conn is returned
// to the caller, which must Close it; on every other path the Conn is
// closed here, so no goroutine or socket leaks.
func dialConn(ctx context.Context, server serverassets.Server, jump *serverassets.JumpHost, creds CredentialProvider, knownHostsPath string) (*sshconn.Conn, TestResult) {
	start := time.Now()
	result := TestResult{ServerID: server.ID, Stage: StageTCP}
	// conn is owned by the caller only on success (keep); every other
	// return path closes it here.
	var conn *sshconn.Conn
	keep := false
	defer func() {
		if !keep && conn != nil {
			conn.Close()
		}
	}()
	finish := func() (*sshconn.Conn, TestResult) {
		result.Duration = time.Since(start)
		return conn, result
	}

	// Stage 1: TCP reachability of the entry address.
	entryHost := strings.TrimSpace(server.Address)
	if entryHost == "" {
		entryHost = strings.TrimSpace(server.Hostname)
	}
	entryPort := server.Port
	if jump != nil {
		entryHost = strings.TrimSpace(jump.Address)
		entryPort = jump.Port
	}
	if entryPort == 0 {
		entryPort = 22
	}
	entryAddr := net.JoinHostPort(entryHost, strconv.Itoa(entryPort))
	dialer := net.Dialer{Timeout: TCPTimeout}
	tcpConn, err := dialer.DialContext(ctx, "tcp", entryAddr)
	if err != nil {
		result.Error = err
		switch {
		case ctx.Err() != nil:
			result.Kind = KindTimeout
		case jump != nil:
			result.Kind = KindJumpHostFailed
			result.JumpHost = true
		default:
			result.Kind = KindNetworkUnreachable
		}
		return finish()
	}
	tcpConn.Close()

	// Stages 2+3: SSH handshake and authentication via sshconn.
	config, err := BuildSSHConfig(server, jump, creds, knownHostsPath)
	if err != nil {
		result.Stage = StageSSH
		result.Kind = KindInvalidConfig
		result.Error = err
		return finish()
	}
	conn = sshconn.Dial(config)
	result.Stage = StageSSH
	dialStart := time.Now()
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			conn.Cancel()
			result.Kind = KindTimeout
			result.Error = ctx.Err()
			return finish()
		case <-ticker.C:
			status := conn.Status()
			switch status.Phase {
			case sshconn.PhaseConnected:
				result.Stage = StageAuth
				result.OK = true
				result.Kind = KindNone
				result.ServerVersion = status.ServerVersion
				keep = true
				return finish()
			case sshconn.PhaseFailed:
				result.Kind = classifyFailure(status, ctx.Err())
				result.JumpHost = status.EndpointRole == sshconn.EndpointProxyJump
				if status.Err != nil {
					result.Error = status.Err
				}
				return finish()
			case sshconn.PhaseHostKeyPrompt:
				// A fingerprint the user already confirmed and stored on
				// this hop's asset is trusted (and persisted to
				// known_hosts, so later connections verify without
				// prompting). Anything else stops the test: unknown and
				// changed keys are rejected, never trusted. sshconn
				// verifies each hop independently, so EndpointRole says
				// which hop presented this key.
				result.Stage = StageSSH
				result.Fingerprint = status.HostKey.Fingerprint
				result.FingerprintType = status.HostKey.Type
				result.FingerprintChanged = status.HostKey.Changed
				result.KnownFingerprints = append([]string(nil), status.HostKey.Known...)
				result.JumpHost = status.EndpointRole == sshconn.EndpointProxyJump
				stored := server.HostKeyFingerprint
				if result.JumpHost && jump != nil {
					stored = jump.HostKeyFingerprint
				}
				if stored != "" && status.HostKey.Fingerprint == stored {
					conn.TrustHostKey(true)
					continue
				}
				if status.HostKey.Changed {
					result.Kind = KindFingerprintChanged
				} else {
					result.Kind = KindFingerprintUnknown
				}
				conn.RejectHostKey()
				return finish()
			case sshconn.PhasePasswordPrompt:
				result.Stage = StageAuth
				if answerPasswordPrompt(conn, status, server, jump, creds, &result) {
					return finish()
				}
			case sshconn.PhaseKeyPassphrasePrompt:
				result.Stage = StageAuth
				answerKeyPassphrasePrompt(conn, status, server, jump, creds)
			case sshconn.PhaseAuthenticating:
				result.Stage = StageAuth
			case sshconn.PhaseConnecting:
				if time.Since(dialStart) > handshakeStallTimeout {
					conn.Cancel()
					result.Kind = KindSSHUnavailable
					result.Error = fmt.Errorf("serverconn: SSH handshake stalled for %s", handshakeStallTimeout)
					return finish()
				}
			}
		}
	}
}

// Dial opens one SSH connection to a server non-interactively and returns
// it on success. Prompt handling is identical to TestConnection's: the
// stored fingerprint is trusted (and persisted to known_hosts), unknown or
// changed fingerprints are rejected, and password/key-passphrase prompts are
// answered from creds. The caller owns the returned Conn and must Close it
// when done. On failure the returned Conn is nil and result explains why
// (Kind/Stage/Error/Fingerprint, as in TestResult). This is the dial the
// collection scheduler uses to keep one connection per server across
// rounds; it opens no command session itself.
func Dial(ctx context.Context, server serverassets.Server, jump *serverassets.JumpHost, creds CredentialProvider, knownHostsPath string) (*sshconn.Conn, TestResult) {
	conn, result := dialConn(ctx, server, jump, creds, knownHostsPath)
	if !result.OK {
		return nil, result
	}
	return conn, result
}

// TestConnection tests one server non-interactively in the three stages
// described on dialConn and reports the outcome. Any connection opened
// along the way is closed before returning.
func TestConnection(ctx context.Context, server serverassets.Server, jump *serverassets.JumpHost, creds CredentialProvider, knownHostsPath string) TestResult {
	conn, result := dialConn(ctx, server, jump, creds, knownHostsPath)
	if conn != nil {
		conn.Close()
	}
	return result
}

// credentialOwnerID resolves which asset a prompt belongs to: the jump host
// when the prompt's hop is a jump host, otherwise the target server.
func credentialOwnerID(status sshconn.Status, server serverassets.Server, jump *serverassets.JumpHost) string {
	if status.EndpointRole == sshconn.EndpointProxyJump && jump != nil {
		return jump.ID
	}
	return server.ID
}

// isPasswordPrompt mirrors workbench's isPasswordQuestion: exactly one
// secret question, worded as a password prompt (or unworded, the plain
// password method). Anything else — a second question, a one-time code, a
// PIN — cannot be answered non-interactively with the stored password.
func isPasswordPrompt(questions []sshconn.Question) bool {
	if len(questions) != 1 || !questions[0].Secret {
		return false
	}
	wording := strings.ToLower(strings.TrimSpace(questions[0].Text))
	if wording == "" {
		return true
	}
	for _, word := range []string{"password", "passwd", "passwort", "mot de passe", "contraseña", "senha", "пароль", "密码"} {
		if strings.Contains(wording, word) {
			return true
		}
	}
	return false
}

// answerPasswordPrompt answers a password prompt from the vault-backed
// provider. It returns true with result filled when the prompt cannot be
// answered (no password stored, provider error, or a question that is not
// a password) — the caller then ends the test as AuthFailed. Otherwise it
// submits the password and returns false to keep polling; sshconn caps
// refused passwords at three attempts and then fails with ErrAuthFailed.
// The password value is never stored, logged, or put in an error.
func answerPasswordPrompt(conn *sshconn.Conn, status sshconn.Status, server serverassets.Server, jump *serverassets.JumpHost, creds CredentialProvider, result *TestResult) bool {
	fail := func(err error) bool {
		result.Kind = KindAuthFailed
		result.Error = err
		conn.Cancel()
		return true
	}
	if !isPasswordPrompt(status.Questions) {
		return fail(errors.New("serverconn: server asks for more than a password"))
	}
	if creds == nil {
		return fail(errors.New("serverconn: no credential provider"))
	}
	password, found, err := creds.Password(credentialOwnerID(status, server, jump))
	if err != nil {
		return fail(fmt.Errorf("serverconn: credential lookup failed: %w", err))
	}
	if !found {
		return fail(errors.New("serverconn: no password stored for this server"))
	}
	conn.SubmitPassword(password)
	return false
}

// answerKeyPassphrasePrompt answers an encrypted-key prompt from the
// provider. A missing passphrase (or provider error, or no provider) skips
// the identity, as workbench's agent path does: the remaining methods are
// still tried, and with RequirePublicKey set the connection then fails as
// AuthFailed on its own. It never ends the test directly.
func answerKeyPassphrasePrompt(conn *sshconn.Conn, status sshconn.Status, server serverassets.Server, jump *serverassets.JumpHost, creds CredentialProvider) {
	if creds == nil {
		conn.SkipIdentity()
		return
	}
	passphrase, found, err := creds.KeyPassphrase(credentialOwnerID(status, server, jump))
	if err != nil || !found {
		conn.SkipIdentity()
		return
	}
	conn.SubmitKeyPassphrase(passphrase)
}

// TestConnections tests many servers with bounded concurrency. Results are
// returned in input order; one server's failure never affects the others.
// concurrency <= 0 defaults to 10. A server whose JumpHostID names a jump
// host missing from jumps is reported as KindInvalidConfig without dialing.
func TestConnections(ctx context.Context, servers []serverassets.Server, jumps map[string]*serverassets.JumpHost, creds CredentialProvider, knownHostsPath string, concurrency int) []TestResult {
	if concurrency <= 0 {
		concurrency = 10
	}
	results := make([]TestResult, len(servers))
	var wg sync.WaitGroup
	sem := make(chan struct{}, concurrency)
	for index, server := range servers {
		wg.Add(1)
		go func(index int, server serverassets.Server) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if server.JumpHostID != "" {
				jump, ok := jumps[server.JumpHostID]
				if !ok {
					results[index] = TestResult{
						ServerID: server.ID,
						Stage:    StageTCP,
						Kind:     KindInvalidConfig,
						Error:    fmt.Errorf("serverconn: jump host %s not found", server.JumpHostID),
					}
					return
				}
				results[index] = TestConnection(ctx, server, jump, creds, knownHostsPath)
				return
			}
			results[index] = TestConnection(ctx, server, nil, creds, knownHostsPath)
		}(index, server)
	}
	wg.Wait()
	return results
}
