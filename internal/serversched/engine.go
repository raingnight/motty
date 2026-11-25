// Engine: the collection scheduler's core — connection cache, round
// execution with retries, and the public Engine API.
//
// Concurrency model: the scheduler loop (scheduler.go) enqueues one job per
// due (server, kind) pair in FIFO order; a dispatcher starts jobs in queue
// order, gated by a semaphore of MaxConcurrency (default 20) collection
// rounds. One server's failure never affects another server's job, and a
// job for a (server, kind) pair is never started while another job for the
// same pair is queued or running (per-server per-kind in-flight marks).
// Collection always runs on the engine's own goroutines, never on a UI
// goroutine; CollectNow blocks its caller, so callers must invoke it from
// a background goroutine, not the UI thread.
//
// Connection model: one cached SSH connection per server, dialled with
// serverconn.Dial (the same dial the connection test uses) and owned solely
// by this engine — terminal sessions open their own connections and never
// share this one. A cached connection is dropped when Dial fails, when its
// Status leaves PhaseConnected, or when a round ends with a round-level
// error; the next round redials. Redial is backed off (first retry after
// 30s, then 2m, then 5m capped) so an offline server is not hammered with
// a handshake every minute.
//
// Round model: one round carries a single context with the whole-round
// budget (default 60s, covering retries); each item's 15s budget is
// enforced inside the Collector. A failed round retries at most MaxRetries
// (default 2) times, only the failed kinds, with RetryInterval (default
// 5s) between attempts. A round cut short by its context is reported with
// RoundResult.Interrupted=true and its partial successes are still stored.
package serversched

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"motty/internal/configstore"
	"motty/internal/serverassets"
	"motty/internal/servercollect"
	"motty/internal/serverconn"
	"motty/internal/sshconn"
)

// Defaults from Motty_Linux.md §7.
const (
	// DefaultMaxConcurrency is the default cap on simultaneous collection rounds.
	DefaultMaxConcurrency = 20
	// DefaultRoundTimeout bounds one whole round, retries included.
	DefaultRoundTimeout = 60 * time.Second
	// DefaultRetryInterval is the wait between retry attempts.
	DefaultRetryInterval = 5 * time.Second
	// DefaultMaxRetries is how many times a failed item is retried.
	DefaultMaxRetries = 2
	// DefaultTickInterval is how often the scheduler looks for due work.
	DefaultTickInterval = time.Second
	// DefaultGapThreshold is how late a tick (or a due time) may be before
	// the engine treats it as a sleep/wake gap and reschedules without
	// catching up.
	DefaultGapThreshold = 10 * time.Minute
	// DefaultFlushInterval is how often buffered latest values are
	// written to disk. A crash loses at most this long of unflushed
	// values; after a restart those kinds read as uncollected or
	// expired, never as zeroes.
	DefaultFlushInterval = 10 * time.Second
)

var (
	// ErrNotRunning means the engine is not started.
	ErrNotRunning = errors.New("serversched: engine is not running")
	// ErrPaused means the engine is paused.
	ErrPaused = errors.New("serversched: engine is paused")
	// ErrBusy means a collection round for that server is already running.
	ErrBusy = errors.New("serversched: collection already in progress for this server")
	// ErrStopped means the engine stopped while a manual round was waiting.
	ErrStopped = errors.New("serversched: engine stopped")
)

// DialFunc dials one server the way serverconn.Dial does. It is a
// Config field so tests can substitute a stub; production code passes nil
// and gets serverconn.Dial.
type DialFunc func(ctx context.Context, server serverassets.Server, jump *serverassets.JumpHost, creds serverconn.CredentialProvider, knownHostsPath string) (*sshconn.Conn, serverconn.TestResult)

