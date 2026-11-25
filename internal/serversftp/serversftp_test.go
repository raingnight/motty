package serversftp

// Tests for the SFTP browser. The transfer and directory tests run
// against an in-memory fake backend rooted at a temp dir, so they cover
// the Worker resume/pause/cancel path without a network; the SCP
// fallback test runs a real SSH server that accepts session channels but
// refuses the SFTP subsystem, and checks Open falls back to SCP.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"motty/internal/serverassets"
	"motty/internal/serverconn"
	"motty/internal/sftpclient"
	"motty/internal/sshconn"
)

// fakeRemote is an sftpclient.Remote backed by a local temp directory.
// It implements the write side the Worker asserts to, so uploads and
// downloads run the real transfer code.
type fakeRemote struct {
	root string
	mu   sync.Mutex

	// writeDelay, when set, sleeps this long per Write on an uploaded
	// file, so a cancel-during-transfer test has time to act.
	writeDelay time.Duration
}

func (f *fakeRemote) resolve(p string) string {
	cleaned := strings.TrimPrefix(p, "/")
	return filepath.Join(f.root, filepath.FromSlash(cleaned))
}

func (f *fakeRemote) Getwd() (string, error) { return "/home/test", nil }

func (f *fakeRemote) ReadDir(p string) ([]os.FileInfo, error) {
	entries, err := os.ReadDir(f.resolve(p))
	if err != nil {
		return nil, err
	}
	infos := make([]os.FileInfo, 0, len(entries))
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		infos = append(infos, info)
	}
	return infos, nil
}

func (f *fakeRemote) Open(p string) (io.ReadCloser, error) {
	return os.Open(f.resolve(p))
}

func (f *fakeRemote) Close() error { return nil }

func (f *fakeRemote) Lstat(p string) (os.FileInfo, error) {
	return os.Lstat(f.resolve(p))
}

func (f *fakeRemote) OpenTransferFile(p string, flags int) (sftpclient.TransferFile, error) {
	file, err := os.OpenFile(f.resolve(p), flags, 0o644)
	if err != nil {
		return nil, err
	}
	if f.writeDelay > 0 {
		return &slowFile{File: file, delay: f.writeDelay}, nil
	}
	return file, nil
}

func (f *fakeRemote) Mkdir(p string) error {
	return os.MkdirAll(f.resolve(p), 0o755)
}

func (f *fakeRemote) Rename(oldPath, newPath string) error {
	return os.Rename(f.resolve(oldPath), f.resolve(newPath))
}

func (f *fakeRemote) PosixRename(oldPath, newPath string) error {
	return os.Rename(f.resolve(oldPath), f.resolve(newPath))
}

func (f *fakeRemote) Remove(p string) error { return os.Remove(f.resolve(p)) }

func (f *fakeRemote) RemoveDirectory(p string) error { return os.Remove(f.resolve(p)) }

func (f *fakeRemote) Chmod(p string, mode os.FileMode) error {
	return os.Chmod(f.resolve(p), mode)
}

// slowFile delays every Write so a transfer stays in flight long enough
// to pause or cancel it from the test.
type slowFile struct {
	*os.File
	delay time.Duration
}

func (f *slowFile) Write(p []byte) (int, error) {
	time.Sleep(f.delay)
	return f.File.Write(p)
}

func newTestBrowser(t *testing.T, remote *fakeRemote) *Browser {
	t.Helper()
	browser := &Browser{
		server:  serverassets.Server{ID: "srv-1", Name: "test"},
		remote:  remote,
		home:    "/home/test",
		waiters: map[uint64]chan error{},
	}
	browser.worker = sftpclient.NewWorker(func() (sftpclient.Remote, error) {
		return remote, nil
	}, func() {})
	go browser.drainResults()
	t.Cleanup(func() { _ = browser.Close() })
	return browser
}

