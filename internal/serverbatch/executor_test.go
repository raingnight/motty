package serverbatch

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"motty/internal/serverassets"
	"motty/internal/serverconn"
	"motty/internal/sshconn"
)

// dialFixture wires an Executor DialFunc to one real test SSH server
// through serverconn.Dial — the same path the production adapter uses —
// with per-server behaviour overrides for failure injection.
type dialFixture struct {
	t          *testing.T
	server     *testSSHServer
	knownHosts string
	creds      *mapCreds
	// failDial counts down per server ID: while > 0 the dial fails.
	failDial map[string]*atomic.Int32
	// dialErr, when set, is the error a failing dial returns.
	dialErr error
	// dials counts dial attempts (all servers).
	dials atomic.Int32
}

func newDialFixture(t *testing.T) *dialFixture {
	t.Helper()
	srv := startTestSSHServer(t, "testpass")
	knownHosts := filepath.Join(t.TempDir(), "known_hosts")
	srv.trust(t, knownHosts)
	return &dialFixture{
		t:          t,
		server:     srv,
		knownHosts: knownHosts,
		creds:      &mapCreds{passwords: map[string]string{}},
		failDial:   map[string]*atomic.Int32{},
	}
}

func (f *dialFixture) testServer(id, name string) serverassets.Server {
	host, port := f.server.host()
	return serverassets.Server{
		ID: id, Name: name, Address: host, Port: port,
		User: "testuser", AuthMethod: serverassets.AuthPassword,
	}
}

func (f *dialFixture) dialFunc() DialFunc {
	return func(ctx context.Context, server serverassets.Server) (*sshconn.Conn, error) {
		f.dials.Add(1)
		if counter, ok := f.failDial[server.ID]; ok && counter.Load() > 0 {
			counter.Add(-1)
			err := f.dialErr
			if err == nil {
				err = errors.New("connection refused")
			}
			return nil, err
		}
		f.creds.setPassword(server.ID, "testpass")
		conn, result := serverconn.Dial(ctx, server, nil, f.creds, f.knownHosts)
		if !result.OK {
			if result.Error != nil {
				return nil, result.Error
			}
			return nil, fmt.Errorf("serverconn: dial failed: %s", result.Kind)
		}
		return conn, nil
	}
}

func TestExecutorSuccessAndIsolation(t *testing.T) {
	f := newDialFixture(t)
	dial := f.dialFunc()
	// srv-bad always fails to dial; srv-ok runs for real.
	dial = func(ctx context.Context, server serverassets.Server) (*sshconn.Conn, error) {
		if server.ID == "srv-bad" {
			return nil, errors.New("connection refused")
		}
		return f.dialFunc()(ctx, server)
	}
	ex := NewExecutor(dial, DefaultRunOptions(), nil)
	servers := []serverassets.Server{
		f.testServer("srv-ok", "ok"),
		f.testServer("srv-bad", "bad"),
	}
	run, err := ex.Run(context.Background(), servers, "echo hello", "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if run.State != RunFinished || run.Aborted {
		t.Errorf("run state = %v aborted=%v, want finished", run.State, run.Aborted)
	}
	if len(run.Targets) != 2 {
		t.Fatalf("targets = %d, want 2", len(run.Targets))
	}
	ok := run.Targets[0]
	if ok.State != StateSuccess || !strings.Contains(ok.Output, "hello") {
		t.Errorf("ok target = %+v", ok)
	}
	if ok.ExitCode == nil || *ok.ExitCode != 0 {
		t.Errorf("ok exit code = %v, want 0", ok.ExitCode)
	}
	bad := run.Targets[1]
	if bad.State != StateFailed {
		t.Errorf("bad target state = %v, want failed", bad.State)
	}
	if bad.Error == "" || strings.Contains(bad.Error, "testpass") {
		t.Errorf("bad error = %q", bad.Error)
	}
	// One server's failure retried once (default MaxRetries=1) and
	// still failed: Retries counts the retries.
	if bad.Retries != 1 {
		t.Errorf("bad retries = %d, want 1", bad.Retries)
	}
}