// Config assembles an Engine. Store, Assets and Collector are required;
// everything else has a default. Nothing here reads the vault or a config
// file: the caller injects the already-opened store, the credential
// provider and the known-hosts path.
type Config struct {
	Store          *configstore.Store
	Assets         *serverassets.Store
	Creds          serverconn.CredentialProvider
	KnownHostsPath string
	Collector      servercollect.Collector
	// Notify, when set, is called after new data or a state change, so the
	// UI can refresh. It is always called without holding the engine lock.
	Notify func()
	// Dial overrides the dial function (nil means serverconn.Dial).
	Dial DialFunc
	// MaxConcurrency <= 0 means DefaultMaxConcurrency.
	MaxConcurrency int
	// RoundTimeout <= 0 means DefaultRoundTimeout.
	RoundTimeout time.Duration
	// RetryInterval <= 0 means DefaultRetryInterval.
	RetryInterval time.Duration
	// MaxRetries <= 0 means DefaultMaxRetries.
	MaxRetries int
	// TickInterval <= 0 means DefaultTickInterval.
	TickInterval time.Duration
	// GapThreshold <= 0 means DefaultGapThreshold.
	GapThreshold time.Duration
	// FlushInterval <= 0 means DefaultFlushInterval: how often buffered
	// latest values are flushed to disk.
	FlushInterval time.Duration
}

// job is one unit of collection work: one kind (scheduled) or all kinds
// (manual CollectNow). resultCh is set only for manual jobs; the worker
// sends the finished round on it exactly once.
type job struct {
	serverID      string
	kinds         []servercollect.MetricKind
	manual        bool
	bypassBackoff bool
	resultCh      chan servercollect.RoundResult
}

// cachedConn is one server's cached connection plus its redial backoff
// state. conn may be nil when a test dial reports success without a live
// connection; connected records the dial outcome, not the pointer.
type cachedConn struct {
	conn       *sshconn.Conn
	connected  bool
	failures   int
	nextDialAt time.Time
}

// backoffDelay returns how long to wait before redialling after failures
// consecutive dial failures: 30s, then 2m, then 5m capped.
func backoffDelay(failures int) time.Duration {
	switch {
	case failures <= 0:
		return 0
	case failures == 1:
		return 30 * time.Second
	case failures == 2:
		return 2 * time.Minute
	default:
		return 5 * time.Minute
	}
}

// Engine schedules collection rounds and owns the per-server connections.
// The zero value is not usable; use New.
type Engine struct {
	store          *Store
	assets         *serverassets.Store
	collector      servercollect.Collector
	creds          serverconn.CredentialProvider
	knownHostsPath string
	notify         func()
	dial           DialFunc
	maxConc        int
	roundTimeout   time.Duration
	retryInterval  time.Duration
	maxRetries     int
	tickInterval   time.Duration
	gapThreshold   time.Duration
	flushInterval  time.Duration
	batch          *BatchQueue

	mu             sync.Mutex
	queueCond      *sync.Cond
	running        bool
	paused         bool
	ctx            context.Context
	cancel         context.CancelFunc
	inflightCtx    context.Context
	inflightCancel context.CancelFunc
	nextDue        map[string]map[servercollect.MetricKind]time.Time
	inFlight       map[string]map[servercollect.MetricKind]bool
	conns          map[string]*cachedConn
	dialLocks      map[string]*sync.Mutex
	templates      map[string]string // serverID -> template ID ("default" when unset)
	fallback       map[string]string // serverID -> requested template ID that fell back to default
	lastErr        map[string]map[servercollect.MetricKind]string
	queue          []*job
	sem            chan struct{}
	wg             sync.WaitGroup // scheduler loop + dispatcher
	workers        sync.WaitGroup // collection workers
	lastTick       time.Time
}

