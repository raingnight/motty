// Package serversftp is the phase 5 SFTP file-management core for one
// managed server: directory browsing, mkdir/rename/delete/stat and
// upload/download over the existing sftpclient transport.
//
// It owns no UI and no SSH dialing of its own: Open takes a DialFunc with
// the same signature as serverbatch.DialFunc (in production a
// serverconn.Dial wrapper, so jump hosts, fingerprint checks and
// vault-backed credentials stay in one place) and dials once. The SFTP
// subsystem is tried first; when the server refuses it
// (sftpclient.IsSubsystemRefused) the browser falls back to the SCP
// backend (sftpclient.OpenSCP) and reports IsSCP so the UI can say the
// listing is approximate. Transfers run through sftpclient.Worker, which
// supplies the resume check, pause/cancel and bounded concurrency — this
// package never reimplements the transport.
//
// Deletion and friends do their own confirmation nowhere: the UI layer
// confirms, this package just performs. Paths are normalised and a ".."
// escape out of the joined directory is refused. No credential value is
// ever logged or returned: errors name paths and reasons only.
package serversftp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"motty/internal/serverassets"
	"motty/internal/sftpclient"
	"motty/internal/sshconn"
)

// DialFunc dials one server and returns a live connection. It is the same
// signature as serverbatch.DialFunc; the workbench passes one function
// for both.
type DialFunc func(ctx context.Context, server serverassets.Server) (*sshconn.Conn, error)

// Entry is one remote directory item for display.
type Entry struct {
	Name    string
	Size    int64
	ModTime time.Time
	IsDir   bool
	// Type is the display kind: "目录", "文件", "符号链接" or "其他".
	Type string
}

// ProgressFunc reports transfer progress: bytes sent so far, the total
// when known (0 when the worker has not measured it yet) and the worker's
// smoothed bytes-per-second estimate.
type ProgressFunc func(sent, total, bytesPerSec int64)

// TransferInfo is one transfer's snapshot for the UI's transfer list.
type TransferInfo struct {
	ID             uint64
	Operation      string // "upload" or "download"
	Name           string // display name (base name of the file)
	Source         string
	Destination    string
	State          string // queued, running, completed, failed, canceled, paused, interrupted
	BytesDone      int64
	BytesTotal     int64
	BytesPerSecond int64
	Error          string
}

// remoteOps is the write side of a remote backend. Both the SFTP remote
// (which embeds *sftp.Client) and the SCP remote implement it; the
// exported sftpclient.Remote interface only exposes the read side, so
// this package asserts to it locally instead of depending on the
// unexported transferRemote type.
type remoteOps interface {
	Lstat(path string) (os.FileInfo, error)
	Mkdir(path string) error
	Rename(oldPath, newPath string) error
	Remove(path string) error
	RemoveDirectory(path string) error
}

var errWorkerClosed = errors.New("sftp: 传输通道已关闭")

// Browser is one open SFTP/SCP session to one server.
type Browser struct {
	server serverassets.Server
	conn   *sshconn.Conn
	remote sftpclient.Remote
	scp    bool
	worker *sftpclient.Worker
	home   string

	mu     sync.Mutex
	closed bool

	resultMu sync.Mutex
	waiters  map[uint64]chan error
}

// openRemote opens one file backend over an authenticated SSH client:
// SFTP first, SCP when the server refuses the subsystem. It reports
// whether the SCP backend was chosen.
func openRemote(client *ssh.Client) (sftpclient.Remote, bool, error) {
	if client == nil {
		return nil, false, fmt.Errorf("sftp: SSH 客户端未连接")
	}
	remote, err := sftpclient.Open(client)
	if err == nil {
		return remote, false, nil
	}
	if !sftpclient.IsSubsystemRefused(err) {
		return nil, false, fmt.Errorf("sftp: 打开 SFTP 子系统失败: %w", err)
	}
	scpRemote, scpErr := sftpclient.OpenSCP(client)
	if scpErr != nil {
		return nil, false, fmt.Errorf("sftp: SFTP 子系统被拒绝，且 SCP 回退失败: %w", scpErr)
	}
	return scpRemote, true, nil
}