func TestExecutorNonZeroExitIsFailed(t *testing.T) {
	f := newDialFixture(t)
	ex := NewExecutor(f.dialFunc(), DefaultRunOptions(), nil)
	run, err := ex.Run(context.Background(),
		[]serverassets.Server{f.testServer("srv-1", "one")}, "exit 3", "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	target := run.Targets[0]
	if target.State != StateFailed {
		t.Errorf("state = %v, want failed", target.State)
	}
	if target.ExitCode == nil || *target.ExitCode != 3 {
		t.Errorf("exit code = %v, want 3", target.ExitCode)
	}
	if !strings.Contains(target.Error, "3") {
		t.Errorf("error = %q, want it to name the exit code", target.Error)
	}
}

func TestExecutorRetryOnlyFailed(t *testing.T) {
	f := newDialFixture(t)
	// srv-flaky fails its first dial, then succeeds.
	flaky := &atomic.Int32{}
	flaky.Store(1)
	f.failDial["srv-flaky"] = flaky
	// srv-dead fails every dial.
	dead := &atomic.Int32{}
	dead.Store(10)
	f.failDial["srv-dead"] = dead
	ex := NewExecutor(f.dialFunc(), RunOptions{
		Concurrency: 5, TimeoutPerServer: 10 * time.Second,
		MaxRetries: 1, MaxOutputBytes: 1 << 20,
	}, nil)
	servers := []serverassets.Server{
		f.testServer("srv-flaky", "flaky"),
		f.testServer("srv-dead", "dead"),
		f.testServer("srv-ok", "ok"),
	}
	run, err := ex.Run(context.Background(), servers, "echo hi", "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	byID := map[string]ServerResult{}
	for _, target := range run.Targets {
		byID[target.ServerID] = target
	}
	if byID["srv-flaky"].State != StateSuccess || byID["srv-flaky"].Retries != 1 {
		t.Errorf("flaky = %+v, want success after 1 retry", byID["srv-flaky"])
	}
	if byID["srv-dead"].State != StateFailed || byID["srv-dead"].Retries != 1 {
		t.Errorf("dead = %+v, want failed after 1 retry", byID["srv-dead"])
	}
	if byID["srv-ok"].State != StateSuccess || byID["srv-ok"].Retries != 0 {
		t.Errorf("ok = %+v, want success with no retry", byID["srv-ok"])
	}
}

func TestExecutorConcurrencyLimit(t *testing.T) {
	f := newDialFixture(t)
	opts := DefaultRunOptions()
	opts.Concurrency = 2
	ex := NewExecutor(f.dialFunc(), opts, nil)
	servers := make([]serverassets.Server, 6)
	for i := range servers {
		servers[i] = f.testServer(fmt.Sprintf("srv-%d", i), fmt.Sprintf("s%d", i))
	}
	start := time.Now()
	run, err := ex.Run(context.Background(), servers, "sleep 0.5", "")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, target := range run.Targets {
		if target.State != StateSuccess {
			t.Errorf("target %s state = %v, want success", target.ServerID, target.State)
		}
	}
	if max := f.server.maxActive.Load(); max > 2 {
		t.Errorf("max concurrent exec sessions = %d, want <= 2", max)
	}
	// Serial would take 6 x 0.5s = 3s; two-at-a-time takes ~1.5s.
	if elapsed > 2800*time.Millisecond {
		t.Errorf("elapsed %v, want under 2.8s (concurrency was not honoured)", elapsed)
	}
}

func TestExecutorTimeout(t *testing.T) {
	f := newDialFixture(t)
	opts := DefaultRunOptions()
	opts.TimeoutPerServer = 300 * time.Millisecond
	opts.MaxRetries = 0
	ex := NewExecutor(f.dialFunc(), opts, nil)
	run, err := ex.Run(context.Background(),
		[]serverassets.Server{f.testServer("srv-1", "one")}, "sleep 5", "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	target := run.Targets[0]
	if target.State != StateTimeout {
		t.Errorf("state = %v, want timeout", target.State)
	}
	if target.Error == "" {
		t.Error("timeout target has no error text")
	}
	// A timeout is never retried, even with MaxRetries set.
	opts.MaxRetries = 2
	ex2 := NewExecutor(f.dialFunc(), opts, nil)
	run2, err := ex2.Run(context.Background(),
		[]serverassets.Server{f.testServer("srv-1", "one")}, "sleep 5", "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if run2.Targets[0].Retries != 0 {
		t.Errorf("timeout retries = %d, want 0", run2.Targets[0].Retries)
	}
}

func TestExecutorAbort(t *testing.T) {
	f := newDialFixture(t)
	opts := DefaultRunOptions()
	opts.Concurrency = 1
	opts.MaxRetries = 0
	ex := NewExecutor(f.dialFunc(), opts, nil)
	servers := make([]serverassets.Server, 3)
	for i := range servers {
		servers[i] = f.testServer(fmt.Sprintf("srv-%d", i), fmt.Sprintf("s%d", i))
	}
	done := make(chan *Run, 1)
	go func() {
		run, err := ex.Run(context.Background(), servers, "sleep 30", "")
		if err != nil {
			t.Errorf("Run: %v", err)
			done <- nil
			return
		}
		done <- run
	}()
	// Wait until the first server's dial has started (so its sleep is
	// running), give the exec a moment to start, then abort.
	deadline := time.Now().Add(10 * time.Second)
	for f.dials.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if f.dials.Load() == 0 {
		t.Fatal("no dial started within 10s")
	}
	time.Sleep(500 * time.Millisecond)
	ex.Abort()
	select {
	case run := <-done:
		if run == nil {
			t.Fatal("Run returned nil")
		}
		if run.State != RunAborted || !run.Aborted {
			t.Errorf("run state = %v aborted=%v, want aborted", run.State, run.Aborted)
		}
		cancelled := 0
		for _, target := range run.Targets {
			if target.State == StateCancelled {
				cancelled++
			}
			if target.State == StateSuccess {
				t.Errorf("target %s succeeded after abort", target.ServerID)
			}
		}
		if cancelled != 3 {
			t.Errorf("cancelled targets = %d, want 3", cancelled)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return after Abort")
	}
}

func TestExecutorBlockedRefused(t *testing.T) {
	f := newDialFixture(t)
	ex := NewExecutor(f.dialFunc(), DefaultRunOptions(), nil)
	_, err := ex.Run(context.Background(),
		[]serverassets.Server{f.testServer("srv-1", "one")}, "rm -rf /", "")
	if !errors.Is(err, ErrBlockedCommand) {
		t.Errorf("Run(rm -rf /) err = %v, want ErrBlockedCommand", err)
	}
	_, err = ex.Run(context.Background(),
		[]serverassets.Server{f.testServer("srv-1", "one")}, "   ", "")
	if err == nil {
		t.Error("Run with an empty command should fail")
	}
}

func TestExecutorOutputTruncation(t *testing.T) {
	f := newDialFixture(t)
	opts := DefaultRunOptions()
	opts.MaxOutputBytes = 1024
	opts.MaxRetries = 0
	ex := NewExecutor(f.dialFunc(), opts, nil)
	run, err := ex.Run(context.Background(),
		[]serverassets.Server{f.testServer("srv-1", "one")}, "seq 1 100000", "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	target := run.Targets[0]
	if target.State != StateSuccess {
		t.Errorf("state = %v, want success", target.State)
	}
	if !target.OutputTruncated {
		t.Error("OutputTruncated is false, want true")
	}
	if len(target.Output) > 1024 {
		t.Errorf("output length %d exceeds the 1024 limit", len(target.Output))
	}
}

func TestExecutorOnUpdate(t *testing.T) {
	f := newDialFixture(t)
	var mu sync.Mutex
	updates := 0
	ex := NewExecutor(f.dialFunc(), DefaultRunOptions(), func(run *Run) {
		mu.Lock()
		updates++
		mu.Unlock()
	})
	servers := []serverassets.Server{
		f.testServer("srv-1", "one"),
		f.testServer("srv-2", "two"),
	}
	if _, err := ex.Run(context.Background(), servers, "echo hi", ""); err != nil {
		t.Fatalf("Run: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if updates < 2 {
		t.Errorf("onUpdate called %d times, want at least 2", updates)
	}
}

func TestExecutorEmptyServers(t *testing.T) {
	f := newDialFixture(t)
	ex := NewExecutor(f.dialFunc(), DefaultRunOptions(), nil)
	run, err := ex.Run(context.Background(), nil, "echo hi", "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if run.State != RunFinished || len(run.Targets) != 0 {
		t.Errorf("empty run = state %v, %d targets", run.State, len(run.Targets))
	}
}

func TestDefaultRunOptions(t *testing.T) {
	opts := DefaultRunOptions()
	if opts.Concurrency != 20 || opts.TimeoutPerServer != 5*time.Minute ||
		opts.MaxRetries != 1 || opts.MaxOutputBytes != 1<<20 {
		t.Errorf("DefaultRunOptions = %+v", opts)
	}
}