// New builds an Engine. It panics on a nil Store, Assets or Collector:
// without any of those no collection can succeed.
func New(cfg Config) *Engine {
	if cfg.Store == nil {
		panic("serversched: nil configstore.Store")
	}
	if cfg.Assets == nil {
		panic("serversched: nil serverassets.Store")
	}
	if cfg.Collector == nil {
		panic("serversched: nil servercollect.Collector")
	}
	dial := cfg.Dial
	if dial == nil {
		dial = serverconn.Dial
	}
	maxConc := cfg.MaxConcurrency
	if maxConc <= 0 {
		maxConc = DefaultMaxConcurrency
	}
	roundTimeout := cfg.RoundTimeout
	if roundTimeout <= 0 {
		roundTimeout = DefaultRoundTimeout
	}
	retryInterval := cfg.RetryInterval
	if retryInterval <= 0 {
		retryInterval = DefaultRetryInterval
	}
	maxRetries := cfg.MaxRetries
	if maxRetries <= 0 {
		maxRetries = DefaultMaxRetries
	}
	tickInterval := cfg.TickInterval
	if tickInterval <= 0 {
		tickInterval = DefaultTickInterval
	}
	gapThreshold := cfg.GapThreshold
	if gapThreshold <= 0 {
		gapThreshold = DefaultGapThreshold
	}
	flushInterval := cfg.FlushInterval
	if flushInterval <= 0 {
		flushInterval = DefaultFlushInterval
	}
	e := &Engine{
		store:          NewStore(cfg.Store),
		assets:         cfg.Assets,
		collector:      cfg.Collector,
		creds:          cfg.Creds,
		knownHostsPath: cfg.KnownHostsPath,
		notify:         cfg.Notify,
		dial:           dial,
		maxConc:        maxConc,
		roundTimeout:   roundTimeout,
		retryInterval:  retryInterval,
		maxRetries:     maxRetries,
		tickInterval:   tickInterval,
		gapThreshold:   gapThreshold,
		flushInterval:  flushInterval,
		batch:          NewBatchQueue(),
		nextDue:        map[string]map[servercollect.MetricKind]time.Time{},
		inFlight:       map[string]map[servercollect.MetricKind]bool{},
		conns:          map[string]*cachedConn{},
		dialLocks:      map[string]*sync.Mutex{},
		templates:      map[string]string{},
		fallback:       map[string]string{},
		lastErr:        map[string]map[servercollect.MetricKind]string{},
		sem:            make(chan struct{}, maxConc),
	}
	e.queueCond = sync.NewCond(&e.mu)
	return e
}

// Start begins scheduling. It is idempotent. The first tick collects every
// kind for every known server immediately, so baselines are established
// without waiting a full interval. Stored template assignments are loaded
// first, so a restart resumes each server's assigned template, and a
// flusher goroutine is started to write buffered values to disk every
// FlushInterval.
func (e *Engine) Start() {
	e.mu.Lock()
	if e.running {
		e.mu.Unlock()
		return
	}
	e.ctx, e.cancel = context.WithCancel(context.Background())
	e.inflightCtx, e.inflightCancel = context.WithCancel(e.ctx)
	e.running = true
	e.paused = false
	e.lastTick = time.Time{}
	e.nextDue = map[string]map[servercollect.MetricKind]time.Time{}
	e.queue = nil
	e.mu.Unlock()
	e.loadAssignments()
	e.wg.Add(3)
	go e.loop()
	go e.dispatcher()
	go e.flusher()
}

// loadAssignments replaces the in-memory template map with the stored
// assignments. A load failure keeps the current map: the engine still
// collects, with the default template for unassigned servers, and the
// next Start retries the load.
func (e *Engine) loadAssignments() {
	assigns, err := e.store.ListTemplateAssignments()
	if err != nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.templates = assigns
	e.fallback = map[string]string{}
}

// flusher writes buffered latest values to disk every flushInterval. A
// failed flush keeps the dirty marks, so the next tick retries the same
// values. When the engine stops it flushes once more, so values buffered
// by rounds finishing as the engine stops still reach disk.
func (e *Engine) flusher() {
	defer e.wg.Done()
	ticker := time.NewTicker(e.flushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-e.ctx.Done():
			_ = e.store.Flush()
			return
		case <-ticker.C:
			_ = e.store.Flush()
		}
	}
}

