package serverbatch

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"motty/internal/serverassets"
	"motty/internal/sshconn"
)

// Executor runs one batch command across many servers (§10.1, §10.2).
//
// Concurrency is a semaphore over per-server workers (default 20); each
// server is independent, so one server's dial failure, execution failure
// or timeout never blocks the others. Only failed servers are retried
// (dial and execution failures, never timeout or cancellation), up to
// MaxRetries. A per-server, per-attempt context carries TimeoutPerServer
// over dial + Exec; a timeout is recorded as StateTimeout.
//
// The executor runs synchronously in the caller's goroutine: the UI must
// call Run from a background goroutine, never the UI thread. Progress is
// reported through onUpdate, called with a deep copy of the Run after
// each server attempt finishes (including intermediate retry attempts)
// and once when an aborted run is finalised.
//
// Risk is deliberately not re-judged here: grading is the UI's job and
// ClassifyRisk is its tool. The one exception is defensive: a
// RiskBlocked command is refused before any Run is built, so a UI bug
// can never execute it through this path.
//
// Abort cancels the run context, which closes the SSH command sessions.
// A remote process that detached from its session may keep running after
// the channel closes; the executor reports those servers as Cancelled
// and never claims the remote process was killed.
type Executor struct {
	dial     DialFunc
	opts     RunOptions
	onUpdate func(*Run)

	mu     sync.Mutex
	cancel context.CancelFunc // cancels the active run, if any
	active bool
}

// NewExecutor builds an Executor. dial must not be nil. onUpdate may be
// nil; when set it receives a deep copy of the Run, so it may keep the
// pointer without racing the executor.
func NewExecutor(dial DialFunc, opts RunOptions, onUpdate func(*Run)) *Executor {
	if dial == nil {
		panic("serverbatch: nil DialFunc")
	}
	return &Executor{dial: dial, opts: opts.normalised(), onUpdate: onUpdate}
}

// Abort cancels the currently running Run, if any; with no run in
// progress it is a no-op. Servers not yet started are marked Cancelled
// by Run as it unwinds; servers already running have their SSH sessions
// closed (see the Executor doc comment for what that does not guarantee).
func (e *Executor) Abort() {
	e.mu.Lock()
	cancel := e.cancel
	e.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Run executes command on servers synchronously until every server has
// a terminal state or ctx is cancelled. It returns the finished (or
// aborted) Run. A RiskBlocked command or an empty command is an error
// and builds no Run; a second concurrent Run on the same Executor is
// ErrBusy.
func (e *Executor) Run(ctx context.Context, servers []serverassets.Server, command string, templateID string) (*Run, error) {
	if strings.TrimSpace(command) == "" {
		return nil, fmt.Errorf("%w: command is required", ErrValidation)
	}
	if assessment := ClassifyRisk(command); assessment.Level == RiskBlocked {
		return nil, fmt.Errorf("%w: %s", ErrBlockedCommand, strings.Join(assessment.Reasons, "；"))
	}
	if ctx == nil {
		ctx = context.Background()
	}

	e.mu.Lock()
	if e.active {
		e.mu.Unlock()
		return nil, ErrBusy
	}
	e.active = true
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		e.active = false
		e.cancel = nil
		e.mu.Unlock()
	}()

	now := nowUTC()
	run := &Run{
		ID:         newUUID(),
		Command:    command,
		TemplateID: templateID,
		State:      RunRunning,
		CreatedAt:  now,
		StartedAt:  now,
	}
	for _, srv := range servers {
		address := strings.TrimSpace(srv.Address)
		if address == "" {
			address = strings.TrimSpace(srv.Hostname)
		}
		run.Targets = append(run.Targets, ServerResult{
			ServerID:   srv.ID,
			ServerName: srv.Name,
			Address:    address,
			State:      StateSkipped, // placeholder: not started yet
		})
	}

	runCtx, cancel := context.WithCancel(ctx)
	e.mu.Lock()
	e.cancel = cancel
	e.mu.Unlock()
	defer cancel()

	sem := make(chan struct{}, e.opts.Concurrency)
	var wg sync.WaitGroup
	for i := range run.Targets {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-runCtx.Done():
				return
			}
			e.runOne(runCtx, run, i, servers[i], command)
		}(i)
	}
	wg.Wait()

	// Finalise: anything still a placeholder never started because the
	// run was cancelled.
	aborted := runCtx.Err() != nil
	e.mu.Lock()
	run.State = RunFinished
	if aborted {
		run.State = RunAborted
		run.Aborted = true
		for i := range run.Targets {
			if run.Targets[i].StartedAt.IsZero() {
				run.Targets[i].State = StateCancelled
				run.Targets[i].FinishedAt = nowUTC()
			}
		}
	}
	run.FinishedAt = nowUTC()
	e.mu.Unlock()
	if aborted {
		e.notify(run)
	}
	return run, nil
}

// runOne executes command on one server, retrying failures only.
func (e *Executor) runOne(runCtx context.Context, run *Run, index int, server serverassets.Server, command string) {
	started := nowUTC()
	for attempt := 0; ; attempt++ {
		if runCtx.Err() != nil {
			last := ServerResult{
				ServerID: run.Targets[index].ServerID, ServerName: run.Targets[index].ServerName,
				Address: run.Targets[index].Address, State: StateCancelled,
				StartedAt: started, FinishedAt: nowUTC(),
			}
			e.setResult(run, index, last)
			return
		}
		attemptCtx, cancel := context.WithTimeout(runCtx, e.opts.TimeoutPerServer)
		result := e.tryOnce(attemptCtx, server, command, started)
		cancel()
		result.Retries = attempt
		switch result.State {
		case StateSuccess, StateTimeout, StateCancelled:
			e.setResult(run, index, result)
			return
		case StateFailed:
			// Retry failures only, up to MaxRetries.
			if attempt >= e.opts.MaxRetries {
				e.setResult(run, index, result)
				return
			}
			// Report the intermediate failure, then retry.
			e.setResult(run, index, result)
		}
	}
}

