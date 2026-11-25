// Scheduler loop, dispatcher, sleep/wake handling, and the reserved
// batch-command queue.
//
// Calling convention for sleep/wake (the workbench wires this up in a
// later step; this package only provides the mechanism):
//
//   - On sleep, lock, or shutdown preparation: Engine.Pause(). It stops
//     new rounds from starting and cancels rounds already running; those
//     rounds finish as Interrupted and are never recorded as successful.
//   - On wake or unlock: Engine.Resume(). It recomputes every kind's next
//     run as now plus its interval, so work that came due during the sleep
//     is not caught up, and starts a lightweight connectivity probe (see
//     probeConnectivity).
//   - The scheduler loop itself watches for a gap: if a tick arrives more
//     than GapThreshold (default 10m) after the previous tick, or any due
//     time is more than GapThreshold overdue, the machine must have slept
//     without Pause being called, and the loop handles it exactly like a
//     Resume (minus the probe): in-flight rounds are interrupted and next
//     runs are recomputed, with nothing caught up.
package serversched

import (
	"context"
	"crypto/rand"
	"fmt"
	"strings"
	"sync"
	"time"

	"motty/internal/serverassets"
	"motty/internal/servercollect"
)

// loop ticks until the engine stops, looking for due work on each tick.
func (e *Engine) loop() {
	defer e.wg.Done()
	ticker := time.NewTicker(e.tickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-e.ctx.Done():
			return
		case now := <-ticker.C:
			e.tick(now)
		}
	}
}

// tick is one scheduler pass: detect a sleep gap, then enqueue due work.
func (e *Engine) tick(now time.Time) {
	e.mu.Lock()
	if !e.running {
		e.mu.Unlock()
		return
	}
	gap := false
	if !e.lastTick.IsZero() && now.Sub(e.lastTick) > e.gapThreshold {
		gap = true
	}
	if !gap && !e.paused {
		for _, kinds := range e.nextDue {
			for _, due := range kinds {
				if now.Sub(due) > e.gapThreshold {
					gap = true
					break
				}
			}
			if gap {
				break
			}
		}
	}
	e.lastTick = now
	if gap && !e.paused {
		e.wakeLocked(now)
	}
	paused := e.paused
	e.mu.Unlock()
	if paused {
		return
	}
	e.enqueueDue(now)
}

