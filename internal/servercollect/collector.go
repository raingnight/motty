// Package servercollect implements agentless Linux resource collection for
// managed servers (phase 3). See types.go for the frozen contract.
//
// Collection reuses remotestats wherever it exists: the streaming Worker for
// CPU/memory samples and the DetailsWorker for load averages, disk capacity
// and per-interface network totals. One-shot commands and parsers are only
// added for data remotestats does not provide (OS identification, disk IO,
// NIC details, process/service status, Docker).
//
// A Collector is stateless. The caller owns the SSH connection and passes it
// in; Collect never closes it and never dials a new one. Each item enforces
// its own 15s timeout inside the round's 60s budget; a single item failing
// only marks that item unavailable and never fails the round. Missing data is
// never reported as zero, and no credential ever appears in an error.
package servercollect

import (
	"context"
	"errors"
	"time"

	"motty/internal/remotestats"
	"motty/internal/serverassets"
	"motty/internal/sshconn"
)

// itemTimeout is the per-item budget from spec §7.3 (15s).
const defaultItemTimeout = 15 * time.Second

// transport is the surface Collect needs from a connection. *sshconn.Conn
// satisfies it directly; tests supply fakes through collectWithTransport.
type transport interface {
	RunCommand(ctx context.Context, command string, maxOutputBytes int) ([]byte, error)
	StreamCommand(ctx context.Context, command string, maxLineBytes int, line func([]byte) bool) error
	Status() sshconn.Status
}

type collector struct {
	itemTimeout time.Duration
}

// New returns a Collector.
func New() Collector {
	return &collector{itemTimeout: defaultItemTimeout}
}

func (c *collector) Collect(ctx context.Context, conn *sshconn.Conn, server serverassets.Server, kinds []MetricKind) (RoundResult, error) {
	started := time.Now()
	if ctx == nil {
		ctx = context.Background()
	}
	kinds = normalizeKinds(kinds)
	if conn == nil {
		res := RoundResult{ServerID: server.ID, StartedAt: started, FinishedAt: time.Now(),
			Err: errors.New("servercollect: no connection provided")}
		for _, kind := range kinds {
			res.Values = append(res.Values, unavailable(kind, time.Now(), "no connection provided"))
		}
		return res, nil
	}
	res := c.collect(ctx, conn, server, kinds)
	res.StartedAt = started
	return res, nil
}