// Open dials one server and opens its file backend. The caller owns the
// returned Browser and must Close it. A dial failure, a refused subsystem
// with a failed SCP fallback, or any other open failure is reported with
// a concrete reason and no credential.
func Open(ctx context.Context, server serverassets.Server, dial DialFunc) (*Browser, error) {
	if dial == nil {
		return nil, fmt.Errorf("sftp: 没有可用的拨号函数")
	}
	conn, err := dial(ctx, server)
	if err != nil {
		return nil, fmt.Errorf("sftp: 连接 %s 失败: %w", server.Name, err)
	}
	client := conn.Client()
	if client == nil {
		conn.Close()
		return nil, fmt.Errorf("sftp: 连接 %s 未建立 SSH 会话", server.Name)
	}
	remote, scp, err := openRemote(client)
	if err != nil {
		conn.Close()
		return nil, err
	}
	home, _ := remote.Getwd()
	if home == "" {
		home = "/"
	}
	browser := &Browser{
		server:  server,
		conn:    conn,
		remote:  remote,
		scp:     scp,
		home:    home,
		waiters: map[uint64]chan error{},
	}
	// The worker opens its own backend over the same SSH client whenever
	// it needs one, so a transfer never shares the listing's subsystem
	// session. Its notify is unused: progress is polled by the blocking
	// Upload/Download calls, and results are drained by drainResults so
	// the worker's publish never blocks.
	browser.worker = sftpclient.NewWorker(func() (sftpclient.Remote, error) {
		remote, _, err := openRemote(conn.Client())
		return remote, err
	}, func() {})
	go browser.drainResults()
	return browser, nil
}

// IsSCP reports whether this browser fell back to the SCP backend
// (listings are approximate there).
func (b *Browser) IsSCP() bool { return b.scp }

// WorkingDir is the remote home directory the browser started in.
func (b *Browser) WorkingDir() string { return b.home }

// ServerName is the display name of the server this browser is open on.
func (b *Browser) ServerName() string { return b.server.Name }

// ServerID is the asset id of the server this browser is open on.
func (b *Browser) ServerID() string { return b.server.ID }

// Close ends the transfers, the backend and the SSH connection. It is
// safe to call twice.
func (b *Browser) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	b.mu.Unlock()
	if b.worker != nil {
		b.worker.Close()
	}
	var err error
	if b.remote != nil {
		err = b.remote.Close()
	}
	if b.conn != nil {
		b.conn.Close()
	}
	return err
}

func (b *Browser) isClosed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closed
}

// --- paths ---

// CleanPath normalises a remote path: it must be non-empty and absolute
// after cleaning. A relative path is an error, never silently resolved
// against the working directory.
func CleanPath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", fmt.Errorf("sftp: 路径为空")
	}
	if !path.IsAbs(p) {
		return "", fmt.Errorf("sftp: 路径 %q 不是绝对路径", p)
	}
	cleaned := path.Clean(p)
	if cleaned == "." || cleaned == "" {
		return "", fmt.Errorf("sftp: 路径 %q 无效", p)
	}
	return cleaned, nil
}

// JoinDir joins one entry name onto a directory: the name must be a
// single segment (no "/", not "." or ".."), so a crafted name can never
// escape the directory it is listed under.
func JoinDir(dir, name string) (string, error) {
	if name == "" || name == "." || name == ".." || strings.Contains(name, "/") {
		return "", fmt.Errorf("sftp: 非法的文件名 %q", name)
	}
	cleaned, err := CleanPath(dir)
	if err != nil {
		return "", err
	}
	return path.Join(cleaned, name), nil
}

// ops returns the backend's write side, or an error when the backend
// cannot do write operations.
func (b *Browser) ops() (remoteOps, error) {
	ops, ok := b.remote.(remoteOps)
	if !ok {
		return nil, fmt.Errorf("sftp: 当前连接不支持写操作")
	}
	return ops, nil
}

// entryOf converts one FileInfo into a display entry.
func entryOf(info os.FileInfo) Entry {
	entry := Entry{
		Name:    info.Name(),
		Size:    info.Size(),
		ModTime: info.ModTime(),
		IsDir:   info.IsDir(),
		Type:    "文件",
	}
	switch {
	case info.IsDir():
		entry.Type = "目录"
	case info.Mode()&os.ModeSymlink != 0:
		entry.Type = "符号链接"
	case !info.Mode().IsRegular():
		entry.Type = "其他"
	}
	return entry
}

