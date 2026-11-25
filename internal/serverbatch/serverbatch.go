// Package serverbatch implements batch command execution over managed
// Linux servers (Motty_Linux.md §10, §13.2, acceptance item 12).
//
// It is the A-road of phase 5: risk classification, command templates,
// the concurrent executor, run history and export. It does not own any UI
// (that is the B-road's internal/workbench surface, which calls the frozen
// API in this package) and it does not build a second SSH stack: every
// execution goes through sshconn.Conn.Exec, and dialing is injected by the
// caller as a DialFunc — in production an adapter over serverconn.Dial,
// which carries the jump host, fingerprint check and vault-backed
// credentials. The executor itself never sees a password, a private key,
// or an application password, and neither logs, errors nor exports from
// this package may contain one; command text and command output are passed
// through Redact before export.
//
// Risk classification (§10.3) uses a rule table over a shell-structure
// parse — separators, pipes, command substitution, sudo/env prefixes,
// redirections, quotes and comments — not plain substring matching. Even
// so, identification cannot cover every risky command: the Reasons of any
// non-normal assessment say so, and the UI must always show the command
// and the target list before executing, whatever the level.
//
// Cancellation is honest about its limits: aborting cancels the run
// context, which closes the SSH command sessions, but a remote process
// that detached from its session may keep running after the SSH channel
// closes. A cancelled run therefore reports the servers as Cancelled and
// never claims the remote process was killed.
package serverbatch

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"time"

	"motty/internal/serverassets"
	"motty/internal/sshconn"
)

// RiskLevel grades how dangerous a command looks before it runs.
type RiskLevel int

const (
	// RiskNormal is an ordinary command: run it.
	RiskNormal RiskLevel = iota
	// RiskHigh is a risky command: the UI asks for a second confirmation.
	RiskHigh
	// RiskCritical is a very risky command: the UI requires the user to
	// type ConfirmPhrase verbatim before running it.
	RiskCritical
	// RiskBlocked is a command the executor refuses to run at all.
	RiskBlocked
)

// String returns the lowercase English level name.
func (l RiskLevel) String() string {
	switch l {
	case RiskNormal:
		return "normal"
	case RiskHigh:
		return "high"
	case RiskCritical:
		return "critical"
	case RiskBlocked:
		return "blocked"
	default:
		return "normal"
	}
}

// ConfirmPhrase is the exact text the user must type before a
// RiskCritical command runs.
const ConfirmPhrase = "确认执行"

// RiskAssessment is the outcome of ClassifyRisk.
type RiskAssessment struct {
	Level   RiskLevel
	Reasons []string // short Chinese reasons
}

// ResultState is how one server's execution ended.
type ResultState int

const (
	// StateSuccess is a zero exit status.
	StateSuccess ResultState = iota
	// StateFailed is a dial failure, a non-zero exit status, or an
	// execution error other than timeout/cancellation.
	StateFailed
	// StateTimeout is a per-server timeout.
	StateTimeout
	// StateCancelled is a run aborted (or never started) by ctx cancel.
	StateCancelled
	// StateSkipped is a placeholder for a server not started yet
	// (StartedAt is zero), or a server skipped because the run ended
	// before it began.
	StateSkipped
)

// String returns the lowercase English state name.
func (s ResultState) String() string {
	switch s {
	case StateSuccess:
		return "success"
	case StateFailed:
		return "failed"
	case StateTimeout:
		return "timeout"
	case StateCancelled:
		return "cancelled"
	case StateSkipped:
		return "skipped"
	default:
		return "skipped"
	}
}

// ServerResult is one server's outcome within a Run.
type ServerResult struct {
	ServerID   string      `json:"server_id"`
	ServerName string      `json:"server_name"`
	Address    string      `json:"address"`
	State      ResultState `json:"state"`
	ExitCode   *int        `json:"exit_code,omitempty"`
	Output     string      `json:"output,omitempty"`
	// OutputTruncated reports that the remote output exceeded
	// MaxOutputBytes and was cut.
	OutputTruncated bool          `json:"output_truncated,omitempty"`
	Error           string        `json:"error,omitempty"`
	Duration        time.Duration `json:"duration_ns"`
	Retries         int           `json:"retries"`
	StartedAt       time.Time     `json:"started_at"`
	FinishedAt      time.Time     `json:"finished_at"`
}

