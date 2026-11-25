package serverconn

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"motty/internal/serverassets"
	"motty/internal/sshconn"
)

func passwordServer(id, host string, port int) serverassets.Server {
	return serverassets.Server{
		ID:         id,
		Name:       "test-" + id,
		Address:    host,
		Port:       port,
		User:       "tester",
		AuthMethod: serverassets.AuthPassword,
	}
}

// --- failure classification with synthetic inputs ---

func TestClassifyFailure(t *testing.T) {
	cases := []struct {
		name   string
		status sshconn.Status
		ctxErr error
		want   FailureKind
	}{
		{"auth failed", sshconn.Status{Err: sshconn.ErrAuthFailed}, nil, KindAuthFailed},
		{"too many attempts is auth failed", sshconn.Status{Err: sshconn.ErrTooManyAuthAttempts}, nil, KindAuthFailed},
		{"host key changed", sshconn.Status{Err: sshconn.ErrHostKeyChanged}, nil, KindFingerprintChanged},
		{"jump hop failure", sshconn.Status{EndpointRole: sshconn.EndpointProxyJump, Err: errors.New("boom")}, nil, KindJumpHostFailed},
		{"jump hop auth failure is a jump failure", sshconn.Status{EndpointRole: sshconn.EndpointProxyJump, Err: sshconn.ErrAuthFailed}, nil, KindJumpHostFailed},
		{"target handshake failure", sshconn.Status{Err: errors.New("boom")}, nil, KindSSHUnavailable},
		{"context timeout wins", sshconn.Status{Err: sshconn.ErrAuthFailed}, context.DeadlineExceeded, KindTimeout},
		{"canceled by us means auth failed", sshconn.Status{Err: sshconn.ErrCanceled}, nil, KindAuthFailed},
		{"rejected key means unknown", sshconn.Status{Err: sshconn.ErrHostKeyRejected}, nil, KindFingerprintUnknown},
		{"no error", sshconn.Status{}, nil, KindNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyFailure(tc.status, tc.ctxErr); got != tc.want {
				t.Errorf("classifyFailure() = %q, want %q", got, tc.want)
			}
		})
	}
}

// --- BuildSSHConfig ---

