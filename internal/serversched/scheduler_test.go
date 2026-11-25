package serversched

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"motty/internal/serverassets"
	"motty/internal/servercollect"
	"motty/internal/serverconn"
	"motty/internal/sshconn"
)

// fakeCollector is a Collect stub with controllable failures, blocking and
// concurrency accounting. It never touches SSH.
type fakeCollector struct {
	mu            sync.Mutex
	calls         int
	callsByKind   map[servercollect.MetricKind]int
	callsByServer map[string]int
	cur, max      int32
	block         chan struct{}
	failKinds     map[servercollect.MetricKind]int // remaining failures per kind
	failServers   map[string]bool
	roundErrTimes int
	delay         time.Duration
}

func newFakeCollector() *fakeCollector {
	return &fakeCollector{
		callsByKind:   map[servercollect.MetricKind]int{},
		callsByServer: map[string]int{},
		failKinds:     map[servercollect.MetricKind]int{},
		failServers:   map[string]bool{},
	}
}

func (f *fakeCollector) setBlock(ch chan struct{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.block = ch
}

func (f *fakeCollector) resetCounts() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = 0
	f.callsByKind = map[servercollect.MetricKind]int{}
	f.callsByServer = map[string]int{}
}

func (f *fakeCollector) counts() (int, map[servercollect.MetricKind]int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[servercollect.MetricKind]int{}
	for k, v := range f.callsByKind {
		out[k] = v
	}
	return f.calls, out
}

func (f *fakeCollector) callsFor(kind servercollect.MetricKind) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.callsByKind[kind]
}

func (f *fakeCollector) Collect(ctx context.Context, conn *sshconn.Conn, server serverassets.Server, kinds []servercollect.MetricKind) (servercollect.RoundResult, error) {
	cur := atomic.AddInt32(&f.cur, 1)
	for {
		m := atomic.LoadInt32(&f.max)
		if cur <= m || atomic.CompareAndSwapInt32(&f.max, m, cur) {
			break
		}
	}
	defer atomic.AddInt32(&f.cur, -1)

	f.mu.Lock()
	f.calls++
	for _, k := range kinds {
		f.callsByKind[k]++
	}
	f.callsByServer[server.ID]++
	roundErr := f.roundErrTimes > 0
	if roundErr {
		f.roundErrTimes--
	}
	block := f.block
	delay := f.delay
	f.mu.Unlock()

	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return servercollect.RoundResult{ServerID: server.ID, Interrupted: true}, ctx.Err()
		}
	}
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return servercollect.RoundResult{ServerID: server.ID, Interrupted: true}, ctx.Err()
		}
	}
	if roundErr {
		return servercollect.RoundResult{}, errors.New("fake round error")
	}
	now := time.Now()
	res := servercollect.RoundResult{ServerID: server.ID, StartedAt: now}
	for _, kind := range kinds {
		f.mu.Lock()
		fail := f.failServers[server.ID]
		rem := f.failKinds[kind]
		if rem > 0 {
			f.failKinds[kind] = rem - 1
		}
		f.mu.Unlock()
		if fail || rem > 0 {
			res.Values = append(res.Values, servercollect.MetricValue{
				Kind: kind, CollectedAt: now,
				StaleAfter:  servercollect.StaleFactor * servercollect.DefaultIntervals[kind],
				Unavailable: "fake unavailable",
			})
			continue
		}
		res.Values = append(res.Values, testValue(kind, now))
	}
	res.FinishedAt = time.Now()
	return res, nil
}

func stubDialOK(ctx context.Context, server serverassets.Server, jump *serverassets.JumpHost, creds serverconn.CredentialProvider, knownHostsPath string) (*sshconn.Conn, serverconn.TestResult) {
	return nil, serverconn.TestResult{ServerID: server.ID, Stage: serverconn.StageAuth, OK: true}
}

type dialStub struct {
	mu    sync.Mutex
	calls int
	fail  bool
}

func (d *dialStub) dial(ctx context.Context, server serverassets.Server, jump *serverassets.JumpHost, creds serverconn.CredentialProvider, knownHostsPath string) (*sshconn.Conn, serverconn.TestResult) {
	d.mu.Lock()
	d.calls++
	fail := d.fail
	d.mu.Unlock()
	if fail {
		return nil, serverconn.TestResult{ServerID: server.ID, Kind: serverconn.KindNetworkUnreachable, Error: errors.New("fake dial failure")}
	}
	return nil, serverconn.TestResult{ServerID: server.ID, Stage: serverconn.StageAuth, OK: true}
}

