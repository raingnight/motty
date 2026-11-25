package serverbatch

// Test-only SSH server with real exec sessions, adapted from
// internal/serverconn/testserver_test.go (that server refuses session
// channels, which the executor needs). Production code never dials SSH
// itself; tests reach it through serverconn.Dial, the same path the
// production DialFunc adapter uses.

import (
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

type testSSHServer struct {
	t        *testing.T
	listener net.Listener
	hostKey  ssh.Signer
	password string

	mu sync.Mutex
	wg sync.WaitGroup

	// active counts exec sessions currently running; maxActive is the
	// highest value active ever reached (for the concurrency test).
	active    atomic.Int32
	maxActive atomic.Int32
}

func startTestSSHServer(t *testing.T, password string) *testSSHServer {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	server := &testSSHServer{t: t, hostKey: signer, password: password}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server.listener = listener
	config := &ssh.ServerConfig{PasswordCallback: func(_ ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
		if string(pass) == password {
			return nil, nil
		}
		return nil, os.ErrPermission
	}}
	config.AddHostKey(signer)
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
					if newChannel.ChannelType() != "session" {
						newChannel.Reject(ssh.Prohibited, "only session channels in tests")
						continue
					}
					channel, requests, err := newChannel.Accept()
					if err != nil {
						continue
					}
					go server.handleSession(channel, requests)
				}
				serverConn.Wait()
			}()
		}
	}()
	t.Cleanup(server.stop)
	return server
}

func (server *testSSHServer) handleSession(channel ssh.Channel, requests <-chan *ssh.Request) {
	defer channel.Close()
	for req := range requests {
		if req.Type != "exec" {
			// pty-req, env, window-change and friends: not needed
			// for Exec; answer false without failing the session.
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
			continue
		}
		var payload struct{ Command string }
		if err := ssh.Unmarshal(req.Payload, &payload); err != nil {
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
			return
		}
		if req.WantReply {
			_ = req.Reply(true, nil)
		}
		server.runExec(channel, payload.Command)
		return
	}
}

func (server *testSSHServer) runExec(channel ssh.Channel, command string) {
	active := server.active.Add(1)
	defer server.active.Add(-1)
	for {
		if max := server.maxActive.Load(); active > max {
			if server.maxActive.CompareAndSwap(max, active) {
				break
			}
			continue
		}
		break
	}
	cmd := exec.Command("sh", "-c", command)
	cmd.Stdout = channel
	cmd.Stderr = channel.Stderr()
	// If the client closes the channel mid-command, Wait blocks until
	// the command exits on its own; that only strands this test
	// goroutine (it is not tracked by server.wg), never the test.
	err := cmd.Run()
	code := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			code = exitErr.ExitCode()
		} else {
			code = 1
		}
	}
	_, _ = channel.SendRequest("exit-status", false,
		ssh.Marshal(struct{ ExitStatus uint32 }{ExitStatus: uint32(code)}))
}

func (server *testSSHServer) stop() {
	server.listener.Close()
	server.wg.Wait()
}

func (server *testSSHServer) host() (string, int) {
	address := server.listener.Addr().(*net.TCPAddr)
	return "127.0.0.1", address.Port
}

func (server *testSSHServer) addr() string {
	host, port := server.host()
	return net.JoinHostPort(host, strconv.Itoa(port))
}

func (server *testSSHServer) fingerprint() string {
	return ssh.FingerprintSHA256(server.hostKey.PublicKey())
}

// trust writes a known_hosts line trusting this server's host key.
func (server *testSSHServer) trust(t *testing.T, knownHostsPath string) {
	t.Helper()
	line := knownhosts.Line([]string{knownhosts.Normalize(server.addr())}, server.hostKey.PublicKey()) + "\n"
	if err := os.MkdirAll(filepath.Dir(knownHostsPath), 0o700); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(knownHostsPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
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

// mapCreds is a serverconn.CredentialProvider backed by in-memory maps.
// It is safe for concurrent use: the executor dials servers in parallel.
type mapCreds struct {
	mu          sync.Mutex
	passwords   map[string]string
	passphrases map[string]string
}

func (m *mapCreds) Password(id string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	password, ok := m.passwords[id]
	return password, ok, nil
}

func (m *mapCreds) KeyPassphrase(id string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	passphrase, ok := m.passphrases[id]
	return passphrase, ok, nil
}

func (m *mapCreds) setPassword(id, password string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.passwords[id] = password
}