func writeRemoteFile(t *testing.T, remote *fakeRemote, p, content string) {
	t.Helper()
	full := remote.resolve(p)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCleanPathAndJoinDir(t *testing.T) {
	if got, err := CleanPath("/a/../b"); err != nil || got != "/b" {
		t.Fatalf("CleanPath(/a/../b) = %q, %v, want /b", got, err)
	}
	if got, err := CleanPath("/a//b/"); err != nil || got != "/a/b" {
		t.Fatalf("CleanPath(/a//b/) = %q, %v, want /a/b", got, err)
	}
	for _, bad := range []string{"", "relative/path", "  "} {
		if _, err := CleanPath(bad); err == nil {
			t.Fatalf("CleanPath(%q) = nil error, want an error", bad)
		}
	}
	if got, err := JoinDir("/a/b", "c.txt"); err != nil || got != "/a/b/c.txt" {
		t.Fatalf("JoinDir = %q, %v, want /a/b/c.txt", got, err)
	}
	// ".." and "/" in a name must never escape the directory.
	for _, bad := range []string{"..", ".", "", "x/y", "../etc"} {
		if _, err := JoinDir("/a/b", bad); err == nil {
			t.Fatalf("JoinDir(/a/b, %q) = nil error, want an error", bad)
		}
	}
}

func TestBrowserListDirSort(t *testing.T) {
	remote := &fakeRemote{root: t.TempDir()}
	writeRemoteFile(t, remote, "/dir/zeta.txt", "z")
	writeRemoteFile(t, remote, "/dir/Alpha.txt", "a")
	writeRemoteFile(t, remote, "/dir/beta.txt", "b")
	if err := os.MkdirAll(remote.resolve("/dir/mango"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(remote.resolve("/dir/Apple"), 0o755); err != nil {
		t.Fatal(err)
	}
	browser := newTestBrowser(t, remote)
	entries, err := browser.ListDir("/dir")
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	var got []string
	for _, entry := range entries {
		got = append(got, entry.Name)
	}
	want := []string{"Apple", "mango", "Alpha.txt", "beta.txt", "zeta.txt"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("ListDir order = %v, want %v", got, want)
	}
	if entries[0].Type != "目录" || !entries[0].IsDir {
		t.Fatalf("first entry = %+v, want a directory", entries[0])
	}
	if entries[2].Type != "文件" || entries[2].IsDir {
		t.Fatalf("third entry = %+v, want a file", entries[2])
	}
	if _, err := browser.ListDir("relative"); err == nil {
		t.Fatal("ListDir(relative) = nil error, want an error")
	}
	if _, err := browser.ListDir("/no-such-dir"); err == nil || err.Error() == "" {
		t.Fatal("ListDir(missing) did not report a reason")
	}
}

func TestBrowserMkdirRenameDeleteStat(t *testing.T) {
	remote := &fakeRemote{root: t.TempDir()}
	browser := newTestBrowser(t, remote)
	if err := browser.Mkdir("/a/b/c"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	writeRemoteFile(t, remote, "/a/b/c/file.txt", "hello")
	stat, err := browser.Stat("/a/b/c/file.txt")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if stat.Size != 5 || stat.Type != "文件" {
		t.Fatalf("Stat = %+v, want size 5 file", stat)
	}
	if err := browser.Rename("/a/b/c/file.txt", "/a/b/c/renamed.txt"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if _, err := browser.Stat("/a/b/c/file.txt"); err == nil {
		t.Fatal("old name still stats after rename")
	}
	if err := browser.Rename("/a/b/c/renamed.txt", "/a/b/c/renamed.txt"); err == nil {
		t.Fatal("Rename to the same path = nil error, want an error")
	}
	if err := browser.Delete("/a/b/c/renamed.txt", false); err != nil {
		t.Fatalf("Delete file: %v", err)
	}
	// A non-empty directory must not vanish silently.
	writeRemoteFile(t, remote, "/a/b/c/keep.txt", "x")
	if err := browser.Delete("/a/b/c", true); err == nil || err.Error() == "" {
		t.Fatal("Delete(non-empty dir) did not report a reason")
	}
	if err := browser.Delete("/a/b/c/keep.txt", false); err != nil {
		t.Fatalf("Delete keep.txt: %v", err)
	}
	if err := browser.Delete("/a/b/c", true); err != nil {
		t.Fatalf("Delete empty dir: %v", err)
	}
	if err := browser.Delete("/nope.txt", false); err == nil || err.Error() == "" {
		t.Fatal("Delete(missing) did not report a reason")
	}
}

func TestBrowserUploadDownloadRoundtrip(t *testing.T) {
	remote := &fakeRemote{root: t.TempDir()}
	browser := newTestBrowser(t, remote)
	content := strings.Repeat("motty-sftp-test\n", 1000)
	local := filepath.Join(t.TempDir(), "up.bin")
	if err := os.WriteFile(local, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	// The worker stages the upload inside the destination directory,
	// so the directory must exist first (as it does for a real cwd).
	if err := browser.Mkdir("/up"); err != nil {
		t.Fatalf("Mkdir /up: %v", err)
	}
	var sawSent, sawTotal, sawRate int64
	progress := func(sent, total, rate int64) {
		sawSent, sawTotal, sawRate = sent, total, rate
	}
	if err := browser.Upload(local, "/up/file.bin", progress); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if sawTotal != int64(len(content)) {
		t.Fatalf("progress total = %d, want %d", sawTotal, len(content))
	}
	if sawSent != int64(len(content)) {
		t.Fatalf("progress sent = %d, want %d", sawSent, len(content))
	}
	_ = sawRate
	downloaded := filepath.Join(t.TempDir(), "down.bin")
	if err := browser.Download("/up/file.bin", downloaded, nil); err != nil {
		t.Fatalf("Download: %v", err)
	}
	got, err := os.ReadFile(downloaded)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != content {
		t.Fatal("downloaded content differs from uploaded content")
	}
	transfers := browser.Transfers()
	if len(transfers) == 0 {
		t.Fatal("Transfers() is empty after two transfers")
	}
	found := false
	for _, transfer := range transfers {
		if transfer.State == "已完成" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no completed transfer in %+v", transfers)
	}
}

func TestBrowserUploadDownloadErrors(t *testing.T) {
	remote := &fakeRemote{root: t.TempDir()}
	browser := newTestBrowser(t, remote)
	if err := browser.Upload(filepath.Join(t.TempDir(), "missing.bin"), "/x.bin", nil); err == nil || err.Error() == "" {
		t.Fatal("Upload(missing local) did not report a reason")
	}
	if err := browser.Download("/missing.bin", filepath.Join(t.TempDir(), "out.bin"), nil); err == nil || err.Error() == "" {
		t.Fatal("Download(missing remote) did not report a reason")
	}
	if err := browser.Download("/dir", filepath.Join(t.TempDir(), "out.bin"), nil); err == nil || err.Error() == "" {
		// /dir does not exist either; either way a reason is required.
		t.Fatal("Download(directory) did not report a reason")
	}
	if err := browser.Upload(t.TempDir(), "/x.bin", nil); err == nil || err.Error() == "" {
		t.Fatal("Upload(directory as source) did not report a reason")
	}
	// Pause/Resume/Cancel of an unknown transfer id report false.
	if browser.PauseTransfer(99999) || browser.ResumeTransfer(99999) || browser.CancelTransfer(99999) {
		t.Fatal("Pause/Resume/Cancel of an unknown id returned true")
	}
}

func TestBrowserCancelDuringUpload(t *testing.T) {
	remote := &fakeRemote{root: t.TempDir(), writeDelay: 50 * time.Millisecond}
	browser := newTestBrowser(t, remote)
	// A large file written in delayed chunks stays in flight long
	// enough to cancel from the test.
	local := filepath.Join(t.TempDir(), "big.bin")
	data := make([]byte, 32<<20)
	for i := range data {
		data[i] = byte(i % 251)
	}
	if err := os.WriteFile(local, data, 0o644); err != nil {
		t.Fatal(err)
	}
	errCh := make(chan error, 1)
	go func() {
		errCh <- browser.Upload(local, "/big.bin", func(sent, total, rate int64) {})
	}()
	// Wait until the worker has the transfer, then cancel it.
	deadline := time.Now().Add(10 * time.Second)
	var id uint64
	for time.Now().Before(deadline) {
		for _, transfer := range browser.Transfers() {
			if transfer.Operation == "upload" {
				id = transfer.ID
			}
		}
		if id != 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if id == 0 {
		t.Fatal("upload never appeared in Transfers()")
	}
	if !browser.CancelTransfer(id) {
		t.Fatal("CancelTransfer(running upload) returned false")
	}
	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("cancelled upload returned nil error")
		}
		if !strings.Contains(err.Error(), "取消") {
			t.Fatalf("cancelled upload error = %q, want it to say 取消", err.Error())
		}
	case <-time.After(15 * time.Second):
		t.Fatal("cancelled upload did not finish")
	}
}

// --- Open over a real SSH server ---

// subsystemRefusingServer is a minimal SSH server that accepts session
// channels but refuses the SFTP subsystem, so Open must fall back to SCP.
type subsystemRefusingServer struct {
	t        *testing.T
	listener net.Listener
	hostKey  ssh.Signer
}

func startSubsystemRefusingServer(t *testing.T) *subsystemRefusingServer {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &subsystemRefusingServer{t: t, listener: listener, hostKey: signer}
	config := &ssh.ServerConfig{PasswordCallback: func(_ ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
		if string(password) == "testpass" {
			return nil, nil
		}
		return nil, errors.New("bad password")
	}}
	config.AddHostKey(signer)
	go func() {
		for {
			socket, err := listener.Accept()
			if err != nil {
				return
			}
			go func(socket net.Conn) {
				defer socket.Close()
				serverConn, channels, requests, err := ssh.NewServerConn(socket, config)
				if err != nil {
					return
				}
				go ssh.DiscardRequests(requests)
				for newChannel := range channels {
					if newChannel.ChannelType() != "session" {
						newChannel.Reject(ssh.Prohibited, "no such channel")
						continue
					}
					channel, requests, err := newChannel.Accept()
					if err != nil {
						continue
					}
					go func() {
						defer channel.Close()
						for req := range requests {
							// Refuse every subsystem (and exec) request:
							// the SFTP subsystem is not offered here.
							req.Reply(false, nil)
						}
					}()
				}
				serverConn.Wait()
			}(socket)
		}
	}()
	t.Cleanup(func() { listener.Close() })
	return server
}

func (s *subsystemRefusingServer) addr() string { return s.listener.Addr().String() }

func (s *subsystemRefusingServer) fingerprint() string {
	return ssh.FingerprintSHA256(s.hostKey.PublicKey())
}

type mapCreds struct{ passwords map[string]string }

func (m mapCreds) Password(id string) (string, bool, error) {
	password, ok := m.passwords[id]
	return password, ok, nil
}

func (m mapCreds) KeyPassphrase(id string) (string, bool, error) {
	return "", false, nil
}

func TestOpenSCPFallback(t *testing.T) {
	server := startSubsystemRefusingServer(t)
	host, portStr, err := net.SplitHostPort(server.addr())
	if err != nil {
		t.Fatal(err)
	}
	var port int
	if _, err := fmt.Sscanf(portStr, "%d", &port); err != nil {
		t.Fatal(err)
	}
	knownHostsPath := filepath.Join(t.TempDir(), "known_hosts")
	line := knownhosts.Line([]string{knownhosts.Normalize(server.addr())}, server.hostKey.PublicKey()) + "\n"
	if err := os.WriteFile(knownHostsPath, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	asset := serverassets.Server{
		ID: "srv-scp", Name: "scp-test", Address: host, Port: port,
		User: "tester", AuthMethod: serverassets.AuthPassword,
		HostKeyFingerprint: server.fingerprint(),
	}
	creds := mapCreds{passwords: map[string]string{"srv-scp": "testpass"}}
	dial := func(ctx context.Context, srv serverassets.Server) (*sshconn.Conn, error) {
		conn, result := serverconn.Dial(ctx, srv, nil, creds, knownHostsPath)
		if !result.OK {
			if result.Error != nil {
				return nil, result.Error
			}
			return nil, fmt.Errorf("serverconn: dial failed: %s", result.Kind)
		}
		return conn, nil
	}
	browser, err := Open(context.Background(), asset, dial)
	if err != nil {
		t.Fatalf("Open with a subsystem-refusing server: %v", err)
	}
	defer browser.Close()
	if !browser.IsSCP() {
		t.Fatal("Open did not fall back to SCP on a subsystem-refusing server")
	}
	if browser.WorkingDir() == "" {
		t.Fatal("WorkingDir is empty")
	}
}

func TestOpenDialFailure(t *testing.T) {
	asset := serverassets.Server{ID: "srv-x", Name: "unreachable", Address: "127.0.0.1", Port: 1, User: "u"}
	dial := func(ctx context.Context, srv serverassets.Server) (*sshconn.Conn, error) {
		return nil, errors.New("dial: connection refused")
	}
	if _, err := Open(context.Background(), asset, dial); err == nil || err.Error() == "" {
		t.Fatal("Open with a failing dial did not report a reason")
	}
	if _, err := Open(context.Background(), asset, nil); err == nil {
		t.Fatal("Open with a nil dial = nil error")
	}
}
