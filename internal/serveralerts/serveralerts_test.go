package serveralerts

import (
	"testing"
	"time"

	"motty/internal/serverassets"
	"motty/internal/servercollect"
)

func testServer(id, name string) serverassets.Server {
	return serverassets.Server{ID: id, Name: name, Address: "10.0.0.1", Port: 22, User: "root"}
}

func staleAfter(kind servercollect.MetricKind) time.Duration {
	return servercollect.StaleFactor * servercollect.DefaultIntervals[kind]
}

func cpuValue(pct float64, at time.Time) servercollect.MetricValue {
	return servercollect.MetricValue{
		Kind: servercollect.MetricCPU, CollectedAt: at, StaleAfter: staleAfter(servercollect.MetricCPU),
		CPU: &servercollect.CPUInfo{Percent: &pct},
	}
}

func cpuBaselineValue(at time.Time) servercollect.MetricValue {
	// First sample: Percent is nil, so there is no usable value.
	return servercollect.MetricValue{
		Kind: servercollect.MetricCPU, CollectedAt: at, StaleAfter: staleAfter(servercollect.MetricCPU),
		CPU: &servercollect.CPUInfo{},
	}
}

func memValue(pct float64, at time.Time) servercollect.MetricValue {
	return servercollect.MetricValue{
		Kind: servercollect.MetricMemory, CollectedAt: at, StaleAfter: staleAfter(servercollect.MetricMemory),
		Memory: &servercollect.MemoryInfo{TotalBytes: 8 << 30, UsedBytes: uint64(float64(8<<30) * pct / 100), Percent: &pct},
	}
}

func diskValue(at time.Time, filesystems ...servercollect.Filesystem) servercollect.MetricValue {
	return servercollect.MetricValue{
		Kind: servercollect.MetricDiskUsage, CollectedAt: at, StaleAfter: staleAfter(servercollect.MetricDiskUsage),
		DiskUsage: &servercollect.DiskUsageInfo{Filesystems: filesystems},
	}
}

func connValue(ok bool, at time.Time) servercollect.MetricValue {
	return servercollect.MetricValue{
		Kind: servercollect.MetricConnectivity, CollectedAt: at, StaleAfter: staleAfter(servercollect.MetricConnectivity),
		Connectivity: &servercollect.ConnectivityInfo{OK: ok, Stage: "auth"},
	}
}

func latestWith(serverID string, values ...servercollect.MetricValue) servercollect.Latest {
	m := map[servercollect.MetricKind]servercollect.MetricValue{}
	for _, v := range values {
		m[v.Kind] = v
	}
	return servercollect.Latest{ServerID: serverID, Values: m, UpdatedAt: time.Now()}
}

func floatPtr(f float64) *float64 { return &f }

// --- thresholds with hysteresis ---

func TestEvaluateCPUHysteresis(t *testing.T) {
	srv := testServer("srv-1", "web-1")
	now := time.Now()
	// 79% is below the 80% alert threshold: no alert.
	created, updated, recovered := Evaluate(srv, latestWith("srv-1", cpuValue(79, now)), true, nil, nil, now)
	if len(created) != 0 || len(updated) != 0 || len(recovered) != 0 {
		t.Fatalf("79%%: got created=%d updated=%d recovered=%d, want none", len(created), len(updated), len(recovered))
	}
	// 80% triggers.
	created, _, _ = Evaluate(srv, latestWith("srv-1", cpuValue(80, now)), true, nil, nil, now)
	if len(created) != 1 {
		t.Fatalf("80%%: got %d created, want 1", len(created))
	}
	if created[0].ID != AlertID("srv-1", KindCPU, "") || created[0].Threshold != CPUAlertThreshold {
		t.Fatalf("80%%: unexpected alert %+v", created[0])
	}
	prev := created
	// 76% is between the 80% alert and 75% recovery thresholds: the
	// alert stays active and is refreshed, not recovered.
	_, updated, recovered = Evaluate(srv, latestWith("srv-1", cpuValue(76, now.Add(time.Minute))), true, nil, prev, now.Add(time.Minute))
	if len(updated) != 1 || len(recovered) != 0 {
		t.Fatalf("76%%: got updated=%d recovered=%d, want 1/0", len(updated), len(recovered))
	}
	if updated[0].ID != prev[0].ID || !updated[0].CreatedAt.Equal(prev[0].CreatedAt) {
		t.Fatalf("76%%: refresh must keep id and CreatedAt: %+v vs %+v", updated[0], prev[0])
	}
	// 74% is below the 75% recovery threshold: recovered.
	_, _, recovered = Evaluate(srv, latestWith("srv-1", cpuValue(74, now.Add(2*time.Minute))), true, nil, updated, now.Add(2*time.Minute))
	if len(recovered) != 1 || recovered[0].State != StateRecovered || recovered[0].RecoveredAt == nil || recovered[0].RecoverReason == "" {
		t.Fatalf("74%%: got recovered=%+v, want one recovered alert with reason", recovered)
	}
}