func TestBuildSSHConfig(t *testing.T) {
	knownHosts := filepath.Join(t.TempDir(), "known_hosts")

	t.Run("direct password narrows auth", func(t *testing.T) {
		server := passwordServer("srv-1", "10.0.0.5", 2222)
		config, err := BuildSSHConfig(server, nil, nil, knownHosts)
		if err != nil {
			t.Fatal(err)
		}
		if len(config.ProxyJumps) != 0 {
			t.Errorf("ProxyJumps = %v, want empty (direct)", config.ProxyJumps)
		}
		if config.Host != "10.0.0.5" || config.Port != 2222 || config.User != "tester" {
			t.Errorf("endpoint = %s:%d@%s, want 10.0.0.5:2222@tester", config.Host, config.Port, config.User)
		}
		if config.KnownHostsPath != knownHosts || config.DialTimeout != DialTimeout || !config.NoProbes || config.Agent {
			t.Errorf("config = %+v, want KnownHostsPath set, DialTimeout %s, NoProbes, no agent", config, DialTimeout)
		}
		target := sshconn.Endpoint{Host: "10.0.0.5", Port: 2222, User: "tester"}
		auth := config.Authentication[target]
		want := sshconn.AuthKeyboardInteractive | sshconn.AuthPassword
		if auth.Methods != want {
			t.Errorf("target Methods = %v, want %v", auth.Methods, want)
		}
		if auth.IdentityFiles == nil || len(auth.IdentityFiles) != 0 {
			t.Errorf("target IdentityFiles = %#v, want empty non-nil slice (defaults disabled)", auth.IdentityFiles)
		}
		if auth.RequirePublicKey {
			t.Errorf("target RequirePublicKey = true, want false for password auth")
		}
	})

	t.Run("port defaults to 22", func(t *testing.T) {
		config, err := BuildSSHConfig(passwordServer("srv-1", "10.0.0.5", 0), nil, nil, knownHosts)
		if err != nil {
			t.Fatal(err)
		}
		if config.Port != 22 {
			t.Errorf("Port = %d, want 22", config.Port)
		}
	})

	t.Run("key auth sets identity file only when named", func(t *testing.T) {
		server := passwordServer("srv-1", "10.0.0.5", 22)
		server.AuthMethod = serverassets.AuthKey
		server.IdentityFile = "/home/tester/.ssh/id_ed25519"
		config, err := BuildSSHConfig(server, nil, nil, knownHosts)
		if err != nil {
			t.Fatal(err)
		}
		auth := config.Authentication[sshconn.Endpoint{Host: "10.0.0.5", Port: 22, User: "tester"}]
		if !auth.RequirePublicKey || auth.Methods&sshconn.AuthPassword != 0 {
			t.Errorf("key auth = %+v, want RequirePublicKey and no password method", auth)
		}
		if len(auth.IdentityFiles) != 1 || auth.IdentityFiles[0] != "/home/tester/.ssh/id_ed25519" {
			t.Errorf("IdentityFiles = %v, want the named key file", auth.IdentityFiles)
		}

		server.IdentityFile = ""
		config, err = BuildSSHConfig(server, nil, nil, knownHosts)
		if err != nil {
			t.Fatal(err)
		}
		auth = config.Authentication[sshconn.Endpoint{Host: "10.0.0.5", Port: 22, User: "tester"}]
		if auth.IdentityFiles != nil {
			t.Errorf("IdentityFiles = %v, want nil (defaults) when no key file is named", auth.IdentityFiles)
		}
	})

	t.Run("jump host gets its own endpoint and auth", func(t *testing.T) {
		server := passwordServer("srv-1", "10.0.0.5", 22)
		jump := &serverassets.JumpHost{
			ID: "jump-1", Address: "10.0.0.1", Port: 22, User: "jumpuser",
			AuthMethod: serverassets.AuthPassword,
		}
		config, err := BuildSSHConfig(server, jump, nil, knownHosts)
		if err != nil {
			t.Fatal(err)
		}
		if len(config.ProxyJumps) != 1 || config.ProxyJumps[0] != (sshconn.Endpoint{Host: "10.0.0.1", Port: 22, User: "jumpuser"}) {
			t.Errorf("ProxyJumps = %v, want the single jump endpoint", config.ProxyJumps)
		}
		jumpEndpoint := sshconn.Endpoint{Host: "10.0.0.1", Port: 22, User: "jumpuser"}
		jumpAuth, ok := config.Authentication[jumpEndpoint]
		if !ok {
			t.Fatalf("no Authentication entry for the jump endpoint")
		}
		if jumpAuth.Methods != sshconn.AuthKeyboardInteractive|sshconn.AuthPassword {
			t.Errorf("jump Methods = %v, want password methods", jumpAuth.Methods)
		}
		if len(config.Authentication) != 2 {
			t.Errorf("Authentication has %d entries, want 2 (target + jump)", len(config.Authentication))
		}
	})

	t.Run("validation errors", func(t *testing.T) {
		server := passwordServer("srv-1", "10.0.0.5", 22)
		if _, err := BuildSSHConfig(server, nil, nil, ""); err == nil {
			t.Errorf("empty known-hosts path: want error")
		}
		badAuth := server
		badAuth.AuthMethod = "token"
		if _, err := BuildSSHConfig(badAuth, nil, nil, knownHosts); err == nil {
			t.Errorf("unknown auth method: want error")
		}
		noUser := server
		noUser.User = ""
		if _, err := BuildSSHConfig(noUser, nil, nil, knownHosts); err == nil {
			t.Errorf("empty user: want error")
		}
		noHost := server
		noHost.Address, noHost.Hostname = "", ""
		if _, err := BuildSSHConfig(noHost, nil, nil, knownHosts); err == nil {
			t.Errorf("empty address: want error")
		}
		badJump := &serverassets.JumpHost{ID: "j", Address: "10.0.0.1", Port: 22, User: "u", AuthMethod: "token"}
		if _, err := BuildSSHConfig(server, badJump, nil, knownHosts); err == nil {
			t.Errorf("jump with unknown auth method: want error")
		}
	})
}