func (d *dialStub) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

func testEngine(t *testing.T, fc *fakeCollector, opts func(*Config)) (*Engine, *serverassets.Store) {
	t.Helper()
	raw := openTestRaw(t)
	assets := serverassets.NewStore(raw)
	cfg := Config{
		Store:         raw,
		Assets:        assets,
		Collector:     fc,
		Dial:          stubDialOK,
		TickInterval:  50 * time.Millisecond,
		RoundTimeout:  5 * time.Second,
		RetryInterval: 10 * time.Millisecond,
		MaxRetries:    2,
	}
	if opts != nil {
		opts(&cfg)
	}
	return New(cfg), assets
}

func createServers(t *testing.T, assets *serverassets.Store, n int) []serverassets.Server {
	t.Helper()
	out := make([]serverassets.Server, 0, n)
	for i := 0; i < n; i++ {
		created, err := assets.CreateServer(serverassets.Server{
			Name: fmt.Sprintf("srv-%d", i), Address: fmt.Sprintf("10.0.0.%d", i+1),
			Port: 22, User: "root", AuthMethod: serverassets.AuthPassword,
		})
		if err != nil {
			t.Fatalf("CreateServer: %v", err)
		}
		out = append(out, created)
	}
	return out
}

// waitFor polls cond until it is true or the deadline passes.
func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// startAndSettle starts the engine and waits until every server's first
// scheduled round (all kinds) has run to completion — values stored and
// no job still in flight — then resets the collector counts so the test
// that follows measures only its own work. Waiting on call counts alone
// would race: a call is counted at entry, while its in-flight mark is
// still held.
func startAndSettle(t *testing.T, e *Engine, fc *fakeCollector, servers []serverassets.Server) {
	t.Helper()
	e.Start()
	waitFor(t, "first scheduled round", 10*time.Second, func() bool {
		for _, srv := range servers {
			latest, ok := e.Latest(srv.ID)
			if !ok {
				return false
			}
			for _, kind := range servercollect.AllMetricKinds {
				if _, ok := latest.Get(kind); !ok {
					return false
				}
			}
		}
		e.mu.Lock()
		empty := len(e.inFlight) == 0
		e.mu.Unlock()
		return empty
	})
	fc.resetCounts()
}

func TestEngineMaxConcurrencyAndQueue(t *testing.T) {
	fc := newFakeCollector()
	block := make(chan struct{})
	fc.setBlock(block)
	e, assets := testEngine(t, fc, nil)
	servers := createServers(t, assets, 25)
	e.Start()
	defer e.Stop()
	// With every collector call blocked, at most 20 may run at once.
	waitFor(t, "concurrency to reach 20", 10*time.Second, func() bool {
		return atomic.LoadInt32(&fc.cur) == 20
	})
	if max := atomic.LoadInt32(&fc.max); max > 20 {
		t.Fatalf("max concurrent Collect calls = %d, want <= 20", max)
	}
	close(block)
	// All 25 servers x 10 kinds drain through the queue. Each job
	// buffers its values and the flusher writes them, so the drain is
	// bounded by collection, not by store throughput.
	waitFor(t, "all queued jobs to drain", 60*time.Second, func() bool {
		calls, _ := fc.counts()
		return calls >= len(servers)*len(servercollect.AllMetricKinds)
	})
	if max := atomic.LoadInt32(&fc.max); max > 20 {
		t.Fatalf("max concurrent Collect calls = %d, want <= 20", max)
	}
}

func TestEngineSingleFailureDoesNotBlockOthers(t *testing.T) {
	fc := newFakeCollector()
	e, assets := testEngine(t, fc, nil)
	servers := createServers(t, assets, 3)
	fc.mu.Lock()
	fc.failServers[servers[0].ID] = true
	fc.mu.Unlock()
	e.Start()
	defer e.Stop()
	// Wait for what the test asserts: a CPU value for each healthy
	// server. Waiting only for any value to appear would race the CPU
	// job, which runs as its own queued job.
	waitFor(t, "healthy servers to get data", 10*time.Second, func() bool {
		for _, srv := range servers[1:] {
			latest, ok := e.Latest(srv.ID)
			if !ok {
				return false
			}
			if _, ok := latest.Get(servercollect.MetricCPU); !ok {
				return false
			}
		}
		return true
	})
	if _, found := e.Latest(servers[0].ID); found {
		t.Fatal("failed server got stored values")
	}
	for _, srv := range servers[1:] {
		latest, _ := e.Latest(srv.ID)
		if _, ok := latest.Get(servercollect.MetricCPU); !ok {
			t.Fatalf("healthy server %s has no CPU value", srv.ID)
		}
	}
}