// ListDir lists one remote directory: directories first, then names
// case-insensitively. "." and ".." are never returned. A failure names
// the directory and the reason.
func (b *Browser) ListDir(dir string) ([]Entry, error) {
	if b.isClosed() {
		return nil, fmt.Errorf("sftp: 连接已关闭")
	}
	cleaned, err := CleanPath(dir)
	if err != nil {
		return nil, err
	}
	infos, err := b.remote.ReadDir(cleaned)
	if err != nil {
		return nil, fmt.Errorf("sftp: 读取目录 %s 失败: %w", cleaned, err)
	}
	entries := make([]Entry, 0, len(infos))
	for _, info := range infos {
		if info.Name() == "." || info.Name() == ".." {
			continue
		}
		entries = append(entries, entryOf(info))
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].IsDir != entries[j].IsDir {
			return entries[i].IsDir
		}
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})
	return entries, nil
}

// Stat reports one remote path. When the backend cannot stat directly it
// falls back to listing the parent directory.
func (b *Browser) Stat(p string) (Entry, error) {
	if b.isClosed() {
		return Entry{}, fmt.Errorf("sftp: 连接已关闭")
	}
	cleaned, err := CleanPath(p)
	if err != nil {
		return Entry{}, err
	}
	ops, err := b.ops()
	if err != nil {
		return Entry{}, err
	}
	info, err := ops.Lstat(cleaned)
	if err == nil {
		return entryOf(info), nil
	}
	// Lstat is not always the failure's whole story (a stale link, a
	// permission gap on the parent): try the parent's listing before
	// giving up, so the error names the real cause.
	parent, name := path.Split(cleaned)
	entries, listErr := b.ListDir(parent)
	if listErr != nil {
		return Entry{}, fmt.Errorf("sftp: 查看 %s 失败: %w", cleaned, err)
	}
	for _, entry := range entries {
		if entry.Name == name {
			return entry, nil
		}
	}
	return Entry{}, fmt.Errorf("sftp: 查看 %s 失败: %w", cleaned, err)
}

// Mkdir creates one remote directory (parents included, like mkdir -p).
func (b *Browser) Mkdir(p string) error {
	if b.isClosed() {
		return fmt.Errorf("sftp: 连接已关闭")
	}
	cleaned, err := CleanPath(p)
	if err != nil {
		return err
	}
	ops, err := b.ops()
	if err != nil {
		return err
	}
	if err := ops.Mkdir(cleaned); err != nil {
		return fmt.Errorf("sftp: 创建目录 %s 失败: %w", cleaned, err)
	}
	return nil
}

// Rename moves or renames one remote path. The destination must not
// already exist (the backends refuse to replace it).
func (b *Browser) Rename(oldPath, newPath string) error {
	if b.isClosed() {
		return fmt.Errorf("sftp: 连接已关闭")
	}
	oldCleaned, err := CleanPath(oldPath)
	if err != nil {
		return err
	}
	newCleaned, err := CleanPath(newPath)
	if err != nil {
		return err
	}
	if oldCleaned == newCleaned {
		return fmt.Errorf("sftp: 新旧路径相同")
	}
	ops, err := b.ops()
	if err != nil {
		return err
	}
	if err := ops.Rename(oldCleaned, newCleaned); err != nil {
		return fmt.Errorf("sftp: 重命名 %s 为 %s 失败: %w", oldCleaned, newCleaned, err)
	}
	return nil
}

// Delete removes one remote path: a directory goes through
// RemoveDirectory (which refuses a non-empty directory), a file through
// Remove. The UI confirms first; this method only performs.
func (b *Browser) Delete(p string, isDir bool) error {
	if b.isClosed() {
		return fmt.Errorf("sftp: 连接已关闭")
	}
	cleaned, err := CleanPath(p)
	if err != nil {
		return err
	}
	ops, err := b.ops()
	if err != nil {
		return err
	}
	var removeErr error
	if isDir {
		removeErr = ops.RemoveDirectory(cleaned)
	} else {
		removeErr = ops.Remove(cleaned)
	}
	if removeErr != nil {
		return fmt.Errorf("sftp: 删除 %s 失败: %w", cleaned, removeErr)
	}
	return nil
}

// --- transfers ---

// drainResults reads the worker's results so its publish never blocks,
// and wakes the Upload/Download call waiting on that transfer id. The
// worker closes the channel when it shuts down, which fails every
// waiter instead of leaving it hung.
func (b *Browser) drainResults() {
	for result := range b.worker.Results() {
		b.resultMu.Lock()
		waiter, ok := b.waiters[result.TransferID]
		b.resultMu.Unlock()
		if ok {
			select {
			case waiter <- result.Err:
			default:
			}
		}
	}
	b.resultMu.Lock()
	for _, waiter := range b.waiters {
		select {
		case waiter <- errWorkerClosed:
		default:
		}
	}
	b.resultMu.Unlock()
}