// --- TCP stage ---

func TestConnectionTCPUnreachable(t *testing.T) {
	// A port on loopback that was just closed: connecting is refused at
	// once, deterministically, with no network involved. (192.0.2.1
	// TEST-NET-1 was tried first, but this sandbox's network accepts the
	// TCP connect through a middlebox and resets during the handshake, so
	// it does not behave as unreachable here.)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	server := passwordServer("srv-1", "127.0.0.1", port)
	result := TestConnection(context.Background(), server, nil, nil, filepath.Join(t.TempDir(), "known_hosts"))
	if result.OK || result.Kind != KindNetworkUnreachable || result.Stage != StageTCP {
		t.Errorf("result = %+v, want not-OK NetworkUnreachable at TCP stage", result)
	}
	if result.Error == nil {
		t.Errorf("result.Error is nil, want the dial error")
	}
	if result.Duration <= 0 || result.Duration > 30*time.Second {
		t.Errorf("Duration = %s, want a positive bounded duration", result.Duration)
	}
}

func TestConnectionUnreachableJumpHost(t *testing.T) {
	server := passwordServer("srv-1", "10.0.0.5", 22)
	jump := &serverassets.JumpHost{ID: "jump-1", Address: "192.0.2.1", Port: 22, User: "jumpuser", AuthMethod: serverassets.AuthPassword}
	result := TestConnection(context.Background(), server, jump, nil, filepath.Join(t.TempDir(), "known_hosts"))
	if result.OK || result.Kind != KindJumpHostFailed || !result.JumpHost {
		t.Errorf("result = %+v, want not-OK JumpHostFailed", result)
	}
}

// --- timeout behaviour ---

func startBlackhole(t *testing.T) (host string, port int) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			socket, err := listener.Accept()
			if err != nil {
				return
			}
			// Accept and never speak: the SSH handshake stalls.
			go func() {
				defer socket.Close()
				time.Sleep(30 * time.Second)
			}()
		}
	}()
	return "127.0.0.1", listener.Addr().(*net.TCPAddr).Port
}

