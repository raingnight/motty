package servercollect

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"motty/internal/remotestats"
	"motty/internal/serverassets"
	"motty/internal/sshconn"
)

// fakeTransport implements transport for tests. run dispatches on the
// command text; stream feeds remotestats protocol lines.
type fakeTransport struct {
	run    func(ctx context.Context, cmd string, limit int) ([]byte, error)
	stream func(ctx context.Context, cmd string, limit int, line func([]byte) bool) error
	status sshconn.Status
}

func (f *fakeTransport) RunCommand(ctx context.Context, cmd string, limit int) ([]byte, error) {
	if f.run == nil {
		return nil, errors.New("fake: no RunCommand handler")
	}
	return f.run(ctx, cmd, limit)
}

func (f *fakeTransport) StreamCommand(ctx context.Context, cmd string, limit int, line func([]byte) bool) error {
	if f.stream == nil {
		return errors.New("fake: no StreamCommand handler")
	}
	return f.stream(ctx, cmd, limit, line)
}

func (f *fakeTransport) Status() sshconn.Status { return f.status }

// statsReading mirrors remotestats' test helper: a server 37.5% busy, using
// 3 of 8 GiB, moving 128 KiB/s in and 64 KiB/s out at the given uptime second.
func statsReading(second int) string {
	return "gop-stats-v2 " + itoa(375*second) + " " + itoa(1000*second) + " " +
		itoa(131072*second) + " " + itoa(65536*second) + " 8388608 5242880 " + itoa(second) + ".00\n"
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// feedStats returns a StreamCommand handler that hands the worker the two
// readings it needs for one delta Sample, then waits to be closed.
func feedStats() func(context.Context, string, int, func([]byte) bool) error {
	return func(ctx context.Context, _ string, _ int, line func([]byte) bool) error {
		for _, text := range []string{"gop-net-primary eth0\n", statsReading(10), statsReading(11)} {
			if !line([]byte(text)) {
				return nil
			}
		}
		<-ctx.Done()
		return ctx.Err()
	}
}

// detailsFixture returns DetailsWorker output; the network counters grow by
// (20480, 10240) on every call so the second poll yields real rates.
func detailsFixture(calls *atomic.Int32) []byte {
	n := int(calls.Add(1)) - 1
	rx := 102400 + n*20480
	tx := 204800 + n*10240
	return []byte("info\t8\t0.12\t1.23\t2.34\t123456.78\t6.8.0-test\n" +
		"disk\t102400\t90000\t12400\t88%\t1\t/run\n" +
		"disk\t1024000\t512000\t512000\t50%\t0\t/\n" +
		"disk\t1024000\t512000\t512000\t50%\t0\t/data\n" +
		"disk\t512000\t100000\t412000\t20%\t0\t/data2\n" +
		"net\teth0\t" + itoa(rx) + "\t" + itoa(tx) + "\n")
}

// fastFake is a fake transport serving the whole fast group (streaming
// stats, details, swap) plus canned one-shot outputs keyed by command.
func fastFake(calls *atomic.Int32, swap string) *fakeTransport {
	return &fakeTransport{
		stream: feedStats(),
		status: sshconn.Status{Phase: sshconn.PhaseConnected},
		run: func(ctx context.Context, cmd string, limit int) ([]byte, error) {
			switch {
			case strings.Contains(cmd, "motty-monitor-details-v1"):
				return detailsFixture(calls), nil
			case strings.Contains(cmd, "SwapTotal"):
				return []byte(swap), nil
			default:
				return nil, errors.New("fake: unexpected command: " + cmd)
			}
		},
	}
}

func valueByKind(res RoundResult, kind MetricKind) (MetricValue, bool) {
	for _, v := range res.Values {
		if v.Kind == kind {
			return v, true
		}
	}
	return MetricValue{}, false
}

func TestCollectFastRound(t *testing.T) {
	var calls atomic.Int32
	c := &collector{itemTimeout: 10 * time.Second}
	tr := fastFake(&calls, "SwapTotal:       0 kB\nSwapFree:        0 kB\n")
	res := c.collect(context.Background(), tr, serverassets.Server{ID: "srv-1"},
		[]MetricKind{MetricCPU, MetricMemory, MetricNetwork, MetricDiskUsage})
	if res.Err != nil {
		t.Fatalf("round err = %v", res.Err)
	}
	cpu, ok := valueByKind(res, MetricCPU)
	if !ok || cpu.Unavailable != "" || cpu.CPU == nil {
		t.Fatalf("cpu value = %+v", cpu)
	}
	if cpu.CPU.Percent == nil || *cpu.CPU.Percent != 37.5 {
		t.Fatalf("cpu percent = %v, want 37.5", cpu.CPU.Percent)
	}
	if !cpu.CPU.LoadOK || cpu.CPU.Load1 != 0.12 || cpu.CPU.Load5 != 1.23 || cpu.CPU.Load15 != 2.34 {
		t.Fatalf("cpu load = %+v", cpu.CPU)
	}
	if cpu.CPU.Cores != 8 {
		t.Fatalf("cpu cores = %d, want 8 (details fallback)", cpu.CPU.Cores)
	}
	mem, _ := valueByKind(res, MetricMemory)
	if mem.Unavailable != "" || mem.Memory == nil {
		t.Fatalf("memory value = %+v", mem)
	}
	if mem.Memory.TotalBytes != 8<<30 || mem.Memory.UsedBytes != 3<<30 || mem.Memory.AvailableBytes != 5<<30 {
		t.Fatalf("memory = %+v", mem.Memory)
	}
	if mem.Memory.Percent == nil || *mem.Memory.Percent != 37.5 {
		t.Fatalf("memory percent = %v", mem.Memory.Percent)
	}
	if mem.Memory.SwapEnabled || mem.Memory.SwapPercent != nil {
		t.Fatalf("swap with SwapTotal 0 must be disabled with nil percent: %+v", mem.Memory)
	}
	net, _ := valueByKind(res, MetricNetwork)
	if net.Unavailable != "" || net.Network == nil || len(net.Network.Interfaces) != 1 {
		t.Fatalf("network value = %+v", net)
	}
	iface := net.Network.Interfaces[0]
	if iface.Name != "eth0" || iface.ReceiveTotal != 102400 || iface.SendTotal != 204800 {
		t.Fatalf("network totals = %+v", iface)
	}
	if !iface.HasRate || iface.ReceivePerSec == nil || iface.SendPerSec == nil {
		t.Fatalf("second details poll must yield rates: %+v", iface)
	}
	if *iface.ReceivePerSec < 5000 || *iface.ReceivePerSec > 20000 {
		t.Fatalf("receive rate = %d, want ~10240/s", *iface.ReceivePerSec)
	}
	disk, _ := valueByKind(res, MetricDiskUsage)
	if disk.Unavailable != "" || disk.DiskUsage == nil {
		t.Fatalf("disk value = %+v", disk)
	}
	// /run is virtual (excluded); / and /data are the same underlying
	// filesystem reported twice (deduped); /data2 is distinct.
	if len(disk.DiskUsage.Filesystems) != 2 {
		t.Fatalf("filesystems = %+v, want / and /data2 only", disk.DiskUsage.Filesystems)
	}
	if disk.DiskUsage.Filesystems[0].Mount != "/" || disk.DiskUsage.Filesystems[1].Mount != "/data2" {
		t.Fatalf("filesystems = %+v", disk.DiskUsage.Filesystems)
	}
	if disk.DiskUsage.Filesystems[0].Percent != 50 {
		t.Fatalf("root percent = %v, want 50", disk.DiskUsage.Filesystems[0].Percent)
	}
}

func TestNetworkBaselineHasNoRate(t *testing.T) {
	// Second details poll blocks: the round only ever sees the baseline.
	var calls atomic.Int32
	c := &collector{itemTimeout: 500 * time.Millisecond}
	tr := &fakeTransport{
		stream: feedStats(),
		status: sshconn.Status{Phase: sshconn.PhaseConnected},
		run: func(ctx context.Context, cmd string, limit int) ([]byte, error) {
			switch {
			case strings.Contains(cmd, "motty-monitor-details-v1"):
				if calls.Add(1) == 1 {
					return detailsFixture(&atomic.Int32{}), nil
				}
				<-ctx.Done()
				return nil, ctx.Err()
			case strings.Contains(cmd, "SwapTotal"):
				return []byte("SwapTotal:       0 kB\nSwapFree:        0 kB\n"), nil
			default:
				return nil, errors.New("unexpected command")
			}
		},
	}
	res := c.collect(context.Background(), tr, serverassets.Server{ID: "srv-1"}, []MetricKind{MetricNetwork})
	net, _ := valueByKind(res, MetricNetwork)
	if net.Unavailable != "" || net.Network == nil || len(net.Network.Interfaces) != 1 {
		t.Fatalf("network value = %+v", net)
	}
	iface := net.Network.Interfaces[0]
	if iface.HasRate || iface.ReceivePerSec != nil || iface.SendPerSec != nil {
		t.Fatalf("baseline-only sample must not invent rates: %+v", iface)
	}
	if iface.ReceiveTotal != 102400 {
		t.Fatalf("baseline totals must still be real: %+v", iface)
	}
}

func TestCPUPercentIsAlreadyAPercentage(t *testing.T) {
	sample := remotestats.Sample{CPUPercent: 37.5, Cores: []remotestats.Core{{ID: "0", Percent: 40}, {ID: "1", Percent: 35}}}
	cpu := cpuFromSample(sample, nil)
	if cpu.Percent == nil || *cpu.Percent != 37.5 {
		t.Fatalf("percent = %v, want 37.5 (no rescaling)", cpu.Percent)
	}
	if cpu.Cores != 2 {
		t.Fatalf("cores = %d, want 2", cpu.Cores)
	}
	if cpu.LoadOK {
		t.Fatal("load must be unavailable without details")
	}
}

func TestMemoryFormulasAndSwapBoundaries(t *testing.T) {
	sample := remotestats.Sample{MemoryTotalBytes: 8 << 30, MemoryUsedBytes: 3 << 30}
	mem := memoryFromSample(sample)
	if mem.AvailableBytes != 5<<30 || mem.Percent == nil || *mem.Percent != 37.5 {
		t.Fatalf("memory = %+v", mem)
	}
	applySwap(mem, swapInfo{total: 0, free: 0})
	if mem.SwapEnabled || mem.SwapPercent != nil {
		t.Fatalf("SwapTotal 0 must disable swap without division: %+v", mem)
	}
	applySwap(mem, swapInfo{total: 4 << 30, free: 1 << 30})
	if !mem.SwapEnabled || mem.SwapUsedBytes != 3<<30 || mem.SwapPercent == nil || *mem.SwapPercent != 75 {
		t.Fatalf("swap = %+v", mem)
	}
}

func TestDiskUsageDedupAndVirtualExclusion(t *testing.T) {
	d := remotestats.Details{Disks: []remotestats.Disk{
		{Path: "/run", Total: 100, Used: 90, Available: 10, Virtual: true},
		{Path: "/", Total: 1000, Used: 500, Available: 500},
		{Path: "/mnt/bind", Total: 1000, Used: 500, Available: 500}, // same fs, second mount
		{Path: "/data", Total: 2000, Used: 100, Available: 1900},
		{Path: "/empty", Total: 0, Used: 0, Available: 0},
	}}
	out := diskUsageFromDetails(d)
	if len(out.Filesystems) != 2 || out.Filesystems[0].Mount != "/" || out.Filesystems[1].Mount != "/data" {
		t.Fatalf("filesystems = %+v", out.Filesystems)
	}
}

func TestSingleItemTimeoutDoesNotFailRound(t *testing.T) {
	c := &collector{itemTimeout: 100 * time.Millisecond}
	tr := &fakeTransport{
		status: sshconn.Status{Phase: sshconn.PhaseConnected},
		run: func(ctx context.Context, cmd string, limit int) ([]byte, error) {
			if strings.Contains(cmd, "os-release") {
				<-ctx.Done()
				return nil, ctx.Err()
			}
			if strings.Contains(cmd, "command -v docker") {
				return []byte("MOT_MISSING\n"), nil
			}
			return nil, errors.New("unexpected command")
		},
	}
	res := c.collect(context.Background(), tr, serverassets.Server{ID: "srv-1"},
		[]MetricKind{MetricBasic, MetricConnectivity, MetricDocker})
	if res.Err != nil || res.Interrupted {
		t.Fatalf("round must not fail: err=%v interrupted=%v", res.Err, res.Interrupted)
	}
	basic, _ := valueByKind(res, MetricBasic)
	if basic.Unavailable == "" || !strings.Contains(basic.Unavailable, "timed out") {
		t.Fatalf("basic must be unavailable with a timeout reason: %+v", basic)
	}
	if basic.Basic != nil {
		t.Fatal("unavailable value must carry no payload")
	}
	conn, _ := valueByKind(res, MetricConnectivity)
	if conn.Unavailable != "" || conn.Connectivity == nil || !conn.Connectivity.OK {
		t.Fatalf("connectivity must still succeed: %+v", conn)
	}
	docker, _ := valueByKind(res, MetricDocker)
	if docker.Unavailable != "" || docker.Docker == nil || docker.Docker.Installed {
		t.Fatalf("docker must still succeed: %+v", docker)
	}
}

func TestCollectNilConnection(t *testing.T) {
	res, err := New().Collect(context.Background(), nil, serverassets.Server{ID: "srv-1"}, []MetricKind{MetricCPU, MetricConnectivity})
	if err != nil {
		t.Fatalf("Collect err = %v", err)
	}
	if res.Err == nil || len(res.Values) != 2 {
		t.Fatalf("nil connection must report a round error and unavailable values: %+v", res)
	}
	for _, v := range res.Values {
		if v.Unavailable == "" {
			t.Fatalf("value must be unavailable: %+v", v)
		}
	}
}

func TestCollectCancelledRoundMarksInterrupted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := &collector{itemTimeout: time.Second}
	tr := &fakeTransport{status: sshconn.Status{Phase: sshconn.PhaseConnected}}
	res := c.collect(ctx, tr, serverassets.Server{ID: "srv-1"}, []MetricKind{MetricCPU})
	if !res.Interrupted || len(res.Values) != 1 || res.Values[0].Unavailable == "" {
		t.Fatalf("cancelled round = %+v", res)
	}
}