// transferSnapshot finds one transfer's snapshot, if it is still tracked.
func (b *Browser) transferSnapshot(id uint64) (sftpclient.TransferSnapshot, bool) {
	for _, snapshot := range b.worker.Transfers(nil) {
		if snapshot.ID == id {
			return snapshot, true
		}
	}
	return sftpclient.TransferSnapshot{}, false
}

func transferStateText(state sftpclient.TransferState) string {
	switch state {
	case sftpclient.TransferQueued:
		return "排队中"
	case sftpclient.TransferRunning:
		return "传输中"
	case sftpclient.TransferCompleted:
		return "已完成"
	case sftpclient.TransferFailed:
		return "失败"
	case sftpclient.TransferCanceled:
		return "已取消"
	case sftpclient.TransferPaused:
		return "已暂停"
	case sftpclient.TransferInterrupted:
		return "已中断"
	default:
		return "未知"
	}
}

func transferOpText(op sftpclient.Operation) string {
	switch op {
	case sftpclient.OperationUpload:
		return "upload"
	case sftpclient.OperationDownload:
		return "download"
	default:
		return "transfer"
	}
}

// Transfers snapshots every transfer the worker still tracks, newest
// first by queue order.
func (b *Browser) Transfers() []TransferInfo {
	snapshots := b.worker.Transfers(nil)
	sort.SliceStable(snapshots, func(i, j int) bool { return snapshots[i].ID > snapshots[j].ID })
	out := make([]TransferInfo, 0, len(snapshots))
	for _, snapshot := range snapshots {
		name := path.Base(snapshot.Destination)
		if snapshot.Operation == sftpclient.OperationDownload {
			name = path.Base(snapshot.Source)
		}
		out = append(out, TransferInfo{
			ID:             snapshot.ID,
			Operation:      transferOpText(snapshot.Operation),
			Name:           name,
			Source:         snapshot.Source,
			Destination:    snapshot.Destination,
			State:          transferStateText(snapshot.State),
			BytesDone:      snapshot.BytesDone,
			BytesTotal:     snapshot.BytesTotal,
			BytesPerSecond: snapshot.BytesPerSecond,
			Error:          snapshot.Error,
		})
	}
	return out
}

// PauseTransfer pauses one transfer (it keeps its place and resumes
// where it stopped).
func (b *Browser) PauseTransfer(id uint64) bool { return b.worker.Pause(id) }

// ResumeTransfer resumes a paused transfer.
func (b *Browser) ResumeTransfer(id uint64) bool { return b.worker.Resume(id) }

// CancelTransfer cancels one transfer.
func (b *Browser) CancelTransfer(id uint64) bool { return b.worker.CancelTransfer(id) }

// waitTransfer blocks until one queued transfer reaches a terminal state,
// reporting progress about ten times a second. A paused transfer stays
// in the wait (its snapshot reports 0 B/s); a canceled, failed or
// interrupted one returns its reason. The wait ends promptly when the
// browser closes.
func (b *Browser) waitTransfer(id uint64, knownTotal int64, progress ProgressFunc) error {
	waiter := make(chan error, 1)
	b.resultMu.Lock()
	b.waiters[id] = waiter
	b.resultMu.Unlock()
	defer func() {
		b.resultMu.Lock()
		delete(b.waiters, id)
		b.resultMu.Unlock()
	}()
	if progress != nil {
		progress(0, knownTotal, 0)
	}
	// reportFinal sends one last progress update from the terminal
	// snapshot, so a transfer that finishes between ticks still ends
	// at 100% instead of at the last polled value.
	reportFinal := func() {
		if progress == nil {
			return
		}
		snapshot, ok := b.transferSnapshot(id)
		if !ok {
			return
		}
		total := snapshot.BytesTotal
		if total <= 0 {
			total = knownTotal
		}
		progress(snapshot.BytesDone, total, 0)
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case err := <-waiter:
			if err != nil {
				if errors.Is(err, errWorkerClosed) {
					return err
				}
				if errors.Is(err, sftpclient.ErrCanceled) {
					return fmt.Errorf("sftp: 传输已取消")
				}
				return fmt.Errorf("sftp: 传输失败: %w", err)
			}
			reportFinal()
			return nil
		case <-ticker.C:
			if b.isClosed() {
				return fmt.Errorf("sftp: 连接已关闭，传输中断")
			}
			snapshot, ok := b.transferSnapshot(id)
			if !ok {
				continue
			}
			total := snapshot.BytesTotal
			if total <= 0 {
				total = knownTotal
			}
			if progress != nil {
				progress(snapshot.BytesDone, total, snapshot.BytesPerSecond)
			}
			switch snapshot.State {
			case sftpclient.TransferCompleted:
				reportFinal()
				return nil
			case sftpclient.TransferFailed:
				if snapshot.Error != "" {
					return fmt.Errorf("sftp: 传输失败: %s", snapshot.Error)
				}
				return fmt.Errorf("sftp: 传输失败")
			case sftpclient.TransferCanceled:
				return fmt.Errorf("sftp: 传输已取消")
			case sftpclient.TransferInterrupted:
				if snapshot.Error != "" {
					return fmt.Errorf("sftp: 传输中断: %s", snapshot.Error)
				}
				return fmt.Errorf("sftp: 传输中断（连接丢失）")
			}
		}
	}
}