func TestConnectionContextTimeout(t *testing.T) {
	host, port := startBlackhole(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	result := TestConnection(ctx, passwordServer("srv-1", host, port), nil, nil, filepath.Join(t.TempDir(), "known_hosts"))
	if result.OK || result.Kind != KindTimeout {
		t.Errorf("result = %+v, want not-OK Timeout", result)
	}
}

func TestConnectionHandshakeStall(t *testing.T) {
	old := handshakeStallTimeout
	handshakeStallTimeout = 300 * time.Millisecond
	defer func() { handshakeStallTimeout = old }()
	host, port := startBlackhole(t)
	start := time.Now()
	result := TestConnection(context.Background(), passwordServer("srv-1", host, port), nil, nil, filepath.Join(t.TempDir(), "known_hosts"))
	if result.OK || result.Kind != KindSSHUnavailable || result.Stage != StageSSH {
		t.Errorf("result = %+v, want not-OK SSHUnavailable at SSH stage", result)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("stalled handshake took %s, want it cut off near the 300ms watchdog", elapsed)
	}
}

// --- real handshakes against the test server ---

func TestConnectionPasswordSuccess(t *testing.T) {
	server := startTestSSHServer(t, testServerOptions{password: "secret"})
	host, port := server.host()
	knownHosts := filepath.Join(t.TempDir(), "known_hosts")
	server.trust(t, knownHosts)
	creds := mapCreds{passwords: map[string]string{"srv-1": "secret"}}
	result := TestConnection(context.Background(), passwordServer("srv-1", host, port), nil, creds, knownHosts)
	if !result.OK || result.Kind != KindNone || result.Stage != StageAuth {
		t.Errorf("result = %+v, want OK at auth stage", result)
	}
	if !strings.HasPrefix(result.ServerVersion, "SSH-2.0-") {
		t.Errorf("ServerVersion = %q, want the server identification string", result.ServerVersion)
	}
	if result.Error != nil {
		t.Errorf("Error = %v, want nil on success", result.Error)
	}
}

func TestConnectionWrongPassword(t *testing.T) {
	server := startTestSSHServer(t, testServerOptions{password: "secret"})
	host, port := server.host()
	knownHosts := filepath.Join(t.TempDir(), "known_hosts")
	server.trust(t, knownHosts)
	creds := mapCreds{passwords: map[string]string{"srv-1": "wrong"}}
	result := TestConnection(context.Background(), passwordServer("srv-1", host, port), nil, creds, knownHosts)
	if result.OK || result.Kind != KindAuthFailed || result.Stage != StageAuth {
		t.Errorf("result = %+v, want not-OK AuthFailed at auth stage", result)
	}
}

func TestConnectionNoPasswordStored(t *testing.T) {
	server := startTestSSHServer(t, testServerOptions{password: "secret"})
	host, port := server.host()
	knownHosts := filepath.Join(t.TempDir(), "known_hosts")
	server.trust(t, knownHosts)
	result := TestConnection(context.Background(), passwordServer("srv-1", host, port), nil, mapCreds{}, knownHosts)
	if result.OK || result.Kind != KindAuthFailed {
		t.Errorf("result = %+v, want not-OK AuthFailed when no password is stored", result)
	}
}

func TestConnectionCredentialProviderError(t *testing.T) {
	server := startTestSSHServer(t, testServerOptions{password: "secret"})
	host, port := server.host()
	knownHosts := filepath.Join(t.TempDir(), "known_hosts")
	server.trust(t, knownHosts)
	creds := mapCreds{err: errors.New("vault locked")}
	result := TestConnection(context.Background(), passwordServer("srv-1", host, port), nil, creds, knownHosts)
	if result.OK || result.Kind != KindAuthFailed {
		t.Errorf("result = %+v, want not-OK AuthFailed on provider error", result)
	}
	if result.Error == nil || !strings.Contains(result.Error.Error(), "vault locked") {
		t.Errorf("Error = %v, want the provider error wrapped", result.Error)
	}
}

func TestConnectionFingerprintUnknown(t *testing.T) {
	server := startTestSSHServer(t, testServerOptions{password: "secret"})
	host, port := server.host()
	knownHosts := filepath.Join(t.TempDir(), "known_hosts")
	creds := mapCreds{passwords: map[string]string{"srv-1": "secret"}}
	result := TestConnection(context.Background(), passwordServer("srv-1", host, port), nil, creds, knownHosts)
	if result.OK || result.Kind != KindFingerprintUnknown {
		t.Errorf("result = %+v, want not-OK FingerprintUnknown", result)
	}
	if result.Fingerprint != server.fingerprint() || result.FingerprintType != "ssh-ed25519" {
		t.Errorf("Fingerprint = %q (%q), want %q (ssh-ed25519)", result.Fingerprint, result.FingerprintType, server.fingerprint())
	}
	if result.FingerprintChanged {
		t.Errorf("FingerprintChanged = true, want false for a first connection")
	}
	// Rejecting must not write anything to known_hosts.
	data, err := os.ReadFile(knownHosts)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 0 {
		t.Errorf("known_hosts = %q, want empty: an unknown key must never be trusted by a test", data)
	}
}

func TestConnectionStoredFingerprintTrusted(t *testing.T) {
	server := startTestSSHServer(t, testServerOptions{password: "secret"})
	host, port := server.host()
	knownHosts := filepath.Join(t.TempDir(), "known_hosts")
	creds := mapCreds{passwords: map[string]string{"srv-1": "secret"}}
	asset := passwordServer("srv-1", host, port)
	// The fingerprint the user already confirmed and stored on the asset:
	// the test must succeed and persist the trust to known_hosts.
	asset.HostKeyFingerprint = server.fingerprint()
	result := TestConnection(context.Background(), asset, nil, creds, knownHosts)
	if !result.OK || result.Kind != KindNone {
		t.Errorf("result = %+v, want OK with a stored matching fingerprint", result)
	}
	data, err := os.ReadFile(knownHosts)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		t.Errorf("known_hosts is empty, want the trusted key persisted for later connections")
	}
	// A second test with the same asset must now verify without prompting.
	result = TestConnection(context.Background(), asset, nil, creds, knownHosts)
	if !result.OK || result.Kind != KindNone {
		t.Errorf("second result = %+v, want OK once the key is in known_hosts", result)
	}
}

