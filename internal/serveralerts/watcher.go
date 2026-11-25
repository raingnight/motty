// Background watcher: periodically re-evaluates every server's alerts.
//
// The watcher is deliberately thin: it owns no connections, reads no
// vault and touches no UI state. All inputs arrive through injected
// functions (assets, latest values, per-kind last errors), all outputs
// go through the alert Store plus the OnNewAlert / Notify callbacks,
// which the caller (workbench) wires to the tray notification and the
// UI invalidation. One server's failure never interrupts the others.
package serveralerts

import (
	"sync"
	"time"

	"motty/internal/serverassets"
	"motty/internal/servercollect"
)

// DefaultWatcherInterval is the default evaluation period.
const DefaultWatcherInterval = 60 * time.Second

// PruneRecoveredAfter is how long a recovered alert is kept before the
// watcher prunes it (90 days).
const PruneRecoveredAfter = 90 * 24 * time.Hour

// WatcherConfig assembles a Watcher. Store is required; Assets, Latest
// and LastError are injected so tests can substitute in-memory fakes and
// production can wire the asset store, the scheduler engine and its
// LastError. Interval <= 0 means DefaultWatcherInterval. OnNewAlert is
// called once per newly created alert (never for updates or recoveries);
// Notify is called after an evaluation that changed anything.
type WatcherConfig struct {
	Store      *Store
	Assets     func() ([]serverassets.Server, error)
	Latest     func() []servercollect.Latest
	LastError  func(serverID string, kind servercollect.MetricKind) (string, bool)
	Interval   time.Duration
	OnNewAlert func(Alert)
	Notify     func()
}

// Watcher periodically evaluates alerts for all servers. The zero value
// is not usable; use NewWatcher.
type Watcher struct {
	cfg      WatcherConfig
	interval time.Duration

	mu      sync.Mutex // guards running / stopCh
	evalMu  sync.Mutex // serialises evaluations; concurrent EvaluateNow calls queue
	running bool
	stopCh  chan struct{}
	wg      sync.WaitGroup
}

// NewWatcher builds a Watcher. It panics on a nil Store: without a store
// no evaluation result can be persisted.
func NewWatcher(cfg WatcherConfig) *Watcher {
	if cfg.Store == nil {
		panic("serveralerts: nil Store")
	}
	interval := cfg.Interval
	if interval <= 0 {
		interval = DefaultWatcherInterval
	}
	return &Watcher{cfg: cfg, interval: interval}
}

// Start begins background evaluation. It is idempotent and may be called
// again after Stop. The first evaluation runs immediately (in the
// background, so Start never blocks on it); later ones run every
// Interval.
func (w *Watcher) Start() {
	w.mu.Lock()
	if w.running {
		w.mu.Unlock()
		return
	}
	w.running = true
	w.stopCh = make(chan struct{})
	w.mu.Unlock()
	w.wg.Add(1)
	go w.loop()
}

// Stop ends background evaluation and waits for the in-flight evaluation
// (if any) to finish. It is idempotent.
func (w *Watcher) Stop() {
	w.mu.Lock()
	if !w.running {
		w.mu.Unlock()
		return
	}
	w.running = false
	close(w.stopCh)
	w.mu.Unlock()
	w.wg.Wait()
}

// EvaluateNow triggers one evaluation immediately. Concurrent calls are
// serialised (never run re-entrantly); a call waits for the evaluation
// in progress to finish and then runs its own. Callbacks (OnNewAlert,
// Notify) are invoked after the evaluation lock is released, so a
// callback that calls EvaluateNow cannot deadlock.
func (w *Watcher) EvaluateNow() {
	w.evalMu.Lock()
	created, changed := w.evaluate()
	w.evalMu.Unlock()
	w.dispatch(created, changed)
}

func (w *Watcher) loop() {
	defer w.wg.Done()
	w.EvaluateNow()
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-w.stopCh:
			return
		case <-ticker.C:
			w.EvaluateNow()
		}
	}
}

// dispatch invokes the callbacks for one finished evaluation.
func (w *Watcher) dispatch(created []Alert, changed bool) {
	if w.cfg.OnNewAlert != nil {
		for _, alert := range created {
			w.cfg.OnNewAlert(alert)
		}
	}
	if changed && w.cfg.Notify != nil {
		w.cfg.Notify()
	}
}

// evaluate runs one pass over all servers and persists the changes. It
// returns the newly created alerts (already saved) and whether anything
// changed. The caller must hold w.evalMu. A failure to load the asset
// list or the stored alerts aborts the pass (evaluating without the
// stored alerts could duplicate them); a failure to load latest values
// is treated as "no fresh values", which can neither trigger nor recover
// anything, so the pass still runs. A failure for one server — loading
// its last errors, or saving one of its alerts — never interrupts the
// other servers.
func (w *Watcher) evaluate() (created []Alert, changed bool) {
	if w.cfg.Assets == nil {
		return nil, false
	}
	servers, err := w.cfg.Assets()
	if err != nil {
		return nil, false
	}
	latestByID := map[string]servercollect.Latest{}
	if w.cfg.Latest != nil {
		for _, lat := range w.cfg.Latest() {
			latestByID[lat.ServerID] = lat
		}
	}
	stored, err := w.cfg.Store.List()
	if err != nil {
		return nil, false
	}
	prevByServer := map[string][]Alert{}
	for _, alert := range stored {
		prevByServer[alert.ServerID] = append(prevByServer[alert.ServerID], alert)
	}
	now := time.Now().UTC()
	for _, server := range servers {
		if server.Deleted {
			continue
		}
		failures := map[servercollect.MetricKind]string{}
		if w.cfg.LastError != nil {
			for _, kind := range servercollect.AllMetricKinds {
				if reason, ok := w.cfg.LastError(server.ID, kind); ok && reason != "" {
					failures[kind] = reason
				}
			}
		}
		latest, hasLatest := latestByID[server.ID]
		made, refreshed, recovered := Evaluate(server, latest, hasLatest, failures, prevByServer[server.ID], now)
		for _, alert := range made {
			if err := w.cfg.Store.Save(alert); err == nil {
				created = append(created, alert)
				changed = true
			}
		}
		for _, alert := range refreshed {
			if err := w.cfg.Store.Save(alert); err == nil {
				changed = true
			}
		}
		for _, alert := range recovered {
			if err := w.cfg.Store.Save(alert); err == nil {
				changed = true
			}
		}
	}
	// Recovered alerts older than 90 days are pruned after every pass;
	// a prune failure is not an evaluation failure.
	_ = w.cfg.Store.PruneRecovered(PruneRecoveredAfter)
	return created, changed
}