func TestEvaluateMemoryAndDiskHysteresis(t *testing.T) {
	srv := testServer("srv-1", "web-1")
	now := time.Now()
	// Memory: 84% no trigger, 85% trigger, 81% keep, 79% recover.
	created, _, _ := Evaluate(srv, latestWith("srv-1", memValue(84, now)), true, nil, nil, now)
	if len(created) != 0 {
		t.Fatalf("memory 84%%: want no alert, got %+v", created)
	}
	created, _, _ = Evaluate(srv, latestWith("srv-1", memValue(85, now)), true, nil, nil, now)
	if len(created) != 1 || created[0].Kind != KindMemory {
		t.Fatalf("memory 85%%: want 1 memory alert, got %+v", created)
	}
	_, updated, recovered := Evaluate(srv, latestWith("srv-1", memValue(81, now)), true, nil, created, now)
	if len(updated) != 1 || len(recovered) != 0 {
		t.Fatalf("memory 81%%: want keep (updated=1), got updated=%d recovered=%d", len(updated), len(recovered))
	}
	_, _, recovered = Evaluate(srv, latestWith("srv-1", memValue(79, now)), true, nil, created, now)
	if len(recovered) != 1 {
		t.Fatalf("memory 79%%: want recovery, got %+v", recovered)
	}
	// Disk: per mount, 84% no trigger, 85% trigger, 81% keep, 79% recover.
	fs := func(pct float64) servercollect.Filesystem {
		return servercollect.Filesystem{Mount: "/", Total: 100, Used: uint64(pct), Percent: pct}
	}
	created, _, _ = Evaluate(srv, latestWith("srv-1", diskValue(now, fs(84))), true, nil, nil, now)
	if len(created) != 0 {
		t.Fatalf("disk 84%%: want no alert, got %+v", created)
	}
	created, _, _ = Evaluate(srv, latestWith("srv-1", diskValue(now, fs(85))), true, nil, nil, now)
	if len(created) != 1 || created[0].Kind != KindDisk || created[0].Mount != "/" {
		t.Fatalf("disk 85%%: want 1 disk alert for /, got %+v", created)
	}
	_, updated, recovered = Evaluate(srv, latestWith("srv-1", diskValue(now, fs(81))), true, nil, created, now)
	if len(updated) != 1 || len(recovered) != 0 {
		t.Fatalf("disk 81%%: want keep, got updated=%d recovered=%d", len(updated), len(recovered))
	}
	_, _, recovered = Evaluate(srv, latestWith("srv-1", diskValue(now, fs(79))), true, nil, created, now)
	if len(recovered) != 1 {
		t.Fatalf("disk 79%%: want recovery, got %+v", recovered)
	}
}