func TestConnectionStoredFingerprintMismatch(t *testing.T) {
	server := startTestSSHServer(t, testServerOptions{password: "secret"})
	host, port := server.host()
	knownHosts := filepath.Join(t.TempDir(), "known_hosts")
	creds := mapCreds{passwords: map[string]string{"srv-1": "secret"}}
	asset := passwordServer("srv-1", host, port)
	// A stored fingerprint that is not the presented one: refuse, and do
	// not write anything to known_hosts.
	other := newTestSigner(t)
	asset.HostKeyFingerprint = ssh.FingerprintSHA256(other.PublicKey())
	result := TestConnection(context.Background(), asset, nil, creds, knownHosts)
	if result.OK || result.Kind != KindFingerprintUnknown {
		t.Errorf("result = %+v, want not-OK FingerprintUnknown on a stored mismatch", result)
	}
	if result.Fingerprint != server.fingerprint() {
		t.Errorf("Fingerprint = %q, want the presented %q", result.Fingerprint, server.fingerprint())
	}
	data, err := os.ReadFile(knownHosts)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 0 {
		t.Errorf("known_hosts = %q, want empty: a mismatched key must never be trusted", data)
	}
}

func TestConnectionFingerprintChanged(t *testing.T) {
	server := startTestSSHServer(t, testServerOptions{password: "secret"})
	host, port := server.host()
	knownHosts := filepath.Join(t.TempDir(), "known_hosts")
	// Trust a different key for the same address first.
	other := newTestSigner(t)
	writeKnownHosts(t, knownHosts, server.addr(), other.PublicKey())
	before, err := os.ReadFile(knownHosts)
	if err != nil {
		t.Fatal(err)
	}
	creds := mapCreds{passwords: map[string]string{"srv-1": "secret"}}
	result := TestConnection(context.Background(), passwordServer("srv-1", host, port), nil, creds, knownHosts)
	if result.OK || result.Kind != KindFingerprintChanged {
		t.Errorf("result = %+v, want not-OK FingerprintChanged", result)
	}
	if result.Fingerprint != server.fingerprint() || !result.FingerprintChanged {
		t.Errorf("Fingerprint = %q changed=%v, want %q changed=true", result.Fingerprint, result.FingerprintChanged, server.fingerprint())
	}
	if len(result.KnownFingerprints) != 1 || result.KnownFingerprints[0] != ssh.FingerprintSHA256(other.PublicKey()) {
		t.Errorf("KnownFingerprints = %v, want [%q]", result.KnownFingerprints, ssh.FingerprintSHA256(other.PublicKey()))
	}
	// The changed key must not replace the trusted one.
	after, err := os.ReadFile(knownHosts)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Errorf("known_hosts changed after a rejected key: %q -> %q", before, after)
	}
}

func TestConnectionKeyAuth(t *testing.T) {
	keyPath, public := writeClientKey(t, "")
	target := startTestSSHServer(t, testServerOptions{authorized: []ssh.PublicKey{public}})
	host, port := target.host()
	knownHosts := filepath.Join(t.TempDir(), "known_hosts")
	target.trust(t, knownHosts)
	server := passwordServer("srv-1", host, port)
	server.AuthMethod = serverassets.AuthKey
	server.IdentityFile = keyPath
	result := TestConnection(context.Background(), server, nil, mapCreds{}, knownHosts)
	if !result.OK {
		t.Errorf("result = %+v, want OK for key auth", result)
	}
}