// tryOnce dials once and runs the command once. started is the first
// attempt's start time, so Duration spans retries.
func (e *Executor) tryOnce(ctx context.Context, server serverassets.Server, command string, started time.Time) ServerResult {
	base := ServerResult{
		ServerID: server.ID, ServerName: server.Name,
		Address: addressOf(server), StartedAt: started,
	}
	finish := func(state ResultState, exitCode *int, output string, truncated bool, errText string) ServerResult {
		base.State = state
		base.ExitCode = exitCode
		base.Output = output
		base.OutputTruncated = truncated
		base.Error = errText
		base.FinishedAt = nowUTC()
		base.Duration = base.FinishedAt.Sub(started)
		return base
	}

	conn, err := e.dial(ctx, server)
	if err != nil {
		if ctx.Err() != nil && ctx.Err() == context.DeadlineExceeded {
			return finish(StateTimeout, nil, "", false, "连接超时")
		}
		if ctx.Err() != nil {
			return finish(StateCancelled, nil, "", false, "任务已中止")
		}
		return finish(StateFailed, nil, "", false, classifyDialError(err))
	}
	if conn == nil {
		return finish(StateFailed, nil, "", false, "拨号未返回连接")
	}
	defer conn.Close()

	result, err := conn.Exec(ctx, command, e.opts.MaxOutputBytes)
	if err != nil {
		output := string(result.Output)
		switch {
		case ctx.Err() == context.DeadlineExceeded:
			return finish(StateTimeout, nil, output, result.Truncated, "执行超时")
		case ctx.Err() != nil:
			return finish(StateCancelled, nil, output, result.Truncated, "任务已中止")
		default:
			return finish(StateFailed, nil, output, result.Truncated, classifyExecError(err))
		}
	}
	exitCode := result.ExitCode
	if result.ExitCode != 0 {
		return finish(StateFailed, &exitCode, string(result.Output), result.Truncated,
			fmt.Sprintf("命令退出码为 %d", result.ExitCode))
	}
	return finish(StateSuccess, &exitCode, string(result.Output), result.Truncated, "")
}

// setResult records one server's result and notifies onUpdate with a
// deep copy.
func (e *Executor) setResult(run *Run, index int, result ServerResult) {
	e.mu.Lock()
	run.Targets[index] = result
	e.mu.Unlock()
	e.notify(run)
}

// notify calls onUpdate with a deep copy of run, if one is set.
func (e *Executor) notify(run *Run) {
	if e.onUpdate == nil {
		return
	}
	e.mu.Lock()
	copy := *run
	copy.Targets = append([]ServerResult(nil), run.Targets...)
	e.mu.Unlock()
	e.onUpdate(&copy)
}

func addressOf(server serverassets.Server) string {
	if address := strings.TrimSpace(server.Address); address != "" {
		return address
	}
	return strings.TrimSpace(server.Hostname)
}

// classifyDialError turns a dial failure into a short Chinese reason
// without credential material. The underlying message is truncated and
// redacted as a defence in depth (a dial error should never contain a
// secret, but the executor does not trust that).
func classifyDialError(err error) string {
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "i/o timeout") || strings.Contains(msg, "timed out") || strings.Contains(msg, "deadline"):
		return "连接超时"
	case strings.Contains(msg, "connection refused"):
		return "连接被拒绝"
	case strings.Contains(msg, "no route") || strings.Contains(msg, "network is unreachable") || strings.Contains(msg, "unreachable"):
		return "网络不可达"
	case strings.Contains(msg, "auth") || strings.Contains(msg, "unable to authenticate") || strings.Contains(msg, "no supported methods") || strings.Contains(msg, "permission denied"):
		return "认证失败"
	case strings.Contains(msg, "host key") || strings.Contains(msg, "fingerprint") || strings.Contains(msg, "knownhosts"):
		return "主机指纹校验失败"
	default:
		detail := Redact(err.Error())
		if len(detail) > 120 {
			detail = detail[:120]
		}
		return "拨号失败：" + detail
	}
}

// classifyExecError turns an execution failure into a short Chinese
// reason. A signal kill and a lost connection are the common cases;
// anything else keeps a redacted, truncated detail.
func classifyExecError(err error) string {
	var exitErr *ssh.ExitError
	if errors.As(err, &exitErr) && exitErr.Signal() != "" {
		return fmt.Sprintf("命令被信号 %s 终止", exitErr.Signal())
	}
	if errors.Is(err, sshconn.ErrCanceled) {
		return "任务已中止"
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "connection lost") || strings.Contains(msg, "eof") || strings.Contains(msg, "use of closed"):
		return "连接中断"
	case strings.Contains(msg, "session") && strings.Contains(msg, "refus"):
		return "服务器拒绝执行命令"
	default:
		detail := Redact(err.Error())
		if len(detail) > 120 {
			detail = detail[:120]
		}
		return "执行失败：" + detail
	}
}