func TestEvaluateDiskPerMountIndependent(t *testing.T) {
	srv := testServer("srv-1", "web-1")
	now := time.Now()
	lat := latestWith("srv-1", diskValue(now,
		servercollect.Filesystem{Mount: "/", Total: 100, Used: 90, Percent: 90},
		servercollect.Filesystem{Mount: "/data", Total: 100, Used: 50, Percent: 50},
	))
	created, _, _ := Evaluate(srv, lat, true, nil, nil, now)
	if len(created) != 1 || created[0].Mount != "/" {
		t.Fatalf("want only / alert, got %+v", created)
	}
	if AlertID("srv-1", KindDisk, "/") == AlertID("srv-1", KindDisk, "/data") {
		t.Fatal("disk alert ids must differ per mount")
	}
}

func TestEvaluateExpiredOrMissingNeverTriggersOrRecovers(t *testing.T) {
	srv := testServer("srv-1", "web-1")
	now := time.Now()
	// Expired CPU at 99% must not trigger: the value is 10 minutes old
	// while StaleAfter is 3 minutes.
	expired := cpuValue(99, now.Add(-10*time.Minute))
	created, _, _ := Evaluate(srv, latestWith("srv-1", expired), true, nil, nil, now)
	if len(created) != 0 {
		t.Fatalf("expired 99%% CPU: want no alert, got %+v", created)
	}
	// A nil Percent (CPU baseline sample) is no value either.
	created, _, _ = Evaluate(srv, latestWith("srv-1", cpuBaselineValue(now)), true, nil, nil, now)
	if len(created) != 0 {
		t.Fatalf("baseline CPU (nil percent): want no alert, got %+v", created)
	}
	// An expired value must not recover an active alert.
	active, _, _ := Evaluate(srv, latestWith("srv-1", cpuValue(90, now)), true, nil, nil, now)
	if len(active) != 1 {
		t.Fatalf("setup: want 1 active alert, got %+v", active)
	}
	_, updated, recovered := Evaluate(srv, latestWith("srv-1", expired), true, nil, active, now)
	if len(recovered) != 0 || len(updated) != 0 {
		t.Fatalf("expired value: want no recovery/update, got updated=%d recovered=%d", len(updated), len(recovered))
	}
	// A missing value must not recover either.
	_, _, recovered = Evaluate(srv, latestWith("srv-1"), true, nil, active, now)
	if len(recovered) != 0 {
		t.Fatalf("missing value: want no recovery, got %+v", recovered)
	}
}

func TestEvaluateDuplicateMerges(t *testing.T) {
	srv := testServer("srv-1", "web-1")
	now := time.Now()
	created, _, _ := Evaluate(srv, latestWith("srv-1", cpuValue(90, now)), true, nil, nil, now)
	if len(created) != 1 {
		t.Fatalf("first: want 1 created, got %d", len(created))
	}
	// A second evaluation with the value still high must not create a
	// second record: it refreshes the same one.
	created2, updated, _ := Evaluate(srv, latestWith("srv-1", cpuValue(92, now.Add(time.Minute))), true, nil, created, now.Add(time.Minute))
	if len(created2) != 0 || len(updated) != 1 {
		t.Fatalf("second: want 0 created / 1 updated, got %d / %d", len(created2), len(updated))
	}
	if updated[0].ID != created[0].ID || updated[0].Value != 92 {
		t.Fatalf("second: want same id with new value 92, got %+v", updated[0])
	}
	// After recovery, a fresh breach creates a new incident again.
	_, _, recovered := Evaluate(srv, latestWith("srv-1", cpuValue(70, now.Add(2*time.Minute))), true, nil, updated, now.Add(2*time.Minute))
	if len(recovered) != 1 {
		t.Fatalf("recovery: want 1 recovered, got %+v", recovered)
	}
	created3, _, _ := Evaluate(srv, latestWith("srv-1", cpuValue(90, now.Add(3*time.Minute))), true, nil, recovered, now.Add(3*time.Minute))
	if len(created3) != 1 || created3[0].State != StateActive {
		t.Fatalf("re-trigger after recovery: want 1 new active alert, got %+v", created3)
	}
}