func TestConnectionEncryptedKeyAuth(t *testing.T) {
	keyPath, public := writeClientKey(t, "key-pass")
	target := startTestSSHServer(t, testServerOptions{authorized: []ssh.PublicKey{public}})
	host, port := target.host()
	knownHosts := filepath.Join(t.TempDir(), "known_hosts")
	target.trust(t, knownHosts)
	server := passwordServer("srv-1", host, port)
	server.AuthMethod = serverassets.AuthKey
	server.IdentityFile = keyPath

	t.Run("passphrase supplied", func(t *testing.T) {
		creds := mapCreds{passphrases: map[string]string{"srv-1": "key-pass"}}
		result := TestConnection(context.Background(), server, nil, creds, knownHosts)
		if !result.OK {
			t.Errorf("result = %+v, want OK for encrypted key with passphrase", result)
		}
	})
	t.Run("passphrase missing", func(t *testing.T) {
		result := TestConnection(context.Background(), server, nil, mapCreds{}, knownHosts)
		if result.OK || result.Kind != KindAuthFailed {
			t.Errorf("result = %+v, want not-OK AuthFailed when the passphrase is missing", result)
		}
	})
}

// --- jump host ---

func TestConnectionViaJumpHost(t *testing.T) {
	target := startTestSSHServer(t, testServerOptions{password: "target-secret"})
	targetHost, targetPort := target.host()
	jump := startTestSSHServer(t, testServerOptions{password: "jump-secret", directTarget: target.addr()})
	jumpHost, jumpPort := jump.host()
	knownHosts := filepath.Join(t.TempDir(), "known_hosts")
	target.trust(t, knownHosts)
	jump.trust(t, knownHosts)
	creds := mapCreds{passwords: map[string]string{"srv-1": "target-secret", "jump-1": "jump-secret"}}
	server := passwordServer("srv-1", targetHost, targetPort)
	server.JumpHostID = "jump-1"
	jumpAsset := &serverassets.JumpHost{
		ID: "jump-1", Address: jumpHost, Port: jumpPort, User: "tester",
		AuthMethod: serverassets.AuthPassword,
	}
	result := TestConnection(context.Background(), server, jumpAsset, creds, knownHosts)
	if !result.OK {
		t.Errorf("result = %+v, want OK via jump host", result)
	}
}

func TestConnectionJumpFingerprintUnknown(t *testing.T) {
	target := startTestSSHServer(t, testServerOptions{password: "target-secret"})
	_, targetPort := target.host()
	jump := startTestSSHServer(t, testServerOptions{password: "jump-secret", directTarget: target.addr()})
	jumpHost, jumpPort := jump.host()
	knownHosts := filepath.Join(t.TempDir(), "known_hosts")
	creds := mapCreds{passwords: map[string]string{"srv-1": "target-secret", "jump-1": "jump-secret"}}
	server := passwordServer("srv-1", "127.0.0.1", targetPort)
	server.JumpHostID = "jump-1"
	jumpAsset := &serverassets.JumpHost{
		ID: "jump-1", Address: jumpHost, Port: jumpPort, User: "tester",
		AuthMethod: serverassets.AuthPassword,
	}
	result := TestConnection(context.Background(), server, jumpAsset, creds, knownHosts)
	// The jump hop is verified first, so its unknown key is what stops the test.
	if result.OK || result.Kind != KindFingerprintUnknown || !result.JumpHost {
		t.Errorf("result = %+v, want not-OK FingerprintUnknown on the jump hop", result)
	}
	if result.Fingerprint != jump.fingerprint() {
		t.Errorf("Fingerprint = %q, want the jump host's %q", result.Fingerprint, jump.fingerprint())
	}
}