// Upload copies one local file to a remote path. It blocks until the
// transfer finishes, fails or is cancelled, and reports progress through
// the callback. The worker queues it with ExistingResume, so an
// interrupted upload continues where it stopped instead of starting
// over; a local file that changed meanwhile is re-verified by the
// worker rather than blindly resumed.
func (b *Browser) Upload(localPath, remotePath string, progress ProgressFunc) error {
	if b.isClosed() {
		return fmt.Errorf("sftp: 连接已关闭")
	}
	info, err := os.Stat(localPath)
	if err != nil {
		return fmt.Errorf("sftp: 本地文件 %s 不可读: %w", localPath, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("sftp: %s 不是普通文件", localPath)
	}
	cleaned, err := CleanPath(remotePath)
	if err != nil {
		return err
	}
	id, queued := b.worker.QueueUpload(localPath, cleaned, false, sftpclient.ExistingResume)
	if !queued {
		return fmt.Errorf("sftp: 传输队列已满，上传 %s 未排队", localPath)
	}
	if err := b.waitTransfer(id, info.Size(), progress); err != nil {
		return fmt.Errorf("sftp: 上传 %s 到 %s 失败: %w", localPath, cleaned, err)
	}
	return nil
}

// Download copies one remote file to a local path. It blocks until the
// transfer finishes, fails or is cancelled, and reports progress through
// the callback. The worker queues it with ExistingResume, so an
// interrupted download continues where it stopped.
func (b *Browser) Download(remotePath, localPath string, progress ProgressFunc) error {
	if b.isClosed() {
		return fmt.Errorf("sftp: 连接已关闭")
	}
	cleaned, err := CleanPath(remotePath)
	if err != nil {
		return err
	}
	stat, err := b.Stat(cleaned)
	if err != nil {
		return err
	}
	if stat.IsDir {
		return fmt.Errorf("sftp: %s 是目录，不能下载", cleaned)
	}
	if localPath == "" {
		return fmt.Errorf("sftp: 本地保存路径为空")
	}
	parent := filepath.Dir(localPath)
	// Local paths are OS paths, not remote paths.
	if dir := parent; dir != "" && dir != "." {
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			return fmt.Errorf("sftp: 本地目录 %s 不存在", dir)
		}
	}
	id, queued := b.worker.QueueDownload(cleaned, localPath, false, sftpclient.ExistingResume)
	if !queued {
		return fmt.Errorf("sftp: 传输队列已满，下载 %s 未排队", cleaned)
	}
	if err := b.waitTransfer(id, stat.Size, progress); err != nil {
		return fmt.Errorf("sftp: 下载 %s 到 %s 失败: %w", cleaned, localPath, err)
	}
	return nil
}

// ReadFile reads one remote file (bounded), for callers that need the
// content rather than a transfer. It is a thin wrapper over the
// sftpclient helper.
func (b *Browser) ReadFile(p string, limit int) ([]byte, bool, error) {
	cleaned, err := CleanPath(p)
	if err != nil {
		return nil, false, err
	}
	data, truncated, err := sftpclient.ReadAll(b.remote, cleaned, limit)
	if err != nil {
		return nil, false, fmt.Errorf("sftp: 读取 %s 失败: %w", cleaned, err)
	}
	return data, truncated, nil
}