// RunState is whether a Run is still going.
type RunState int

const (
	// RunRunning is still executing.
	RunRunning RunState = iota
	// RunFinished completed (every server reached a terminal state).
	RunFinished
	// RunAborted was cancelled before every server finished.
	RunAborted
)

// String returns the lowercase English run state name.
func (s RunState) String() string {
	switch s {
	case RunRunning:
		return "running"
	case RunFinished:
		return "finished"
	case RunAborted:
		return "aborted"
	default:
		return "running"
	}
}

// Run is one batch execution. Before it starts, Targets holds one
// placeholder per server (State=StateSkipped, StartedAt zero); results
// fill in as servers finish. A Run is JSON-serialisable as a single
// configstore record (kind "server-batch-history", id = Run.ID).
type Run struct {
	ID         string         `json:"id"`
	Command    string         `json:"command"`
	TemplateID string         `json:"template_id,omitempty"`
	Targets    []ServerResult `json:"targets"`
	State      RunState       `json:"state"`
	CreatedAt  time.Time      `json:"created_at"`
	StartedAt  time.Time      `json:"started_at"`
	FinishedAt time.Time      `json:"finished_at"`
	Aborted    bool           `json:"aborted,omitempty"`
}

// RunOptions tunes one execution.
type RunOptions struct {
	// Concurrency caps simultaneous servers; default 20.
	Concurrency int
	// TimeoutPerServer bounds dial + execution for one server (one
	// attempt); default 5 minutes.
	TimeoutPerServer time.Duration
	// MaxRetries retries failed servers only (dial and execution
	// failures, never timeout or cancellation); default 1, 0 disables.
	MaxRetries int
	// MaxOutputBytes caps retained output per server; default 1<<20.
	MaxOutputBytes int
}

// DefaultRunOptions returns the §10.2 defaults.
func DefaultRunOptions() RunOptions {
	return RunOptions{
		Concurrency:      20,
		TimeoutPerServer: 5 * time.Minute,
		MaxRetries:       1,
		MaxOutputBytes:   1 << 20,
	}
}

// normalised fills in defaults for unset (zero/negative) fields.
func (o RunOptions) normalised() RunOptions {
	def := DefaultRunOptions()
	if o.Concurrency <= 0 {
		o.Concurrency = def.Concurrency
	}
	if o.TimeoutPerServer <= 0 {
		o.TimeoutPerServer = def.TimeoutPerServer
	}
	if o.MaxRetries < 0 {
		o.MaxRetries = def.MaxRetries
	}
	if o.MaxOutputBytes <= 0 {
		o.MaxOutputBytes = def.MaxOutputBytes
	}
	return o
}

// DialFunc dials one server and returns a live connection. The caller
// adapts serverconn.Dial internally (jump host and fingerprint check
// included); a failure returns an error carrying a short classified
// reason, never a credential. The executor owns the returned Conn and
// closes it after the attempt.
type DialFunc func(ctx context.Context, server serverassets.Server) (*sshconn.Conn, error)

var (
	// ErrValidation means a field failed validation.
	ErrValidation = errors.New("serverbatch: validation failed")
	// ErrNotFound means no record with that id exists.
	ErrNotFound = errors.New("serverbatch: not found")
	// ErrBlockedCommand means the command is RiskBlocked and the
	// executor refused to run it.
	ErrBlockedCommand = errors.New("serverbatch: command is blocked")
	// ErrBusy means an Executor already has a run in progress.
	ErrBusy = errors.New("serverbatch: another batch run is already in progress")
)

// newUUID returns a random RFC 4122 version 4 UUID, using only the
// standard library (google/uuid is only an indirect dependency).
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("serverbatch: random source unavailable: " + err.Error())
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func nowUTC() time.Time { return time.Now().UTC() }