// Stop ends scheduling, cancels in-flight rounds (they finish as
// Interrupted), waits for workers, and closes every cached connection.
// Buffered values are flushed to disk before it returns, so a clean
// stop loses nothing. It is idempotent; a stopped engine can be started
// again with Start.
func (e *Engine) Stop() {
	e.mu.Lock()
	if !e.running {
		e.mu.Unlock()
		return
	}
	e.running = false
	e.paused = false
	if e.cancel != nil {
		e.cancel()
	}
	if e.inflightCancel != nil {
		e.inflightCancel()
	}
	e.queueCond.Broadcast()
	e.mu.Unlock()
	e.wg.Wait()
	e.workers.Wait()
	// Rounds finishing as the engine stopped may have buffered values
	// after the flusher's final flush; write those too.
	_ = e.store.Flush()
	e.dropAllConns()
}

// Pause stops new collection from starting. Rounds already running are
// cancelled through their context and finish as Interrupted; queued jobs
// that have not started are dropped. Buffered values are flushed first,
// so a sleep or lock loses nothing already collected. Use Resume to
// restart scheduling. The workbench calls Pause on sleep/lock and
// Resume on wake; see scheduler.go for the calling convention.
func (e *Engine) Pause() {
	_ = e.store.Flush()
	e.mu.Lock()
	if !e.running || e.paused {
		e.mu.Unlock()
		return
	}
	e.paused = true
	e.dropQueuedLocked(ErrPaused)
	cancel := e.inflightCancel
	e.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	e.notifySafe()
}

// Resume restarts scheduling after a Pause (or starts the engine when it
// was never started). Each kind's next run is recomputed as now plus its
// interval — work that came due during the pause is not caught up — and a
// lightweight connectivity probe is started for servers that are not in
// dial backoff (see probeConnectivity). Resume on a running, unpaused
// engine is a no-op.
func (e *Engine) Resume() {
	e.mu.Lock()
	if !e.running {
		e.mu.Unlock()
		e.Start()
		return
	}
	if !e.paused {
		e.mu.Unlock()
		return
	}
	e.paused = false
	e.wakeLocked(time.Now())
	e.mu.Unlock()
	e.notifySafe()
	go e.probeConnectivity()
}

// CollectNow manually triggers one round for serverID covering all kinds.
// It blocks until the round finishes (or the engine stops), so it must be
// called from a background goroutine, never the UI thread. Kinds already
// being collected for that server make it fail with ErrBusy; a paused
// engine fails with ErrPaused. The returned RoundResult carries the
// round-level error, if any; per-kind failures are in its Values as
// Unavailable entries, and previously stored successes are untouched.
func (e *Engine) CollectNow(serverID string) (servercollect.RoundResult, error) {
	server, err := e.assets.GetServer(serverID)
	if err != nil {
		return servercollect.RoundResult{}, err
	}
	if server.Deleted {
		return servercollect.RoundResult{}, fmt.Errorf("%w: server %s is deleted", ErrNotFound, serverID)
	}
	e.mu.Lock()
	if !e.running {
		e.mu.Unlock()
		return servercollect.RoundResult{}, ErrNotRunning
	}
	if e.paused {
		e.mu.Unlock()
		return servercollect.RoundResult{}, ErrPaused
	}
	if len(e.inFlight[serverID]) > 0 {
		e.mu.Unlock()
		return servercollect.RoundResult{}, ErrBusy
	}
	kinds := append([]servercollect.MetricKind(nil), servercollect.AllMetricKinds...)
	for _, kind := range kinds {
		e.markInFlightLocked(serverID, kind)
	}
	j := &job{
		serverID:      serverID,
		kinds:         kinds,
		manual:        true,
		bypassBackoff: true,
		resultCh:      make(chan servercollect.RoundResult, 1),
	}
	e.queue = append([]*job{j}, e.queue...)
	e.queueCond.Broadcast()
	ctx := e.ctx
	e.mu.Unlock()

	select {
	case res := <-j.resultCh:
		return res, res.Err
	case <-ctx.Done():
		return servercollect.RoundResult{ServerID: serverID, Interrupted: true}, ErrStopped
	}
}

