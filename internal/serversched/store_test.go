package serversched

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"motty/internal/configstore"
	"motty/internal/servercollect"
)

func openTestRaw(t *testing.T) *configstore.Store {
	t.Helper()
	raw, err := configstore.Open(filepath.Join(t.TempDir(), "config.db"), 1)
	if err != nil {
		t.Fatalf("open configstore: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	return raw
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	return NewStore(openTestRaw(t))
}

func testValue(kind servercollect.MetricKind, at time.Time) servercollect.MetricValue {
	v := servercollect.MetricValue{
		Kind:        kind,
		CollectedAt: at,
		StaleAfter:  servercollect.StaleFactor * servercollect.DefaultIntervals[kind],
	}
	switch kind {
	case servercollect.MetricBasic:
		v.Basic = &servercollect.BasicInfo{OSName: "Ubuntu", Kernel: "6.8.0"}
	case servercollect.MetricCPU:
		pct := 12.5
		v.CPU = &servercollect.CPUInfo{Percent: &pct, Load1: 0.5, Cores: 4}
	case servercollect.MetricMemory:
		pct := 40.0
		v.Memory = &servercollect.MemoryInfo{TotalBytes: 8 << 30, UsedBytes: 3 << 30, Percent: &pct}
	case servercollect.MetricDiskUsage:
		v.DiskUsage = &servercollect.DiskUsageInfo{Filesystems: []servercollect.Filesystem{{Device: "/dev/sda1", Mount: "/", Total: 100, Used: 40, Percent: 40}}}
	case servercollect.MetricDiskIO:
		v.DiskIO = &servercollect.DiskIOInfo{Devices: []servercollect.DiskDevice{{Name: "sda", Physical: true}}}
	case servercollect.MetricNetwork:
		v.Network = &servercollect.NetworkInfo{Interfaces: []servercollect.NetInterface{{Name: "eth0"}}}
	case servercollect.MetricNIC:
		v.NIC = &servercollect.NICInfo{Interfaces: []servercollect.NICDetail{{Name: "eth0"}}}
	case servercollect.MetricSystem:
		v.System = &servercollect.SystemInfo{UptimeSecs: 3600, ProcessesTotal: 120, ProcessesOK: true}
	case servercollect.MetricDocker:
		v.Docker = &servercollect.DockerInfo{Installed: true, Running: true}
	case servercollect.MetricConnectivity:
		v.Connectivity = &servercollect.ConnectivityInfo{OK: true, Stage: "auth"}
	}
	return v
}

func TestStorePutMergesPerKind(t *testing.T) {
	s := openTestStore(t)
	now := time.Now()
	if err := s.Put("srv-1", []servercollect.MetricValue{
		testValue(servercollect.MetricCPU, now),
		testValue(servercollect.MetricMemory, now),
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// A later Put carrying only memory must leave CPU untouched.
	later := now.Add(time.Minute)
	if err := s.Put("srv-1", []servercollect.MetricValue{testValue(servercollect.MetricMemory, later)}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	latest, found, err := s.Get("srv-1")
	if err != nil || !found {
		t.Fatalf("Get: found=%v err=%v", found, err)
	}
	cpu, ok := latest.Get(servercollect.MetricCPU)
	if !ok {
		t.Fatal("CPU value was overwritten by a memory-only Put")
	}
	if !cpu.CollectedAt.Equal(now) {
		t.Fatalf("CPU CollectedAt changed: %v, want %v", cpu.CollectedAt, now)
	}
	mem, ok := latest.Get(servercollect.MetricMemory)
	if !ok || !mem.CollectedAt.Equal(later) {
		t.Fatalf("memory was not updated: ok=%v value=%+v", ok, mem)
	}
}

func TestStorePutSkipsFailures(t *testing.T) {
	s := openTestStore(t)
	now := time.Now()
	if err := s.Put("srv-1", []servercollect.MetricValue{testValue(servercollect.MetricCPU, now)}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// An unavailable CPU value must not overwrite the success, and an
	// all-failure Put for a new server must write nothing at all.
	failed := servercollect.MetricValue{
		Kind: servercollect.MetricCPU, CollectedAt: now.Add(time.Minute),
		StaleAfter: time.Minute, Unavailable: "timeout",
	}
	if err := s.Put("srv-1", []servercollect.MetricValue{failed}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s.Put("srv-2", []servercollect.MetricValue{failed}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	latest, _, _ := s.Get("srv-1")
	cpu, ok := latest.Get(servercollect.MetricCPU)
	if !ok || !cpu.CollectedAt.Equal(now) {
		t.Fatalf("failed value overwrote the success: ok=%v value=%+v", ok, cpu)
	}
	if _, found, _ := s.Get("srv-2"); found {
		t.Fatal("all-failure Put created a record for srv-2")
	}
}

func TestStorePutConcurrentPerKind(t *testing.T) {
	s := openTestStore(t)
	now := time.Now()
	var wg sync.WaitGroup
	for _, kind := range servercollect.AllMetricKinds {
		wg.Add(1)
		go func(kind servercollect.MetricKind) {
			defer wg.Done()
			if err := s.Put("srv-1", []servercollect.MetricValue{testValue(kind, now)}); err != nil {
				t.Errorf("Put(%s): %v", kind, err)
			}
		}(kind)
	}
	wg.Wait()
	latest, found, err := s.Get("srv-1")
	if err != nil || !found {
		t.Fatalf("Get: found=%v err=%v", found, err)
	}
	for _, kind := range servercollect.AllMetricKinds {
		if _, ok := latest.Get(kind); !ok {
			t.Errorf("kind %s lost in concurrent Put", kind)
		}
	}
}

func TestStoreListAndDelete(t *testing.T) {
	s := openTestStore(t)
	now := time.Now()
	if err := s.Put("srv-b", []servercollect.MetricValue{testValue(servercollect.MetricCPU, now)}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s.Put("srv-a", []servercollect.MetricValue{testValue(servercollect.MetricCPU, now)}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	list, err := s.List()
	if err != nil || len(list) != 2 || list[0].ServerID != "srv-a" || list[1].ServerID != "srv-b" {
		t.Fatalf("List: %v err=%v", list, err)
	}
	if err := s.Delete("srv-a"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, found, _ := s.Get("srv-a"); found {
		t.Fatal("deleted server still present")
	}
	// Deleting again is a no-op, not an error.
	if err := s.Delete("srv-a"); err != nil {
		t.Fatalf("second Delete: %v", err)
	}
	if _, found, _ := s.Get("srv-b"); !found {
		t.Fatal("Delete removed the wrong server")
	}
}

func TestStoreTemplates(t *testing.T) {
	s := openTestStore(t)
	// The default template is virtual: present before anything is saved.
	tmpl, found, err := s.GetTemplate("default")
	if err != nil || !found || tmpl.ID != "default" {
		t.Fatalf("GetTemplate(default): found=%v err=%v tmpl=%+v", found, err, tmpl)
	}
	if got := tmpl.Interval(servercollect.MetricCPU); got != time.Minute {
		t.Fatalf("default CPU interval = %v, want 1m", got)
	}
	if _, found, _ := s.GetTemplate("no-such"); found {
		t.Fatal("unknown template reported as found")
	}
	// The default template may never be deleted.
	if err := s.DeleteTemplate("default"); !errors.Is(err, ErrDefaultTemplate) {
		t.Fatalf("DeleteTemplate(default) = %v, want ErrDefaultTemplate", err)
	}
	custom := servercollect.Template{ID: "fast", Name: "Fast", Intervals: map[servercollect.MetricKind]time.Duration{
		servercollect.MetricCPU: 10 * time.Second,
	}}
	if err := s.SaveTemplate(custom); err != nil {
		t.Fatalf("SaveTemplate: %v", err)
	}
	if err := s.SaveTemplate(servercollect.Template{}); !errors.Is(err, ErrValidation) {
		t.Fatalf("SaveTemplate with empty ID = %v, want ErrValidation", err)
	}
	list, err := s.ListTemplates()
	if err != nil || len(list) != 2 || list[0].ID != "default" || list[1].ID != "fast" {
		t.Fatalf("ListTemplates: %v err=%v", list, err)
	}
	got, found, err := s.GetTemplate("fast")
	if err != nil || !found || got.Interval(servercollect.MetricCPU) != 10*time.Second {
		t.Fatalf("GetTemplate(fast): found=%v err=%v tmpl=%+v", found, err, got)
	}
	// A custom template falls back to the default interval for kinds it
	// does not configure.
	if got := got.Interval(servercollect.MetricMemory); got != time.Minute {
		t.Fatalf("fast memory interval = %v, want default 1m", got)
	}
	if err := s.DeleteTemplate("fast"); err != nil {
		t.Fatalf("DeleteTemplate: %v", err)
	}
	if _, found, _ := s.GetTemplate("fast"); found {
		t.Fatal("deleted template still present")
	}
}

func TestExpiryBoundary(t *testing.T) {
	// StaleAfter is 3x the 1m CPU interval = 3m. Exactly at the boundary
	// the value is still current; one second past it, it is expired.
	now := time.Now()
	collected := now.Add(-3 * time.Minute)
	v := testValue(servercollect.MetricCPU, collected)
	if v.Expired(now) {
		t.Fatal("value exactly at StaleAfter boundary counts as expired")
	}
	if !v.Expired(now.Add(time.Second)) {
		t.Fatal("value past StaleAfter boundary does not count as expired")
	}
	latest := servercollect.Latest{ServerID: "srv-1", Values: map[servercollect.MetricKind]servercollect.MetricValue{
		servercollect.MetricCPU: v,
	}}
	if latest.ExpiredKind(servercollect.MetricCPU, now) {
		t.Fatal("Latest.ExpiredKind disagrees at the boundary")
	}
	if !latest.ExpiredKind(servercollect.MetricCPU, now.Add(time.Second)) {
		t.Fatal("Latest.ExpiredKind misses expiry past the boundary")
	}
	// A missing kind is expired, and a zero CollectedAt is expired.
	if !latest.ExpiredKind(servercollect.MetricMemory, now) {
		t.Fatal("missing kind does not count as expired")
	}
	zero := servercollect.MetricValue{Kind: servercollect.MetricMemory, StaleAfter: time.Minute}
	if !zero.Expired(now) {
		t.Fatal("zero CollectedAt does not count as expired")
	}
}