// --- SSH alerts and health ---

func TestEvaluateSSHAlertAndRecovery(t *testing.T) {
	srv := testServer("srv-1", "web-1")
	now := time.Now()
	failures := map[servercollect.MetricKind]string{
		servercollect.MetricConnectivity: "serversched: dial server srv-1 failed (network_unreachable): connection refused",
	}
	created, _, _ := Evaluate(srv, latestWith("srv-1"), false, failures, nil, now)
	if len(created) != 1 || created[0].Kind != KindSSH {
		t.Fatalf("SSH down: want 1 SSH alert, got %+v", created)
	}
	// A stage change (tcp -> handshake) updates the same alert, it does
	// not create a second one: the key carries no stage.
	failures[servercollect.MetricConnectivity] = "collect failed (stage handshake): ssh unavailable"
	created2, updated, _ := Evaluate(srv, latestWith("srv-1"), false, failures, created, now.Add(time.Minute))
	if len(created2) != 0 || len(updated) != 1 || updated[0].ID != created[0].ID {
		t.Fatalf("stage change: want 0 created / 1 updated same id, got %+v / %+v", created2, updated)
	}
	// A fresh connectivity success recovers it.
	_, _, recovered := Evaluate(srv, latestWith("srv-1", connValue(true, now.Add(2*time.Minute))), true, nil, updated, now.Add(2*time.Minute))
	if len(recovered) != 1 || recovered[0].State != StateRecovered || recovered[0].RecoveredAt == nil {
		t.Fatalf("fresh connectivity: want SSH recovery, got %+v", recovered)
	}
}

func TestDeriveHealthThreeLayers(t *testing.T) {
	now := time.Now()
	srv := testServer("srv-1", "web-1")
	cases := []struct {
		reason string
		want   HealthLevel
	}{
		{"serversched: dial server srv-1 failed (network_unreachable): refused", HealthNetwork},
		{"collect failed (stage tcp): dial timeout", HealthNetwork},
		{"serversched: dial server srv-1 failed (ssh_unavailable): handshake stalled", HealthSSH},
		{"collect failed (stage handshake): no ssh response", HealthSSH},
		{"serversched: dial server srv-1 failed (auth_failed): wrong password", HealthAuth},
		{"collect failed (stage auth): permission denied", HealthAuth},
		{"serversched: dial server srv-1 failed (fingerprint_changed)", HealthSSH},
		{"serversched: dial server srv-1 failed (jump_host_failed)", HealthSSH},
		{"serversched: dial server srv-1 failed (timeout): context deadline exceeded", HealthNetwork},
	}
	for _, tc := range cases {
		h := DeriveHealth(HealthInput{
			Server: srv, Latest: latestWith("srv-1"), HasLatest: false,
			LastErrors: map[servercollect.MetricKind]string{servercollect.MetricConnectivity: tc.reason},
			Now:        now,
		})
		if h.Level != tc.want {
			t.Errorf("reason %q: got level %s, want %s (detail %q)", tc.reason, h.Level, tc.want, h.Detail)
		}
		if !h.Stale {
			t.Errorf("reason %q: want Stale=true with no latest values", tc.reason)
		}
	}
}