// enqueueDue appends a job for every (server, kind) pair whose time has
// come and that is not already queued or running. A server seen for the
// first time is due immediately. When a job is enqueued its next run is
// set to now plus its interval, so the schedule advances from the start
// of the round, not its completion. Templates are loaded once per tick,
// not once per server, so a tick costs two store loads in total.
func (e *Engine) enqueueDue(now time.Time) {
	servers, err := e.assets.ListServers(serverassets.Filter{})
	if err != nil {
		return
	}
	byID := map[string]servercollect.Template{}
	for _, tmpl := range e.Templates() {
		byID[tmpl.ID] = tmpl
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.running || e.paused {
		return
	}
	enqueued := false
	for _, server := range servers {
		tmpl := e.effectiveTemplateLocked(server.ID, byID)
		due := e.nextDue[server.ID]
		if due == nil {
			due = map[servercollect.MetricKind]time.Time{}
			e.nextDue[server.ID] = due
		}
		for _, kind := range servercollect.AllMetricKinds {
			next, ok := due[kind]
			if !ok {
				next = now
				due[kind] = next
			}
			if now.Before(next) {
				continue
			}
			if marks := e.inFlight[server.ID]; marks != nil && marks[kind] {
				continue
			}
			e.markInFlightLocked(server.ID, kind)
			due[kind] = now.Add(tmpl.Interval(kind))
			e.queue = append(e.queue, &job{serverID: server.ID, kinds: []servercollect.MetricKind{kind}})
			enqueued = true
		}
	}
	if enqueued {
		e.queueCond.Broadcast()
	}
}

// dispatcher starts queued jobs in FIFO order, gated by the concurrency
// semaphore. It is the only owner of jobs once they leave the queue: on
// engine stop it drops whatever is still queued, so a job is reported
// exactly once, by either runJob or dropJob.
func (e *Engine) dispatcher() {
	defer e.wg.Done()
	for {
		e.mu.Lock()
		for len(e.queue) == 0 && e.running {
			e.queueCond.Wait()
		}
		if !e.running {
			for _, j := range e.queue {
				e.clearInFlightLocked(j.serverID, j.kinds)
				if j.resultCh != nil {
					j.resultCh <- servercollect.RoundResult{ServerID: j.serverID, Interrupted: true, Err: ErrStopped}
				}
			}
			e.queue = nil
			e.mu.Unlock()
			return
		}
		j := e.queue[0]
		e.queue = e.queue[1:]
		e.mu.Unlock()

		select {
		case e.sem <- struct{}{}:
		case <-e.ctx.Done():
			e.dropJob(j, ErrStopped)
			continue
		}
		e.mu.Lock()
		if !e.running {
			e.mu.Unlock()
			<-e.sem
			e.dropJob(j, ErrStopped)
			continue
		}
		e.workers.Add(1)
		e.mu.Unlock()
		go func(j *job) {
			defer e.workers.Done()
			defer func() { <-e.sem }()
			e.runJob(j)
		}(j)
	}
}

// wakeLocked handles a sleep/wake gap. The caller must hold e.mu and the
// engine must be running. In-flight rounds are interrupted by cancelling
// their shared context and a fresh one is created; queued jobs are
// dropped; every kind's next run is recomputed as now plus its interval,
// so nothing that came due during the gap is caught up.
func (e *Engine) wakeLocked(now time.Time) {
	if e.inflightCancel != nil {
		e.inflightCancel()
	}
	e.inflightCtx, e.inflightCancel = context.WithCancel(e.ctx)
	e.dropQueuedLocked(ErrPaused)
	servers, err := e.assets.ListServers(serverassets.Filter{})
	if err != nil {
		return
	}
	for _, server := range servers {
		tmpl := e.templateForLocked(server.ID)
		due := e.nextDue[server.ID]
		if due == nil {
			due = map[servercollect.MetricKind]time.Time{}
			e.nextDue[server.ID] = due
		}
		for _, kind := range servercollect.AllMetricKinds {
			due[kind] = now.Add(tmpl.Interval(kind))
		}
	}
	e.lastTick = now
}

// dropQueuedLocked drops every queued job, reporting it as interrupted.
// The caller must hold e.mu. resultCh is buffered, so the send cannot
// block while the lock is held.
func (e *Engine) dropQueuedLocked(err error) {
	for _, j := range e.queue {
		e.clearInFlightLocked(j.serverID, j.kinds)
		if j.resultCh != nil {
			j.resultCh <- servercollect.RoundResult{ServerID: j.serverID, Interrupted: true, Err: err}
		}
	}
	e.queue = nil
}

// probeConnectivity runs after Resume: a lightweight reachability check
// for servers that are not in dial backoff. It reuses ensureConn — the
// same serverconn.Dial path as the connection test (which itself starts
// with a TCP reachability dial) — rather than a CollectNow of the
// connectivity kind, because it validates reachability even when there is
// nothing to collect, and a successful dial warms the connection cache
// for the next scheduled round instead of opening a second connection.
// Probes run sequentially in the background so a wake does not start a
// handshake burst; a failed probe records dial backoff, so the next
// scheduled round does not immediately retry it.
func (e *Engine) probeConnectivity() {
	servers, err := e.assets.ListServers(serverassets.Filter{})
	if err != nil {
		return
	}
	for _, server := range servers {
		e.mu.Lock()
		stopped := !e.running
		cc := e.conns[server.ID]
		inBackoff := cc != nil && !cc.nextDialAt.IsZero() && time.Now().Before(cc.nextDialAt)
		ctx := e.ctx
		e.mu.Unlock()
		if stopped || ctx == nil {
			return
		}
		if inBackoff {
			continue
		}
		var jump *serverassets.JumpHost
		if server.JumpHostID != "" {
			jh, err := e.assets.GetJumpHost(server.JumpHostID)
			if err != nil {
				continue
			}
			jump = &jh
		}
		probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, _ = e.ensureConn(probeCtx, server, jump, true)
		cancel()
		select {
		case <-ctx.Done():
			return
		default:
		}
	}
}

// --- batch-command queue (reserved) ---

// BatchJobStatus is the lifecycle of a reserved batch job.
type BatchJobStatus string

const (
	// BatchJobQueued is a job waiting for the (not yet implemented)
	// batch executor.
	BatchJobQueued BatchJobStatus = "queued"
	// BatchJobCancelled is a job the user cancelled before execution.
	BatchJobCancelled BatchJobStatus = "cancelled"
)

// BatchJob is one reserved batch-command job: one command for one server.
// It carries no execution state beyond queuing, because batch execution
// belongs to a later phase (Motty_Linux.md §10) and must run on its own
// queue, isolated from collection concurrency.
type BatchJob struct {
	ID        string
	ServerID  string
	Command   string
	CreatedAt time.Time
	Status    BatchJobStatus
}

// BatchQueue is the reserved batch-command queue. It is deliberately
// execution-free in this phase: jobs can be enqueued, cancelled and
// listed, but nothing runs them. The later batch phase will add an
// executor with its own 20-way concurrency limit, isolated from the
// collection semaphore, plus danger-command confirmation and history;
// until then this queue is a placeholder and must not be mistaken for a
// working batch runner.
type BatchQueue struct {
	mu    sync.Mutex
	jobs  map[string]*BatchJob
	order []string
}

// NewBatchQueue returns an empty reserved queue.
func NewBatchQueue() *BatchQueue {
	return &BatchQueue{jobs: map[string]*BatchJob{}}
}

func newBatchID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("serversched: random source unavailable: " + err.Error())
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// Enqueue adds a job to the reserved queue and returns its ID. The job is
// not executed.
func (q *BatchQueue) Enqueue(job BatchJob) (string, error) {
	if strings.TrimSpace(job.ServerID) == "" {
		return "", fmt.Errorf("%w: batch job server id is required", ErrValidation)
	}
	if strings.TrimSpace(job.Command) == "" {
		return "", fmt.Errorf("%w: batch job command is required", ErrValidation)
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	job.ID = newBatchID()
	job.CreatedAt = time.Now().UTC()
	job.Status = BatchJobQueued
	stored := job
	q.jobs[job.ID] = &stored
	q.order = append(q.order, job.ID)
	return job.ID, nil
}

// Cancel marks a queued job cancelled. It returns false when the job does
// not exist or is no longer queued.
func (q *BatchQueue) Cancel(id string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	job, ok := q.jobs[id]
	if !ok || job.Status != BatchJobQueued {
		return false
	}
	job.Status = BatchJobCancelled
	return true
}

// Get returns one job by ID.
func (q *BatchQueue) Get(id string) (BatchJob, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	job, ok := q.jobs[id]
	if !ok {
		return BatchJob{}, false
	}
	return *job, true
}

// List returns all jobs in enqueue order.
func (q *BatchQueue) List() []BatchJob {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]BatchJob, 0, len(q.order))
	for _, id := range q.order {
		if job, ok := q.jobs[id]; ok {
			out = append(out, *job)
		}
	}
	return out
}