// Latest returns one server's latest stored values.
func (e *Engine) Latest(serverID string) (servercollect.Latest, bool) {
	latest, found, err := e.store.Get(serverID)
	if err != nil {
		return servercollect.Latest{}, false
	}
	return latest, found
}

// AllLatest returns every server's latest stored values, for home-page
// statistics. Expired values are included with their CollectedAt; callers
// must use MetricValue.Expired / Latest.ExpiredKind to exclude them from
// statistics.
func (e *Engine) AllLatest() []servercollect.Latest {
	latest, err := e.store.List()
	if err != nil {
		return nil
	}
	return latest
}

// Templates returns all collection templates, default first.
func (e *Engine) Templates() []servercollect.Template {
	templates, err := e.store.ListTemplates()
	if err != nil {
		return []servercollect.Template{servercollect.DefaultTemplate()}
	}
	return templates
}

// SetServerTemplate chooses which template a server collects with.
// templateID "" means the default. An unknown template ID falls back to
// the default and is recorded (see TemplateFallback). The assignment is
// written to disk first; when that write fails the error is returned
// and the in-memory assignment is left untouched, so a restart never
// disagrees with what the engine used.
func (e *Engine) SetServerTemplate(serverID, templateID string) error {
	templateID = strings.TrimSpace(templateID)
	if templateID == "" {
		templateID = DefaultTemplateID
	}
	_, ok, _ := e.store.GetTemplate(templateID)
	if !ok {
		if err := e.store.DeleteTemplateAssignment(serverID); err != nil {
			return err
		}
		e.mu.Lock()
		defer e.mu.Unlock()
		e.templates[serverID] = DefaultTemplateID
		e.fallback[serverID] = templateID
		return nil
	}
	if err := e.store.SaveTemplateAssignment(serverID, templateID); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.templates[serverID] = templateID
	delete(e.fallback, serverID)
	return nil
}

// TemplateFor returns the effective template for a server: its assigned
// template, or the default when none is assigned or the assigned one no
// longer exists.
func (e *Engine) TemplateFor(serverID string) servercollect.Template {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.templateForLocked(serverID)
}

// TemplateFallback reports the template ID a server asked for that fell
// back to the default, if any.
func (e *Engine) TemplateFallback(serverID string) (requested string, ok bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	requested, ok = e.fallback[serverID]
	return requested, ok
}

// LastError returns the last recorded failure reason for one server and
// kind, if any.
func (e *Engine) LastError(serverID string, kind servercollect.MetricKind) (string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if kinds, ok := e.lastErr[serverID]; ok {
		reason, ok := kinds[kind]
		return reason, ok
	}
	return "", false
}

// BatchQueue returns the reserved batch-command queue. It accepts,
// cancels and lists jobs; execution is not implemented in this phase
// (see scheduler.go).
func (e *Engine) BatchQueue() *BatchQueue { return e.batch }

// templateForLocked resolves a server's effective template. The caller
// must hold e.mu.
func (e *Engine) templateForLocked(serverID string) servercollect.Template {
	byID := map[string]servercollect.Template{}
	for _, tmpl := range e.Templates() {
		byID[tmpl.ID] = tmpl
	}
	return e.effectiveTemplateLocked(serverID, byID)
}

// effectiveTemplateLocked resolves a server's effective template from a
// template map loaded once by the caller. The caller must hold e.mu.
func (e *Engine) effectiveTemplateLocked(serverID string, byID map[string]servercollect.Template) servercollect.Template {
	id := e.templates[serverID]
	if id == "" {
		id = DefaultTemplateID
	}
	if tmpl, ok := byID[id]; ok {
		return tmpl
	}
	if id != DefaultTemplateID {
		e.fallback[serverID] = id
	}
	return servercollect.DefaultTemplate()
}

func (e *Engine) markInFlightLocked(serverID string, kind servercollect.MetricKind) {
	kinds := e.inFlight[serverID]
	if kinds == nil {
		kinds = map[servercollect.MetricKind]bool{}
		e.inFlight[serverID] = kinds
	}
	kinds[kind] = true
}