func TestEngineRetryOnlyFailedKinds(t *testing.T) {
	fc := newFakeCollector()
	e, assets := testEngine(t, fc, func(cfg *Config) {
		cfg.MaxRetries = 2
		cfg.RetryInterval = 10 * time.Millisecond
	})
	servers := createServers(t, assets, 1)
	// A template with long intervals keeps scheduled jobs out of the way
	// after the first round settles.
	intervals := map[servercollect.MetricKind]time.Duration{}
	for _, kind := range servercollect.AllMetricKinds {
		intervals[kind] = time.Hour
	}
	if err := e.store.SaveTemplate(servercollect.Template{ID: "slow", Name: "Slow", Intervals: intervals}); err != nil {
		t.Fatalf("SaveTemplate: %v", err)
	}
	e.SetServerTemplate(servers[0].ID, "slow")
	startAndSettle(t, e, fc, servers)
	fc.mu.Lock()
	fc.failKinds[servercollect.MetricCPU] = 2 // fail twice, succeed on the 2nd retry
	fc.mu.Unlock()
	res, err := e.CollectNow(servers[0].ID)
	if err != nil {
		t.Fatalf("CollectNow: %v", err)
	}
	if res.Interrupted {
		t.Fatal("CollectNow was interrupted")
	}
	// CPU was collected 3 times (initial + 2 retries); memory once.
	if got := fc.callsFor(servercollect.MetricCPU); got != 3 {
		t.Fatalf("CPU Collect calls = %d, want 3 (initial + 2 retries)", got)
	}
	if got := fc.callsFor(servercollect.MetricMemory); got != 1 {
		t.Fatalf("memory Collect calls = %d, want 1 (no retry of a success)", got)
	}
	latest, _ := e.Latest(servers[0].ID)
	if _, ok := latest.Get(servercollect.MetricCPU); !ok {
		t.Fatal("CPU value missing after successful retry")
	}
}

func TestEngineRetryOnRoundError(t *testing.T) {
	fc := newFakeCollector()
	e, assets := testEngine(t, fc, func(cfg *Config) {
		cfg.MaxRetries = 2
		cfg.RetryInterval = 10 * time.Millisecond
	})
	servers := createServers(t, assets, 1)
	intervals := map[servercollect.MetricKind]time.Duration{}
	for _, kind := range servercollect.AllMetricKinds {
		intervals[kind] = time.Hour
	}
	if err := e.store.SaveTemplate(servercollect.Template{ID: "slow", Name: "Slow", Intervals: intervals}); err != nil {
		t.Fatalf("SaveTemplate: %v", err)
	}
	e.SetServerTemplate(servers[0].ID, "slow")
	startAndSettle(t, e, fc, servers)
	fc.mu.Lock()
	fc.roundErrTimes = 2 // first two rounds fail outright
	fc.mu.Unlock()
	res, err := e.CollectNow(servers[0].ID)
	if err == nil {
		t.Fatal("CollectNow with a round error returned no error")
	}
	if res.Interrupted {
		t.Fatal("CollectNow was interrupted")
	}
	// A round error retries every requested kind: 3 rounds total.
	if calls, _ := fc.counts(); calls != 3 {
		t.Fatalf("Collect calls = %d, want 3 rounds (initial + 2 retries)", calls)
	}
	latest, _ := e.Latest(servers[0].ID)
	if _, ok := latest.Get(servercollect.MetricCPU); !ok {
		t.Fatal("no values stored after the round error recovered")
	}
}

func TestEngineRoundTimeoutMarksInterrupted(t *testing.T) {
	fc := newFakeCollector()
	e, assets := testEngine(t, fc, func(cfg *Config) {
		cfg.RoundTimeout = 150 * time.Millisecond
		cfg.RetryInterval = 10 * time.Millisecond
	})
	servers := createServers(t, assets, 1)
	intervals := map[servercollect.MetricKind]time.Duration{}
	for _, kind := range servercollect.AllMetricKinds {
		intervals[kind] = time.Hour
	}
	if err := e.store.SaveTemplate(servercollect.Template{ID: "slow", Name: "Slow", Intervals: intervals}); err != nil {
		t.Fatalf("SaveTemplate: %v", err)
	}
	e.SetServerTemplate(servers[0].ID, "slow")
	startAndSettle(t, e, fc, servers)
	before, _ := e.Latest(servers[0].ID)
	beforeCPU, _ := before.Get(servercollect.MetricCPU)
	fc.setBlock(make(chan struct{})) // never closed: Collect only ends on ctx
	res, _ := e.CollectNow(servers[0].ID)
	if !res.Interrupted {
		t.Fatal("timed-out round did not report Interrupted")
	}
	// A timed-out round must not store anything new: the CPU value must
	// still be exactly the one the settling round stored.
	after, _ := e.Latest(servers[0].ID)
	afterCPU, ok := after.Get(servercollect.MetricCPU)
	if !ok || !afterCPU.CollectedAt.Equal(beforeCPU.CollectedAt) {
		t.Fatalf("interrupted round stored fresh values: before=%v after=%v", beforeCPU.CollectedAt, afterCPU.CollectedAt)
	}
	e.Stop()
}