func TestDeriveHealthFallbackLastCheckAndUnknown(t *testing.T) {
	now := time.Now()
	srv := testServer("srv-1", "web-1")
	// No LastError and no LastCheck: unknown.
	h := DeriveHealth(HealthInput{Server: srv, Latest: latestWith("srv-1"), Now: now})
	if h.Level != HealthUnknown {
		t.Fatalf("no signals: got %s, want unknown", h.Level)
	}
	// Fall back to the manual test: stage tcp -> network, and the detail
	// must say it came from the manual test.
	srv.LastCheck = &serverassets.CheckResult{At: now, OK: false, Stage: "tcp"}
	h = DeriveHealth(HealthInput{Server: srv, Latest: latestWith("srv-1"), Now: now})
	if h.Level != HealthNetwork {
		t.Fatalf("LastCheck tcp: got %s, want network", h.Level)
	}
	if h.Detail == "" {
		t.Fatal("LastCheck fallback detail must be set")
	}
	srv.LastCheck = &serverassets.CheckResult{At: now, OK: false, Stage: "auth"}
	h = DeriveHealth(HealthInput{Server: srv, Latest: latestWith("srv-1"), Now: now})
	if h.Level != HealthAuth {
		t.Fatalf("LastCheck auth: got %s, want auth", h.Level)
	}
	srv.LastCheck = &serverassets.CheckResult{At: now, OK: false, Stage: ""}
	h = DeriveHealth(HealthInput{Server: srv, Latest: latestWith("srv-1"), Now: now})
	if h.Level != HealthSSH {
		t.Fatalf("LastCheck failed without stage: got %s, want ssh", h.Level)
	}
	srv.LastCheck = &serverassets.CheckResult{At: now, OK: true, Stage: "auth"}
	h = DeriveHealth(HealthInput{Server: srv, Latest: latestWith("srv-1"), Now: now})
	if h.Level != HealthOK {
		t.Fatalf("LastCheck ok: got %s, want ok", h.Level)
	}
}

func TestDeriveHealthFreshConnectivityAndCollect(t *testing.T) {
	now := time.Now()
	srv := testServer("srv-1", "web-1")
	// Fresh connectivity OK and no failures: ok, not stale.
	h := DeriveHealth(HealthInput{
		Server: srv, Latest: latestWith("srv-1", connValue(true, now)), HasLatest: true, Now: now,
	})
	if h.Level != HealthOK || h.Stale {
		t.Fatalf("fresh conn ok: got %+v, want ok/not stale", h)
	}
	// Fresh connectivity OK but CPU failed with no fresh CPU value:
	// collect, and the active alert is reported.
	alert := Alert{ID: AlertID("srv-1", KindCollect, "cpu"), State: StateActive}
	h = DeriveHealth(HealthInput{
		Server: srv, Latest: latestWith("srv-1", connValue(true, now)), HasLatest: true,
		LastErrors:   map[servercollect.MetricKind]string{servercollect.MetricCPU: "cpu command timed out"},
		ActiveAlerts: []Alert{alert},
		Now:          now,
	})
	if h.Level != HealthCollect {
		t.Fatalf("cpu failure: got %s, want collect", h.Level)
	}
	if !h.HasAlert || len(h.ActiveAlerts) != 1 || h.ActiveAlerts[0] != alert.ID {
		t.Fatalf("cpu failure: HasAlert/ActiveAlerts wrong: %+v", h)
	}
	// A fresh CPU value despite the (stale) LastError entry means the
	// collection is fine: the failure no longer counts.
	h = DeriveHealth(HealthInput{
		Server: srv, Latest: latestWith("srv-1", connValue(true, now), cpuValue(10, now)), HasLatest: true,
		LastErrors: map[servercollect.MetricKind]string{servercollect.MetricCPU: "cpu command timed out"},
		Now:        now,
	})
	if h.Level != HealthOK {
		t.Fatalf("fresh cpu value: got %s, want ok", h.Level)
	}
}

// --- collection-failure alerts ---

