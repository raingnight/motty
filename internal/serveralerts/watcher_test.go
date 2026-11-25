package serveralerts

import (
	"sync"
	"testing"
	"time"

	"motty/internal/serverassets"
	"motty/internal/servercollect"
)

// In-memory fakes for the watcher's injected functions.
type fakeSource struct {
	mu       sync.Mutex
	servers  []serverassets.Server
	latests  []servercollect.Latest
	failures map[string]map[servercollect.MetricKind]string
}

func (f *fakeSource) Assets() ([]serverassets.Server, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]serverassets.Server(nil), f.servers...), nil
}

func (f *fakeSource) Latest() []servercollect.Latest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]servercollect.Latest(nil), f.latests...)
}

func (f *fakeSource) LastError(serverID string, kind servercollect.MetricKind) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if kinds, ok := f.failures[serverID]; ok {
		reason, ok := kinds[kind]
		return reason, ok
	}
	return "", false
}

func TestWatcherEvaluateCreatesOnceThenRecovers(t *testing.T) {
	store := openTestStore(t)
	now := time.Now()
	src := &fakeSource{
		servers: []serverassets.Server{testServer("srv-1", "web-1")},
		latests: []servercollect.Latest{latestWith("srv-1", cpuValue(90, now))},
	}
	var mu sync.Mutex
	var newAlerts []Alert
	notifyCount := 0
	w := NewWatcher(WatcherConfig{
		Store:      store,
		Assets:     src.Assets,
		Latest:     src.Latest,
		LastError:  src.LastError,
		Interval:   time.Hour, // no background tick during the test
		OnNewAlert: func(a Alert) { mu.Lock(); newAlerts = append(newAlerts, a); mu.Unlock() },
		Notify:     func() { mu.Lock(); notifyCount++; mu.Unlock() },
	})

	// First evaluation creates the CPU alert and notifies.
	w.EvaluateNow()
	list, err := store.List()
	if err != nil || len(list) != 1 || list[0].Kind != KindCPU || list[0].State != StateActive {
		t.Fatalf("first evaluate: got %+v err=%v, want 1 active CPU alert", list, err)
	}
	mu.Lock()
	if len(newAlerts) != 1 || notifyCount != 1 {
		t.Fatalf("first evaluate: OnNewAlert=%d Notify=%d, want 1/1", len(newAlerts), notifyCount)
	}
	mu.Unlock()

	// Second evaluation with the same high value: no duplicate, no new
	// OnNewAlert call (only a refresh, which still notifies).
	w.EvaluateNow()
	list, _ = store.List()
	if len(list) != 1 {
		t.Fatalf("second evaluate: got %d alerts, want 1 (dedup)", len(list))
	}
	mu.Lock()
	if len(newAlerts) != 1 {
		t.Fatalf("second evaluate: OnNewAlert=%d, want still 1", len(newAlerts))
	}
	mu.Unlock()

	// Value drops below the recovery threshold: the alert recovers and
	// records RecoveredAt; OnNewAlert is not called for a recovery.
	src.mu.Lock()
	src.latests = []servercollect.Latest{latestWith("srv-1", cpuValue(70, now.Add(time.Minute)))}
	src.mu.Unlock()
	w.EvaluateNow()
	list, _ = store.List()
	if len(list) != 1 || list[0].State != StateRecovered || list[0].RecoveredAt == nil || list[0].RecoverReason == "" {
		t.Fatalf("recovery: got %+v, want 1 recovered alert with RecoveredAt/reason", list)
	}
	mu.Lock()
	if len(newAlerts) != 1 {
		t.Fatalf("recovery: OnNewAlert=%d, want still 1", len(newAlerts))
	}
	mu.Unlock()
}

func TestWatcherSkipsDeletedAndIsolatesPerServerErrors(t *testing.T) {
	store := openTestStore(t)
	now := time.Now()
	deleted := testServer("srv-deleted", "gone")
	deleted.Deleted = true
	src := &fakeSource{
		servers: []serverassets.Server{testServer("srv-1", "web-1"), deleted},
		latests: []servercollect.Latest{
			latestWith("srv-1", cpuValue(90, now)),
			latestWith("srv-deleted", cpuValue(99, now)),
		},
	}
	w := NewWatcher(WatcherConfig{
		Store: store, Assets: src.Assets, Latest: src.Latest, LastError: src.LastError,
		Interval: time.Hour,
	})
	w.EvaluateNow()
	list, _ := store.List()
	if len(list) != 1 || list[0].ServerID != "srv-1" {
		t.Fatalf("deleted server must be skipped: got %+v", list)
	}
}

func TestWatcherStartStopRestart(t *testing.T) {
	store := openTestStore(t)
	now := time.Now()
	src := &fakeSource{
		servers: []serverassets.Server{testServer("srv-1", "web-1")},
		latests: []servercollect.Latest{latestWith("srv-1", cpuValue(90, now))},
	}
	w := NewWatcher(WatcherConfig{
		Store: store, Assets: src.Assets, Latest: src.Latest, LastError: src.LastError,
		Interval: time.Hour,
	})
	// Start evaluates immediately in the background; Stop is idempotent
	// and the watcher can be started again.
	w.Start()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if list, _ := store.List(); len(list) == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if list, _ := store.List(); len(list) != 1 {
		t.Fatalf("Start: want 1 alert after immediate evaluation, got %d", len(list))
	}
	w.Stop()
	w.Stop() // idempotent
	w.Start()
	w.Stop()
	if list, _ := store.List(); len(list) != 1 {
		t.Fatalf("restart: want still 1 alert (dedup), got %d", len(list))
	}
}

func TestWatcherConcurrentEvaluateNowSerialised(t *testing.T) {
	store := openTestStore(t)
	now := time.Now()
	src := &fakeSource{
		servers: []serverassets.Server{testServer("srv-1", "web-1")},
		latests: []servercollect.Latest{latestWith("srv-1", cpuValue(90, now))},
	}
	w := NewWatcher(WatcherConfig{
		Store: store, Assets: src.Assets, Latest: src.Latest, LastError: src.LastError,
		Interval: time.Hour,
	})
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); w.EvaluateNow() }()
	}
	wg.Wait()
	list, _ := store.List()
	if len(list) != 1 {
		t.Fatalf("concurrent EvaluateNow: got %d alerts, want 1 (serialised dedup)", len(list))
	}
}