func TestEngineTemplateFallback(t *testing.T) {
	fc := newFakeCollector()
	e, assets := testEngine(t, fc, nil)
	servers := createServers(t, assets, 1)
	id := servers[0].ID
	e.SetServerTemplate(id, "no-such-template")
	if got := e.TemplateFor(id); got.ID != "default" {
		t.Fatalf("unknown template resolved to %q, want default", got.ID)
	}
	if requested, ok := e.TemplateFallback(id); !ok || requested != "no-such-template" {
		t.Fatalf("fallback not recorded: requested=%q ok=%v", requested, ok)
	}
	if err := e.store.SaveTemplate(servercollect.Template{ID: "fast", Name: "Fast", Intervals: map[servercollect.MetricKind]time.Duration{
		servercollect.MetricCPU: 5 * time.Second,
	}}); err != nil {
		t.Fatalf("SaveTemplate: %v", err)
	}
	e.SetServerTemplate(id, "fast")
	if got := e.TemplateFor(id); got.ID != "fast" || got.Interval(servercollect.MetricCPU) != 5*time.Second {
		t.Fatalf("assigned template not effective: %+v", got)
	}
	if _, ok := e.TemplateFallback(id); ok {
		t.Fatal("fallback still recorded after a valid assignment")
	}
}

func TestEnginePauseResumeNoCatchUp(t *testing.T) {
	fc := newFakeCollector()
	e, assets := testEngine(t, fc, nil)
	servers := createServers(t, assets, 1)
	intervals := map[servercollect.MetricKind]time.Duration{}
	for _, kind := range servercollect.AllMetricKinds {
		intervals[kind] = time.Hour
	}
	intervals[servercollect.MetricCPU] = 200 * time.Millisecond
	if err := e.store.SaveTemplate(servercollect.Template{ID: "tick", Name: "Tick", Intervals: intervals}); err != nil {
		t.Fatalf("SaveTemplate: %v", err)
	}
	e.SetServerTemplate(servers[0].ID, "tick")
	startAndSettle(t, e, fc, servers)
	// CPU is due every 200ms; wait for scheduled runs to happen.
	waitFor(t, "scheduled CPU runs", 5*time.Second, func() bool {
		return fc.callsFor(servercollect.MetricCPU) >= 2
	})
	e.Pause()
	pausedCount := fc.callsFor(servercollect.MetricCPU)
	time.Sleep(500 * time.Millisecond)
	if got := fc.callsFor(servercollect.MetricCPU); got != pausedCount {
		t.Fatalf("CPU ran while paused: %d calls, want %d", got, pausedCount)
	}
	// Pretend the machine slept: the CPU kind is long overdue.
	e.mu.Lock()
	e.nextDue[servers[0].ID][servercollect.MetricCPU] = time.Now().Add(-20 * time.Minute)
	e.mu.Unlock()
	e.Resume()
	// No catch-up: nothing runs inside the first 100ms (next run is
	// now + 200ms), but scheduling does resume afterwards.
	time.Sleep(100 * time.Millisecond)
	if got := fc.callsFor(servercollect.MetricCPU); got != pausedCount {
		t.Fatalf("overdue work was caught up on resume: %d calls, want %d", got, pausedCount)
	}
	waitFor(t, "scheduling to resume", 5*time.Second, func() bool {
		return fc.callsFor(servercollect.MetricCPU) > pausedCount
	})
	e.Stop()
}