func TestEvaluateCollectAlertOnlyWhenSSHHealthy(t *testing.T) {
	srv := testServer("srv-1", "web-1")
	now := time.Now()
	// SSH healthy (fresh connectivity OK) + CPU failure with no CPU
	// value: one collect alert, keyed by the failing kind.
	failures := map[servercollect.MetricKind]string{servercollect.MetricCPU: "cpu command failed"}
	created, _, _ := Evaluate(srv, latestWith("srv-1", connValue(true, now)), true, failures, nil, now)
	if len(created) != 1 || created[0].Kind != KindCollect || created[0].Mount != "cpu" {
		t.Fatalf("SSH ok + cpu failure: want 1 collect alert for cpu, got %+v", created)
	}
	// SSH down: the same CPU failure must NOT produce a collect alert
	// (it is a consequence of the connectivity problem); only the SSH
	// alert is created.
	failures[servercollect.MetricConnectivity] = "serversched: dial server srv-1 failed (network_unreachable)"
	created, _, _ = Evaluate(srv, latestWith("srv-1"), false, failures, nil, now)
	if len(created) != 1 || created[0].Kind != KindSSH {
		t.Fatalf("SSH down + cpu failure: want only the SSH alert, got %+v", created)
	}
	// The collect alert recovers when the kind has a fresh success value
	// and its LastError is gone.
	collect, _, _ := Evaluate(srv, latestWith("srv-1", connValue(true, now)), true,
		map[servercollect.MetricKind]string{servercollect.MetricCPU: "cpu command failed"}, nil, now)
	_, _, recovered := Evaluate(srv, latestWith("srv-1", connValue(true, now), cpuValue(10, now)), true, nil, collect, now)
	if len(recovered) != 1 || recovered[0].Kind != KindCollect {
		t.Fatalf("fresh cpu value: want collect recovery, got %+v", recovered)
	}
	// But an expired CPU value must not recover it.
	_, _, recovered = Evaluate(srv, latestWith("srv-1", connValue(true, now), cpuValue(10, now.Add(-10*time.Minute))), true, nil, collect, now)
	if len(recovered) != 0 {
		t.Fatalf("expired cpu value: want no recovery, got %+v", recovered)
	}
}

// --- statistics ---

func TestComputeStatsExcludesExpiredAndMissing(t *testing.T) {
	now := time.Now()
	servers := []serverassets.Server{
		testServer("srv-1", "web-1"),
		testServer("srv-2", "web-2"),
		testServer("srv-3", "web-3"),
		testServer("srv-4", "web-4"), // deleted: must not count at all
	}
	servers[3].Deleted = true
	latest := map[string]servercollect.Latest{
		// Fresh 50%: counts.
		"srv-1": latestWith("srv-1", cpuValue(50, now), memValue(40, now)),
		// Expired 99%: must not count, and must not drag the average.
		"srv-2": latestWith("srv-2", cpuValue(99, now.Add(-10*time.Minute)), memValue(99, now.Add(-10*time.Minute))),
		// srv-3 has no latest at all: missing is not zero.
	}
	health := map[string]ServerHealth{
		"srv-1": {Level: HealthOK},
		"srv-2": {Level: HealthOK},
		"srv-3": {Level: HealthUnknown},
		"srv-4": {Level: HealthOK},
	}
	stats := ComputeStats(servers, latest, health, now)
	if stats.Total != 3 {
		t.Fatalf("Total: got %d, want 3 (deleted excluded)", stats.Total)
	}
	if stats.CPUAvg == nil || *stats.CPUAvg != 50 || stats.CPUCount != 1 {
		t.Fatalf("CPU: got avg=%v count=%d, want 50/1", stats.CPUAvg, stats.CPUCount)
	}
	if stats.MemAvg == nil || *stats.MemAvg != 40 || stats.MemCount != 1 {
		t.Fatalf("Mem: got avg=%v count=%d, want 40/1", stats.MemAvg, stats.MemCount)
	}
	if stats.Online != 2 {
		t.Fatalf("Online: got %d, want 2", stats.Online)
	}
	// No fresh values at all: averages are nil (the caller shows
	// "暂无有效数据"), not zero.
	stats = ComputeStats(servers[:1], map[string]servercollect.Latest{
		"srv-1": latestWith("srv-1", cpuBaselineValue(now)),
	}, map[string]ServerHealth{"srv-1": {Level: HealthOK}}, now)
	if stats.CPUAvg != nil || stats.CPUCount != 0 {
		t.Fatalf("baseline only: got avg=%v count=%d, want nil/0", stats.CPUAvg, stats.CPUCount)
	}
}