func (e *Engine) clearInFlightLocked(serverID string, kinds []servercollect.MetricKind) {
	marks := e.inFlight[serverID]
	if marks == nil {
		return
	}
	for _, kind := range kinds {
		delete(marks, kind)
	}
	if len(marks) == 0 {
		delete(e.inFlight, serverID)
	}
}

func (e *Engine) setLastErrLocked(serverID string, kind servercollect.MetricKind, reason string) {
	kinds := e.lastErr[serverID]
	if kinds == nil {
		kinds = map[servercollect.MetricKind]string{}
		e.lastErr[serverID] = kinds
	}
	kinds[kind] = reason
}

func (e *Engine) clearLastErrLocked(serverID string, kind servercollect.MetricKind) {
	if kinds, ok := e.lastErr[serverID]; ok {
		delete(kinds, kind)
		if len(kinds) == 0 {
			delete(e.lastErr, serverID)
		}
	}
}

func (e *Engine) notifySafe() {
	if e.notify != nil {
		e.notify()
	}
}

// --- connections ---

func (e *Engine) dialLockFor(serverID string) *sync.Mutex {
	e.mu.Lock()
	defer e.mu.Unlock()
	lock, ok := e.dialLocks[serverID]
	if !ok {
		lock = &sync.Mutex{}
		e.dialLocks[serverID] = lock
	}
	return lock
}

// ensureConn returns the cached connection for a server, dialling when
// there is none or the cached one is no longer connected. Dial failures
// are recorded with backoff; while in backoff (and bypass is false) the
// call fails immediately without dialling, so an offline server does not
// get a handshake on every round. A nil connection with a nil error is a
// successful dial that produced no live connection (only a test dial does
// this); the collector receives it as-is.
func (e *Engine) ensureConn(ctx context.Context, server serverassets.Server, jump *serverassets.JumpHost, bypass bool) (*sshconn.Conn, error) {
	lock := e.dialLockFor(server.ID)
	lock.Lock()
	defer lock.Unlock()

	now := time.Now()
	e.mu.Lock()
	cc := e.conns[server.ID]
	if cc != nil && !bypass && !cc.nextDialAt.IsZero() && now.Before(cc.nextDialAt) {
		e.mu.Unlock()
		return nil, fmt.Errorf("serversched: server %s is in dial backoff until %s", server.ID, cc.nextDialAt.Format(time.RFC3339))
	}
	failures := 0
	if cc != nil {
		failures = cc.failures
	}
	if cc != nil && cc.connected {
		conn := cc.conn
		e.mu.Unlock()
		if conn != nil && conn.Status().Phase != sshconn.PhaseConnected {
			e.dropConn(server.ID)
			// Fall through and redial; failures keeps the count from
			// the dropped entry, so a broken connection does not reset
			// the backoff ladder.
		} else {
			return conn, nil
		}
	} else {
		e.mu.Unlock()
	}

	conn, result := e.dial(ctx, server, jump, e.creds, e.knownHostsPath)
	if !result.OK {
		failures++
		delay := backoffDelay(failures)
		e.mu.Lock()
		e.conns[server.ID] = &cachedConn{failures: failures, nextDialAt: now.Add(delay)}
		e.mu.Unlock()
		if result.Error != nil {
			return nil, fmt.Errorf("serversched: dial server %s failed (%s): %w", server.ID, result.Kind, result.Error)
		}
		return nil, fmt.Errorf("serversched: dial server %s failed (%s)", server.ID, result.Kind)
	}
	e.mu.Lock()
	e.conns[server.ID] = &cachedConn{conn: conn, connected: true}
	e.mu.Unlock()
	return conn, nil
}