func TestEngineGapDetection(t *testing.T) {
	fc := newFakeCollector()
	e, assets := testEngine(t, fc, nil)
	servers := createServers(t, assets, 1)
	// White-box: arm the engine as running with a stale tick and an
	// overdue due-time, then run one tick by hand (no loop goroutine).
	e.mu.Lock()
	e.running = true
	e.ctx, e.cancel = context.WithCancel(context.Background())
	e.inflightCtx, e.inflightCancel = context.WithCancel(e.ctx)
	e.lastTick = time.Now().Add(-20 * time.Minute)
	e.nextDue[servers[0].ID] = map[servercollect.MetricKind]time.Time{
		servercollect.MetricCPU: time.Now().Add(-20 * time.Minute),
	}
	e.mu.Unlock()
	defer e.Stop()
	e.tick(time.Now())
	e.mu.Lock()
	due := e.nextDue[servers[0].ID][servercollect.MetricCPU]
	e.mu.Unlock()
	if !due.After(time.Now()) {
		t.Fatalf("gap tick did not reschedule the overdue kind: next due %v", due)
	}
	if calls, _ := fc.counts(); calls != 0 {
		t.Fatalf("gap tick ran the overdue job instead of rescheduling: %d calls", calls)
	}
}

func TestEngineDialBackoff(t *testing.T) {
	fc := newFakeCollector()
	stub := &dialStub{fail: true}
	e, assets := testEngine(t, fc, func(cfg *Config) { cfg.Dial = stub.dial })
	servers := createServers(t, assets, 1)
	ctx := context.Background()
	if _, err := e.ensureConn(ctx, servers[0], nil, false); err == nil {
		t.Fatal("failing dial returned no error")
	}
	if got := stub.count(); got != 1 {
		t.Fatalf("dial calls = %d, want 1", got)
	}
	// While in backoff the next attempt fails without dialling.
	if _, err := e.ensureConn(ctx, servers[0], nil, false); err == nil {
		t.Fatal("backed-off dial returned no error")
	}
	if got := stub.count(); got != 1 {
		t.Fatalf("dial calls during backoff = %d, want 1 (no redial)", got)
	}
	// A manual trigger bypasses backoff and dials again.
	if _, err := e.ensureConn(ctx, servers[0], nil, true); err == nil {
		t.Fatal("bypassing dial returned no error")
	}
	if got := stub.count(); got != 2 {
		t.Fatalf("dial calls after bypass = %d, want 2", got)
	}
	// Once the backoff window has passed, dialling resumes.
	e.mu.Lock()
	e.conns[servers[0].ID].nextDialAt = time.Now().Add(-time.Second)
	e.mu.Unlock()
	stub.mu.Lock()
	stub.fail = false
	stub.mu.Unlock()
	if _, err := e.ensureConn(ctx, servers[0], nil, false); err != nil {
		t.Fatalf("dial after backoff: %v", err)
	}
	if got := stub.count(); got != 3 {
		t.Fatalf("dial calls = %d, want 3", got)
	}
}

func TestEngineCollectNowStoresValues(t *testing.T) {
	fc := newFakeCollector()
	e, assets := testEngine(t, fc, nil)
	servers := createServers(t, assets, 1)
	e.Start()
	defer e.Stop()
	res, err := e.CollectNow(servers[0].ID)
	if err != nil {
		t.Fatalf("CollectNow: %v", err)
	}
	if res.Interrupted {
		t.Fatal("CollectNow was interrupted")
	}
	latest, found := e.Latest(servers[0].ID)
	if !found {
		t.Fatal("CollectNow stored nothing")
	}
	for _, kind := range servercollect.AllMetricKinds {
		if _, ok := latest.Get(kind); !ok {
			t.Fatalf("kind %s missing after CollectNow", kind)
		}
	}
}

func TestBatchQueueReserved(t *testing.T) {
	q := NewBatchQueue()
	id, err := q.Enqueue(BatchJob{ServerID: "srv-1", Command: "uptime"})
	if err != nil || id == "" {
		t.Fatalf("Enqueue: id=%q err=%v", id, err)
	}
	if _, err := q.Enqueue(BatchJob{ServerID: "srv-1"}); err == nil {
		t.Fatal("Enqueue accepted an empty command")
	}
	list := q.List()
	if len(list) != 1 || list[0].Status != BatchJobQueued {
		t.Fatalf("List: %+v", list)
	}
	if !q.Cancel(id) {
		t.Fatal("Cancel returned false for a queued job")
	}
	job, ok := q.Get(id)
	if !ok || job.Status != BatchJobCancelled {
		t.Fatalf("Get after Cancel: %+v ok=%v", job, ok)
	}
	if q.Cancel(id) {
		t.Fatal("Cancel succeeded twice")
	}
}