func TestComputeStatsWeightedDiskAndHighDisks(t *testing.T) {
	now := time.Now()
	servers := []serverassets.Server{testServer("srv-1", "a-web"), testServer("srv-2", "b-db")}
	latest := map[string]servercollect.Latest{
		// 90/100 used: high. Two high mounts on the same server count
		// as one server.
		"srv-1": latestWith("srv-1", diskValue(now,
			servercollect.Filesystem{Mount: "/", Total: 100, Used: 90, Percent: 90},
			servercollect.Filesystem{Mount: "/data", Total: 200, Used: 190, Percent: 95},
		)),
		// 60/300 used: not high.
		"srv-2": latestWith("srv-2", diskValue(now,
			servercollect.Filesystem{Mount: "/", Total: 300, Used: 60, Percent: 20},
		)),
	}
	health := map[string]ServerHealth{"srv-1": {Level: HealthOK}, "srv-2": {Level: HealthOK}}
	stats := ComputeStats(servers, latest, health, now)
	// Capacity-weighted: (90+190+60)/(100+200+300) = 340/600 = 56.67%.
	if stats.DiskTotalBytes != 600 || stats.DiskUsedBytes != 340 {
		t.Fatalf("disk totals: got %d/%d, want 600/340", stats.DiskTotalBytes, stats.DiskUsedBytes)
	}
	want := 340.0 / 600.0 * 100
	if stats.DiskPercent == nil || *stats.DiskPercent < want-0.001 || *stats.DiskPercent > want+0.001 {
		t.Fatalf("disk percent: got %v, want %.4f", stats.DiskPercent, want)
	}
	if len(stats.HighDisks) != 2 {
		t.Fatalf("HighDisks: got %d, want 2", len(stats.HighDisks))
	}
	if stats.HighDisks[0].Mount != "/" || stats.HighDisks[1].Mount != "/data" {
		t.Fatalf("HighDisks not sorted by server/mount: %+v", stats.HighDisks)
	}
	if stats.HighDiskServers != 1 {
		t.Fatalf("HighDiskServers: got %d, want 1 (dedup per server)", stats.HighDiskServers)
	}
	// No fresh disk data: DiskPercent is nil.
	stats = ComputeStats(servers[:1], map[string]servercollect.Latest{}, health, now)
	if stats.DiskPercent != nil || stats.DiskTotalBytes != 0 {
		t.Fatalf("no disk data: got percent=%v total=%d, want nil/0", stats.DiskPercent, stats.DiskTotalBytes)
	}
}

func TestComputeStatsOnlineOfflineAbnormal(t *testing.T) {
	now := time.Now()
	servers := []serverassets.Server{
		testServer("srv-1", "ok"),
		testServer("srv-2", "collect-bad"),
		testServer("srv-3", "net-down"),
		testServer("srv-4", "ssh-down"),
		testServer("srv-5", "auth-bad"),
		testServer("srv-6", "ok-with-alert"),
	}
	health := map[string]ServerHealth{
		"srv-1": {Level: HealthOK},
		"srv-2": {Level: HealthCollect},
		"srv-3": {Level: HealthNetwork},
		"srv-4": {Level: HealthSSH},
		"srv-5": {Level: HealthAuth},
		// Level ok but an active alert still counts as abnormal.
		"srv-6": {Level: HealthOK, HasAlert: true, ActiveAlerts: []string{"x"}},
	}
	stats := ComputeStats(servers, nil, health, now)
	if stats.Total != 6 || stats.Online != 3 || stats.Offline != 2 || stats.Abnormal != 5 {
		t.Fatalf("got total=%d online=%d offline=%d abnormal=%d, want 6/3/2/5",
			stats.Total, stats.Online, stats.Offline, stats.Abnormal)
	}
}