// dropConn discards one server's cached connection, closing it. It never
// blocks on the close: Cancel is synchronous and the wait is done in a
// background goroutine, so a worker holding the semaphore is not stalled.
func (e *Engine) dropConn(serverID string) {
	e.mu.Lock()
	cc := e.conns[serverID]
	delete(e.conns, serverID)
	e.mu.Unlock()
	if cc != nil && cc.conn != nil {
		cc.conn.Cancel()
		go cc.conn.Close()
	}
}

func (e *Engine) dropAllConns() {
	e.mu.Lock()
	conns := e.conns
	e.conns = map[string]*cachedConn{}
	e.mu.Unlock()
	for _, cc := range conns {
		if cc != nil && cc.conn != nil {
			cc.conn.Cancel()
			go cc.conn.Close()
		}
	}
}

// --- rounds ---

// executeJob runs one job end to end and reports whether anything changed
// (new data stored, a failure recorded, or an interruption), for notify.
func (e *Engine) executeJob(j *job) (servercollect.RoundResult, bool) {
	server, err := e.assets.GetServer(j.serverID)
	if err != nil {
		return servercollect.RoundResult{ServerID: j.serverID, Err: err}, false
	}
	if server.Deleted {
		e.dropConn(j.serverID)
		return servercollect.RoundResult{ServerID: j.serverID, Err: fmt.Errorf("%w: server %s is deleted", ErrNotFound, j.serverID)}, false
	}
	var jump *serverassets.JumpHost
	if server.JumpHostID != "" {
		jh, err := e.assets.GetJumpHost(server.JumpHostID)
		if err != nil {
			e.recordFailure(j.serverID, j.kinds, err.Error())
			return servercollect.RoundResult{ServerID: j.serverID, Err: err}, true
		}
		jump = &jh
	}

	e.mu.Lock()
	inflight := e.inflightCtx
	e.mu.Unlock()
	if inflight == nil {
		return servercollect.RoundResult{ServerID: j.serverID, Interrupted: true, Err: ErrNotRunning}, true
	}
	conn, err := e.ensureConn(inflight, server, jump, j.bypassBackoff)
	if err != nil {
		e.recordFailure(j.serverID, j.kinds, err.Error())
		return servercollect.RoundResult{ServerID: j.serverID, Err: err}, true
	}
	roundCtx, cancel := context.WithTimeout(inflight, e.roundTimeout)
	defer cancel()
	res, changed := e.runRound(roundCtx, server, jump, conn, j.kinds)
	if res.Err != nil || (conn != nil && conn.Status().Phase != sshconn.PhaseConnected) {
		e.dropConn(j.serverID)
	}
	return res, changed
}

// recordFailure records the same reason for every kind in kinds.
func (e *Engine) recordFailure(serverID string, kinds []servercollect.MetricKind, reason string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, kind := range kinds {
		e.setLastErrLocked(serverID, kind, reason)
	}
}

