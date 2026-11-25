package serverconn

// Test-only SSH servers. sshconn's own test server (internal/sshconn) is
// unexported and lives in another package, so serverconn tests run their own
// minimal server here, built directly on golang.org/x/crypto/ssh. This is
// test scaffolding only: production code in this package never dials SSH
// itself and always goes through sshconn.

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// testSSHServer is a real SSH server on loopback: full key exchange and
// authentication, session channels refused, optional direct-tcpip forwarding
// so it can also play a jump host.
type testSSHServer struct {
	t        *testing.T
	listener net.Listener
	hostKey  ssh.Signer
	password string
	// authorized public keys for publickey auth; empty disables it.
	authorized []ssh.PublicKey
	// directTarget, when set, is the one direct-tcpip destination allowed
	// (for example the target a jump host forwards to).
	directTarget string
	version      string

	mu sync.Mutex
	wg sync.WaitGroup
}

type testServerOptions struct {
	password     string
	authorized   []ssh.PublicKey
	directTarget string
	version      string
}

func newTestSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

func startTestSSHServer(t *testing.T, opts testServerOptions) *testSSHServer {
	t.Helper()
	server := &testSSHServer{
		t:            t,
		hostKey:      newTestSigner(t),
		password:     opts.password,
		authorized:   opts.authorized,
		directTarget: opts.directTarget,
		version:      opts.version,
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server.listener = listener
	version := server.version
	if version == "" {
		version = "SSH-2.0-TestServer"
	}
	config := &ssh.ServerConfig{ServerVersion: version}
	config.AddHostKey(server.hostKey)
	if server.password != "" {
		config.PasswordCallback = func(_ ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
			if string(password) == server.password {
				return nil, nil
			}
			return nil, os.ErrPermission
		}
	}
	if len(server.authorized) > 0 {
		config.PublicKeyCallback = func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			for _, allowed := range server.authorized {
				if string(allowed.Marshal()) == string(key.Marshal()) {
					return nil, nil
				}
			}
			return nil, os.ErrPermission
		}
	}
	server.wg.Add(1)
	go func() {
		defer server.wg.Done()
		for {
			socket, err := listener.Accept()
			if err != nil {
				return
			}
			server.wg.Add(1)
			go func() {
				defer server.wg.Done()
				defer socket.Close()
				serverConn, channels, requests, err := ssh.NewServerConn(socket, config)
				if err != nil {
					return
				}
				go ssh.DiscardRequests(requests)
				for newChannel := range channels {
					switch newChannel.ChannelType() {
					case "direct-tcpip":
						server.serveDirect(newChannel)
					default:
						newChannel.Reject(ssh.Prohibited, "no session channels in tests")
					}
				}
				serverConn.Wait()
			}()
		}
	}()
	t.Cleanup(server.stop)
	return server
}

type directTCPIPRequest struct {
	DestinationHost string
	DestinationPort uint32
	OriginHost      string
	OriginPort      uint32
}

func (server *testSSHServer) serveDirect(newChannel ssh.NewChannel) {
	var request directTCPIPRequest
	if err := ssh.Unmarshal(newChannel.ExtraData(), &request); err != nil {
		newChannel.Reject(ssh.ConnectionFailed, err.Error())
		return
	}
	target := net.JoinHostPort(request.DestinationHost, strconv.Itoa(int(request.DestinationPort)))
	if server.directTarget == "" || target != server.directTarget {
		newChannel.Reject(ssh.Prohibited, "direct-tcpip target refused")
		return
	}
	remote, err := net.Dial("tcp", target)
	if err != nil {
		newChannel.Reject(ssh.ConnectionFailed, err.Error())
		return
	}
	channel, channelRequests, err := newChannel.Accept()
	if err != nil {
		remote.Close()
		return
	}
	server.wg.Add(1)
	go func() {
		defer server.wg.Done()
		defer channel.Close()
		defer remote.Close()
		go ssh.DiscardRequests(channelRequests)
		done := make(chan struct{}, 2)
		go func() { _, _ = io.Copy(remote, channel); done <- struct{}{} }()
		go func() { _, _ = io.Copy(channel, remote); done <- struct{}{} }()
		<-done
	}()
}

func (server *testSSHServer) stop() {
	server.listener.Close()
	server.wg.Wait()
}

// host returns the dial address and port.
func (server *testSSHServer) host() (string, int) {
	address := server.listener.Addr().(*net.TCPAddr)
	return "127.0.0.1", address.Port
}

func (server *testSSHServer) addr() string {
	host, port := server.host()
	return net.JoinHostPort(host, strconv.Itoa(port))
}

// fingerprint is the SHA256 fingerprint of the server's host key.
func (server *testSSHServer) fingerprint() string {
	return ssh.FingerprintSHA256(server.hostKey.PublicKey())
}

// trust writes a known_hosts line trusting this server's host key.
func (server *testSSHServer) trust(t *testing.T, knownHostsPath string) {
	t.Helper()
	writeKnownHosts(t, knownHostsPath, server.addr(), server.hostKey.PublicKey())
}

// writeKnownHosts appends one trusted line for addr.
func writeKnownHosts(t *testing.T, path, addr string, key ssh.PublicKey) {
	t.Helper()
	line := knownhosts.Line([]string{knownhosts.Normalize(addr)}, key) + "\n"
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(line); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

// writeClientKey writes an Ed25519 private key file (encrypted when
// passphrase is non-empty) and returns its path and public key.
func writeClientKey(t *testing.T, passphrase string) (path string, public ssh.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var block *pem.Block
	if passphrase != "" {
		block, err = ssh.MarshalPrivateKeyWithPassphrase(priv, "", []byte(passphrase))
	} else {
		block, err = ssh.MarshalPrivateKey(priv, "")
	}
	if err != nil {
		t.Fatal(err)
	}
	path = filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	public, err = ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return path, public
}

// mapCreds is a CredentialProvider backed by in-memory maps.
type mapCreds struct {
	passwords   map[string]string
	passphrases map[string]string
	err         error
}

func (m mapCreds) Password(id string) (string, bool, error) {
	if m.err != nil {
		return "", false, m.err
	}
	password, ok := m.passwords[id]
	return password, ok, nil
}

func (m mapCreds) KeyPassphrase(id string) (string, bool, error) {
	if m.err != nil {
		return "", false, m.err
	}
	passphrase, ok := m.passphrases[id]
	return passphrase, ok, nil
}