func TestConnectivityStages(t *testing.T) {
	c := &collector{itemTimeout: time.Second}
	for _, tc := range []struct {
		status sshconn.Status
		stage  string
		ok     bool
	}{
		{sshconn.Status{Phase: sshconn.PhaseConnected}, "", true},
		{sshconn.Status{Phase: sshconn.PhaseFailed, Err: sshconn.ErrAuthFailed}, "auth", false},
		{sshconn.Status{Phase: sshconn.PhaseFailed, Err: sshconn.ErrHostKeyChanged}, "handshake", false},
		{sshconn.Status{Phase: sshconn.PhaseClosed}, "tcp", false},
		{sshconn.Status{Phase: sshconn.PhasePasswordPrompt}, "auth", false},
	} {
		tr := &fakeTransport{status: tc.status}
		res := c.collect(context.Background(), tr, serverassets.Server{ID: "srv-1"}, []MetricKind{MetricConnectivity})
		v, _ := valueByKind(res, MetricConnectivity)
		if tc.ok {
			if v.Unavailable != "" || v.Connectivity == nil || !v.Connectivity.OK {
				t.Fatalf("status %+v: %+v", tc.status, v)
			}
		} else {
			if v.Unavailable == "" || !strings.Contains(v.Unavailable, "stage "+tc.stage) {
				t.Fatalf("status %+v: %+v, want stage %s", tc.status, v, tc.stage)
			}
		}
	}
}