func TestConnectionJumpAuthFailed(t *testing.T) {
	target := startTestSSHServer(t, testServerOptions{password: "target-secret"})
	_, targetPort := target.host()
	jump := startTestSSHServer(t, testServerOptions{password: "jump-secret", directTarget: target.addr()})
	jumpHost, jumpPort := jump.host()
	knownHosts := filepath.Join(t.TempDir(), "known_hosts")
	target.trust(t, knownHosts)
	jump.trust(t, knownHosts)
	creds := mapCreds{passwords: map[string]string{"srv-1": "target-secret", "jump-1": "wrong"}}
	server := passwordServer("srv-1", "127.0.0.1", targetPort)
	server.JumpHostID = "jump-1"
	jumpAsset := &serverassets.JumpHost{
		ID: "jump-1", Address: jumpHost, Port: jumpPort, User: "tester",
		AuthMethod: serverassets.AuthPassword,
	}
	result := TestConnection(context.Background(), server, jumpAsset, creds, knownHosts)
	if result.OK || result.Kind != KindJumpHostFailed || !result.JumpHost {
		t.Errorf("result = %+v, want not-OK JumpHostFailed on the jump hop", result)
	}
}

// --- batch ---

func TestConnectionsKeepsOrder(t *testing.T) {
	good := startTestSSHServer(t, testServerOptions{password: "secret"})
	host, port := good.host()
	knownHosts := filepath.Join(t.TempDir(), "known_hosts")
	good.trust(t, knownHosts)
	creds := mapCreds{passwords: map[string]string{"good": "secret"}}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedPort := listener.Addr().(*net.TCPAddr).Port
	listener.Close()

	servers := []serverassets.Server{
		passwordServer("good", host, port),
		passwordServer("closed", "127.0.0.1", closedPort),
		passwordServer("dangling", "127.0.0.1", 22),
	}
	servers[2].JumpHostID = "missing-jump"
	results := TestConnections(context.Background(), servers, map[string]*serverassets.JumpHost{}, creds, knownHosts, 2)
	if len(results) != 3 {
		t.Fatalf("got %d results, want 3", len(results))
	}
	if !results[0].OK || results[0].ServerID != "good" {
		t.Errorf("results[0] = %+v, want OK for good", results[0])
	}
	if results[1].OK || results[1].Kind != KindNetworkUnreachable || results[1].ServerID != "closed" {
		t.Errorf("results[1] = %+v, want NetworkUnreachable for closed", results[1])
	}
	if results[2].OK || results[2].Kind != KindInvalidConfig || results[2].ServerID != "dangling" {
		t.Errorf("results[2] = %+v, want InvalidConfig for a missing jump host", results[2])
	}
}

// --- Dial (live connection for the collection scheduler) ---

func TestDialReturnsLiveConnection(t *testing.T) {
	server := startTestSSHServer(t, testServerOptions{password: "secret"})
	host, port := server.host()
	knownHosts := filepath.Join(t.TempDir(), "known_hosts")
	server.trust(t, knownHosts)
	creds := mapCreds{passwords: map[string]string{"srv-1": "secret"}}
	conn, result := Dial(context.Background(), passwordServer("srv-1", host, port), nil, creds, knownHosts)
	if conn == nil {
		t.Fatalf("Dial returned nil conn, result = %+v", result)
	}
	defer conn.Close()
	if !result.OK || result.Kind != KindNone || result.Stage != StageAuth {
		t.Errorf("result = %+v, want OK at auth stage", result)
	}
	if status := conn.Status(); status.Phase != sshconn.PhaseConnected {
		t.Errorf("conn phase = %v, want connected", status.Phase)
	}
}

func TestDialWrongPasswordReturnsNilConn(t *testing.T) {
	server := startTestSSHServer(t, testServerOptions{password: "secret"})
	host, port := server.host()
	knownHosts := filepath.Join(t.TempDir(), "known_hosts")
	server.trust(t, knownHosts)
	creds := mapCreds{passwords: map[string]string{"srv-1": "wrong"}}
	conn, result := Dial(context.Background(), passwordServer("srv-1", host, port), nil, creds, knownHosts)
	if conn != nil {
		conn.Close()
		t.Errorf("Dial returned a live conn on auth failure")
	}
	if result.OK || result.Kind != KindAuthFailed {
		t.Errorf("result = %+v, want not-OK AuthFailed", result)
	}
}