// normalizeKinds dedups kinds preserving order; empty means all kinds.
func normalizeKinds(kinds []MetricKind) []MetricKind {
	if len(kinds) == 0 {
		return append([]MetricKind(nil), AllMetricKinds...)
	}
	seen := make(map[MetricKind]bool, len(kinds))
	out := make([]MetricKind, 0, len(kinds))
	for _, k := range kinds {
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out
}

func (c *collector) collect(ctx context.Context, tr transport, server serverassets.Server, kinds []MetricKind) RoundResult {
	res := RoundResult{ServerID: server.ID, StartedAt: time.Now()}
	fast := &fastState{}
	for _, kind := range kinds {
		if err := ctx.Err(); err != nil {
			res.Interrupted = true
			if res.Err == nil {
				res.Err = err
			}
			res.Values = append(res.Values, unavailable(kind, time.Now(), "round cancelled: "+err.Error()))
			continue
		}
		itemCtx, cancel := context.WithTimeout(ctx, c.itemTimeout)
		res.Values = append(res.Values, c.collectOne(itemCtx, tr, server, kind, fast))
		cancel()
	}
	res.FinishedAt = time.Now()
	return res
}

func (c *collector) collectOne(ctx context.Context, tr transport, server serverassets.Server, kind MetricKind, fast *fastState) MetricValue {
	now := time.Now()
	switch kind {
	case MetricBasic:
		return c.collectBasic(ctx, tr, server, now)
	case MetricCPU, MetricMemory, MetricDiskUsage, MetricNetwork:
		return c.collectFastKind(ctx, tr, server, kind, fast, now)
	case MetricDiskIO:
		return c.collectDiskIO(ctx, tr, now)
	case MetricNIC:
		return c.collectNIC(ctx, tr, now)
	case MetricSystem:
		return c.collectSystem(ctx, tr, now)
	case MetricDocker:
		return c.collectDocker(ctx, tr, server, now)
	case MetricConnectivity:
		return c.collectConnectivity(tr, now)
	default:
		return unavailable(kind, now, "unknown metric kind")
	}
}

// unavailable builds a MetricValue that reports a failed item with its
// reason and no payload, so a missing value is never mistaken for zero.
func unavailable(kind MetricKind, now time.Time, reason string) MetricValue {
	return MetricValue{
		Kind:        kind,
		CollectedAt: now,
		StaleAfter:  DefaultTemplate().StaleAfter(kind),
		Unavailable: reason,
	}
}

func success(kind MetricKind, now time.Time, fill func(*MetricValue)) MetricValue {
	v := MetricValue{Kind: kind, CollectedAt: now, StaleAfter: DefaultTemplate().StaleAfter(kind)}
	fill(&v)
	return v
}

// reason turns an item error into a short reason. It never includes
// command output or credentials; commands here are fixed strings.
func (c *collector) reason(err error) string {
	if err == nil {
		return "collection failed"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timed out after " + c.itemTimeout.String()
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	return err.Error()
}

// fastState caches one round's shared remotestats workers so CPU, memory,
// network and disk-usage kinds do not each start their own streams.
type fastState struct {
	done    bool
	cpu     *CPUInfo
	cpuErr  string
	mem     *MemoryInfo
	memErr  string
	net     *NetworkInfo
	netErr  string
	disk    *DiskUsageInfo
	diskErr string
}

func (c *collector) collectFastKind(ctx context.Context, tr transport, server serverassets.Server, kind MetricKind, fast *fastState, now time.Time) MetricValue {
	if !fast.done {
		c.runFast(ctx, tr, server, fast)
		fast.done = true
	}
	switch kind {
	case MetricCPU:
		if fast.cpu == nil {
			return unavailable(kind, now, fast.cpuErr)
		}
		cpu := *fast.cpu
		return success(kind, now, func(v *MetricValue) { v.CPU = &cpu })
	case MetricMemory:
		if fast.mem == nil {
			return unavailable(kind, now, fast.memErr)
		}
		mem := *fast.mem
		return success(kind, now, func(v *MetricValue) { v.Memory = &mem })
	case MetricNetwork:
		if fast.net == nil {
			return unavailable(kind, now, fast.netErr)
		}
		net := *fast.net
		return success(kind, now, func(v *MetricValue) { v.Network = &net })
	case MetricDiskUsage:
		if fast.disk == nil {
			return unavailable(kind, now, fast.diskErr)
		}
		disk := *fast.disk
		return success(kind, now, func(v *MetricValue) { v.DiskUsage = &disk })
	}
	return unavailable(kind, now, "unknown metric kind")
}

// runFast starts one short-lived streaming worker (CPU/memory) and one
// DetailsWorker (load/disk/network) and converts their first results.
//
//   - The streaming worker's first published Sample is already the delta of
//     two readings (the warmup reading is never published), so its
//     CPUPercent is a real percentage (0-100, see remotestats.sample) and
//     its memory/network figures are real rates, not a baseline.
//   - Network is built from the DetailsWorker, not from the streaming
//     Sample: the Sample only carries the primary interface's aggregate
//     rates and no per-interface totals, so per-interface totals and rates
//     cannot honestly be built from it. The DetailsWorker's first result
//     gives per-interface totals (HasRate=false, rates nil); a second result
//     one interval later gives real rates. If the second result does not
//     arrive in time, the totals-only result is still returned.
//   - Load comes from the same DetailsWorker result. If details fail, CPU
//     percent is still reported with LoadOK=false rather than failing CPU.
func (c *collector) runFast(ctx context.Context, tr transport, server serverassets.Server, fast *fastState) {
	fast.cpuErr = "CPU sample unavailable"
	fast.memErr = "memory sample unavailable"
	fast.netErr = "network sample unavailable"
	fast.diskErr = "disk usage unavailable"

	worker := remotestats.StartWithInterval(tr, nil, 2, remotestats.CPU|remotestats.Memory|remotestats.NetworkIO)
	if worker != nil {
		defer worker.Close()
	}
	detailsWorker := remotestats.StartDetailsWithInterval(tr, nil, 2)
	if detailsWorker != nil {
		defer detailsWorker.Close()
	}

	var sample *remotestats.Sample
	if worker != nil {
		sample, _ = waitSample(ctx, worker)
	}
	var firstDetails, secondDetails *remotestats.Details
	if detailsWorker != nil {
		firstDetails, secondDetails = waitDetails(ctx, detailsWorker, true)
	}

	if sample != nil {
		fast.cpu = cpuFromSample(*sample, firstDetails)
		fast.mem = memoryFromSample(*sample)
		// Swap is not in remotestats samples; read it from /proc/meminfo.
		if swap, err := readSwap(ctx, tr); err == nil {
			applySwap(fast.mem, swap)
		}
		// If the swap read fails, SwapEnabled stays false and SwapPercent
		// stays nil: unknown swap, never a fabricated zero.
		fast.cpuErr, fast.memErr = "", ""
	}
	if firstDetails != nil {
		if fast.cpu != nil {
			applyLoad(fast.cpu, firstDetails)
		} else {
			// Details alone cannot give a CPU percent, but record the
			// load so a CPU value is not silently dropped: keep cpu nil
			// and let the caller report the streaming failure.
			fast.cpuErr = "CPU sample unavailable; load averages were collected but no CPU percent"
		}
		fast.disk = diskUsageFromDetails(*firstDetails)
		if len(fast.disk.Filesystems) == 0 {
			fast.disk = nil
			fast.diskErr = "no persistent filesystems reported"
		} else {
			fast.diskErr = ""
		}
		fast.net = networkFromDetails(firstDetails, secondDetails)
		if fast.net == nil {
			fast.netErr = "no network interfaces reported"
		} else {
			fast.netErr = ""
		}
	}
}

// waitSample waits for the worker's first published Sample (already a delta
// of two readings) or its terminal error.
func waitSample(ctx context.Context, worker *remotestats.Worker) (*remotestats.Sample, error) {
	for {
		select {
		case result, ok := <-worker.Results():
			if !ok {
				return nil, errors.New("stats worker ended without a sample")
			}
			if result.Err != nil {
				return nil, result.Err
			}
			s := result.Sample
			return &s, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// waitDetails waits for the first DetailResult and, when needSecond is set,
// a second one (which carries network rates). A failed second poll keeps the
// first result; a failed first poll reports the error.
func waitDetails(ctx context.Context, worker *remotestats.DetailsWorker, needSecond bool) (first, second *remotestats.Details) {
	for {
		if first != nil && (!needSecond || second != nil) {
			return first, second
		}
		select {
		case result, ok := <-worker.Results():
			if !ok {
				return first, second
			}
			if result.Err != nil {
				return first, second
			}
			d := result.Details
			if first == nil {
				first = &d
			} else if second == nil {
				second = &d
			}
		case <-ctx.Done():
			return first, second
		}
	}
}

// collectConnectivity judges reachability from the passed-in connection's
// state only. It never dials: redial is the scheduler's job.
func (c *collector) collectConnectivity(tr transport, now time.Time) MetricValue {
	status := tr.Status()
	if status.Phase == sshconn.PhaseConnected {
		return success(MetricConnectivity, now, func(v *MetricValue) {
			v.Connectivity = &ConnectivityInfo{OK: true}
		})
	}
	stage, reason := connectivityStage(status)
	if status.Err != nil {
		reason += ": " + status.Err.Error()
	}
	return unavailable(MetricConnectivity, now, reason+" (stage "+stage+")")
}

// connectivityStage maps a connection phase to the §5.4 test stage
// (tcp / handshake / auth) and a short reason.
func connectivityStage(status sshconn.Status) (string, string) {
	switch status.Phase {
	case sshconn.PhaseConnecting:
		return "tcp", "connection not established"
	case sshconn.PhaseHostKeyPrompt:
		if status.HostKey.Changed {
			return "handshake", "host key changed"
		}
		return "handshake", "host key not yet accepted"
	case sshconn.PhaseAuthenticating, sshconn.PhasePasswordPrompt, sshconn.PhaseKeyPassphrasePrompt:
		return "auth", "authentication not completed"
	case sshconn.PhaseFailed:
		if err := status.Err; err != nil {
			switch {
			case errors.Is(err, sshconn.ErrAuthFailed), errors.Is(err, sshconn.ErrTooManyAuthAttempts):
				return "auth", "authentication failed"
			case errors.Is(err, sshconn.ErrHostKeyChanged), errors.Is(err, sshconn.ErrHostKeyRejected):
				return "handshake", "host key verification failed"
			}
		}
		return "tcp", "connection failed"
	case sshconn.PhaseClosed:
		return "tcp", "connection closed"
	default:
		return "tcp", "connection not established"
	}
}