// runRound collects kinds for one server, retrying only failed kinds.
// ctx carries the whole-round budget. Successful values are buffered as
// soon as they arrive — the flusher writes them to disk, and a manual
// round flushes them at once in runJob — so a later retry or a timeout
// never loses them.
func (e *Engine) runRound(ctx context.Context, server serverassets.Server, jump *serverassets.JumpHost, conn *sshconn.Conn, kinds []servercollect.MetricKind) (servercollect.RoundResult, bool) {
	started := time.Now()
	remaining := append([]servercollect.MetricKind(nil), kinds...)
	merged := map[servercollect.MetricKind]servercollect.MetricValue{}
	var lastErr error
	interrupted := false
	changed := false
	attempt := 0
	for {
		res, err := e.collector.Collect(ctx, conn, server, remaining)
		if err != nil {
			lastErr = err
		} else if res.Err != nil {
			lastErr = res.Err
		}
		if res.Interrupted {
			interrupted = true
		}
		var successes []servercollect.MetricValue
		for _, v := range res.Values {
			if v.Unavailable != "" {
				merged[v.Kind] = v
				continue
			}
			if v.Kind == "" || v.CollectedAt.IsZero() {
				continue
			}
			merged[v.Kind] = v
			successes = append(successes, v)
		}
		if len(successes) > 0 {
			if perr := e.store.PutBuffered(server.ID, successes); perr == nil {
				changed = true
				e.mu.Lock()
				for _, v := range successes {
					e.clearLastErrLocked(server.ID, v.Kind)
				}
				e.mu.Unlock()
			}
		}
		failed := failedKinds(res, remaining, err)
		if len(failed) == 0 {
			break
		}
		if ctx.Err() != nil {
			interrupted = true
			e.recordFailure(server.ID, failed, ctx.Err().Error())
			changed = true
			break
		}
		if attempt >= e.maxRetries {
			reason := "collection failed"
			if lastErr != nil {
				reason = lastErr.Error()
			}
			for _, kind := range failed {
				if v, ok := merged[kind]; ok && v.Unavailable != "" {
					reason = v.Unavailable
					break
				}
			}
			e.recordFailure(server.ID, failed, reason)
			changed = true
			break
		}
		// Wait out the retry interval, or stop at once if the round ends.
		timer := time.NewTimer(e.retryInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			interrupted = true
			e.recordFailure(server.ID, failed, ctx.Err().Error())
			changed = true
			goto done
		case <-timer.C:
		}
		attempt++
		remaining = failed
		// A dead connection must not be retried on: refresh it first.
		if conn != nil && conn.Status().Phase != sshconn.PhaseConnected {
			fresh, derr := e.ensureConn(ctx, server, jump, true)
			if derr != nil {
				e.recordFailure(server.ID, failed, derr.Error())
				changed = true
				break
			}
			conn = fresh
		}
	}
done:
	values := make([]servercollect.MetricValue, 0, len(kinds))
	for _, kind := range kinds {
		if v, ok := merged[kind]; ok {
			values = append(values, v)
		}
	}
	if ctx.Err() != nil {
		interrupted = true
	}
	return servercollect.RoundResult{
		ServerID:    server.ID,
		StartedAt:   started,
		FinishedAt:  time.Now(),
		Values:      values,
		Interrupted: interrupted,
		Err:         lastErr,
	}, changed
}

// failedKinds lists the requested kinds a round did not deliver. A
// round-level error fails every requested kind, per spec §7.3.
func failedKinds(res servercollect.RoundResult, requested []servercollect.MetricKind, err error) []servercollect.MetricKind {
	if err != nil || res.Err != nil {
		return append([]servercollect.MetricKind(nil), requested...)
	}
	byKind := map[servercollect.MetricKind]servercollect.MetricValue{}
	for _, v := range res.Values {
		byKind[v.Kind] = v
	}
	var failed []servercollect.MetricKind
	for _, kind := range requested {
		v, ok := byKind[kind]
		if !ok || v.Unavailable != "" {
			failed = append(failed, kind)
		}
	}
	return failed
}

// runJob executes one job, clears its in-flight marks, reports the round
// to a manual caller, and notifies on change. It runs on a worker
// goroutine, never holding the engine lock while collecting. A manual
// round is user-initiated, so its buffered values are flushed to disk at
// once instead of waiting for the next flush tick.
func (e *Engine) runJob(j *job) {
	res, changed := e.executeJob(j)
	e.mu.Lock()
	e.clearInFlightLocked(j.serverID, j.kinds)
	e.mu.Unlock()
	if j.manual {
		_ = e.store.Flush()
	}
	if j.resultCh != nil {
		j.resultCh <- res
	}
	if changed {
		e.notifySafe()
	}
}

// dropJob abandons a job that will never run (engine stopping): it clears
// the in-flight marks and reports the round as interrupted to a manual
// caller. Exactly one of dropJob and runJob reports a job.
func (e *Engine) dropJob(j *job, err error) {
	e.mu.Lock()
	e.clearInFlightLocked(j.serverID, j.kinds)
	e.mu.Unlock()
	if j.resultCh != nil {
		j.resultCh <- servercollect.RoundResult{ServerID: j.serverID, Interrupted: true, Err: err}
	}
}
