package serversched

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"motty/internal/servercollect"
)

// TestStorePutBufferedVisibleBeforeFlush: a buffered value is visible to
// Get and List at once, before any Flush.
func TestStorePutBufferedVisibleBeforeFlush(t *testing.T) {
	s := openTestStore(t)
	now := time.Now()
	if err := s.PutBuffered("srv-1", []servercollect.MetricValue{testValue(servercollect.MetricCPU, now)}); err != nil {
		t.Fatalf("PutBuffered: %v", err)
	}
	latest, found, err := s.Get("srv-1")
	if err != nil || !found {
		t.Fatalf("Get: found=%v err=%v", found, err)
	}
	if v, ok := latest.Get(servercollect.MetricCPU); !ok || !v.CollectedAt.Equal(now) {
		t.Fatalf("buffered value not visible: ok=%v value=%+v", ok, v)
	}
	list, err := s.List()
	if err != nil || len(list) != 1 || list[0].ServerID != "srv-1" {
		t.Fatalf("List: %v err=%v", list, err)
	}
	// A failed value must not overwrite the buffered success, and an
	// all-failure PutBuffered must not mark anything dirty.
	failed := servercollect.MetricValue{
		Kind: servercollect.MetricCPU, CollectedAt: now.Add(time.Minute),
		StaleAfter: time.Minute, Unavailable: "timeout",
	}
	if err := s.PutBuffered("srv-1", []servercollect.MetricValue{failed}); err != nil {
		t.Fatalf("PutBuffered failure: %v", err)
	}
	latest, _, _ = s.Get("srv-1")
	if v, ok := latest.Get(servercollect.MetricCPU); !ok || !v.CollectedAt.Equal(now) {
		t.Fatalf("failed value overwrote the buffered success: ok=%v value=%+v", ok, v)
	}
	if err := s.PutBuffered("srv-2", []servercollect.MetricValue{failed}); err != nil {
		t.Fatalf("PutBuffered failure for new server: %v", err)
	}
	if _, found, _ := s.Get("srv-2"); found {
		t.Fatal("all-failure PutBuffered created a record for srv-2")
	}
}

// TestStoreFlushPersistsBufferedValues: after Flush a fresh Store over
// the same database reads the buffered values.
func TestStoreFlushPersistsBufferedValues(t *testing.T) {
	raw := openTestRaw(t)
	s := NewStore(raw)
	now := time.Now()
	if err := s.PutBuffered("srv-1", []servercollect.MetricValue{
		testValue(servercollect.MetricCPU, now),
		testValue(servercollect.MetricMemory, now),
	}); err != nil {
		t.Fatalf("PutBuffered: %v", err)
	}
	if err := s.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	fresh := NewStore(raw)
	latest, found, err := fresh.Get("srv-1")
	if err != nil || !found {
		t.Fatalf("fresh Get: found=%v err=%v", found, err)
	}
	for _, kind := range []servercollect.MetricKind{servercollect.MetricCPU, servercollect.MetricMemory} {
		if v, ok := latest.Get(kind); !ok || !v.CollectedAt.Equal(now) {
			t.Fatalf("kind %s missing after Flush: ok=%v value=%+v", kind, ok, v)
		}
	}
}

// TestStoreCrashLosesUnflushedValues: dropping a Store without Flush is
// the crash simulation — the old durable value stays, the buffered one
// is absent, and reading it is not an error. Nothing is fabricated.
func TestStoreCrashLosesUnflushedValues(t *testing.T) {
	raw := openTestRaw(t)
	s := NewStore(raw)
	now := time.Now()
	if err := s.Put("srv-1", []servercollect.MetricValue{testValue(servercollect.MetricCPU, now)}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	later := now.Add(time.Minute)
	if err := s.PutBuffered("srv-1", []servercollect.MetricValue{
		testValue(servercollect.MetricCPU, later),
		testValue(servercollect.MetricMemory, later),
	}); err != nil {
		t.Fatalf("PutBuffered: %v", err)
	}
	// The buffering Store itself sees the new values; a fresh Store —
	// the process after a restart — does not.
	latest, _, _ := s.Get("srv-1")
	if v, ok := latest.Get(servercollect.MetricMemory); !ok || !v.CollectedAt.Equal(later) {
		t.Fatalf("buffering Store does not see its own buffered value: ok=%v value=%+v", ok, v)
	}
	restarted := NewStore(raw)
	latest, found, err := restarted.Get("srv-1")
	if err != nil || !found {
		t.Fatalf("restarted Get: found=%v err=%v", found, err)
	}
	if v, ok := latest.Get(servercollect.MetricCPU); !ok || !v.CollectedAt.Equal(now) {
		t.Fatalf("old durable value lost in crash: ok=%v value=%+v", ok, v)
	}
	if _, ok := latest.Get(servercollect.MetricMemory); ok {
		t.Fatal("unflushed value survived the simulated crash")
	}
}

// TestStoreFlushNoDirtyNoCommit: a Flush with nothing dirty commits
// nothing at all.
func TestStoreFlushNoDirtyNoCommit(t *testing.T) {
	s := openTestStore(t)
	before := s.commits
	if err := s.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if s.commits != before {
		t.Fatalf("Flush with no dirty server committed: commits %d -> %d", before, s.commits)
	}
	// A second Flush right after a successful one is likewise a no-op.
	now := time.Now()
	if err := s.PutBuffered("srv-1", []servercollect.MetricValue{testValue(servercollect.MetricCPU, now)}); err != nil {
		t.Fatalf("PutBuffered: %v", err)
	}
	if err := s.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	before = s.commits
	if err := s.Flush(); err != nil {
		t.Fatalf("second Flush: %v", err)
	}
	if s.commits != before {
		t.Fatalf("second Flush committed: commits %d -> %d", before, s.commits)
	}
	// Buffering a value identical to what is already on disk also
	// commits nothing: the flush finds nothing changed.
	if err := s.PutBuffered("srv-1", []servercollect.MetricValue{testValue(servercollect.MetricCPU, now)}); err != nil {
		t.Fatalf("PutBuffered same value: %v", err)
	}
	if err := s.Flush(); err != nil {
		t.Fatalf("Flush same value: %v", err)
	}
	if s.commits != before {
		t.Fatalf("Flush of an unchanged value committed: commits %d -> %d", before, s.commits)
	}
}

// TestStorePutBufferedConcurrentKinds: concurrent PutBuffered calls do
// not lose kinds, and one Flush writes them all.
func TestStorePutBufferedConcurrentKinds(t *testing.T) {
	raw := openTestRaw(t)
	s := NewStore(raw)
	now := time.Now()
	var wg sync.WaitGroup
	for _, kind := range servercollect.AllMetricKinds {
		wg.Add(1)
		go func(kind servercollect.MetricKind) {
			defer wg.Done()
			if err := s.PutBuffered("srv-1", []servercollect.MetricValue{testValue(kind, now)}); err != nil {
				t.Errorf("PutBuffered(%s): %v", kind, err)
			}
		}(kind)
	}
	wg.Wait()
	if err := s.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	fresh := NewStore(raw)
	latest, found, err := fresh.Get("srv-1")
	if err != nil || !found {
		t.Fatalf("fresh Get: found=%v err=%v", found, err)
	}
	for _, kind := range servercollect.AllMetricKinds {
		if _, ok := latest.Get(kind); !ok {
			t.Errorf("kind %s lost in concurrent PutBuffered", kind)
		}
	}
}

// TestStoreFlushConflictRetries: a write landing between Flush's load
// and commit makes the first attempt conflict; the retry reloads and
// the buffered value still lands, without losing the interfering write.
func TestStoreFlushConflictRetries(t *testing.T) {
	raw := openTestRaw(t)
	s1 := NewStore(raw)
	s2 := NewStore(raw)
	now := time.Now()
	if err := s2.Put("srv-1", []servercollect.MetricValue{testValue(servercollect.MetricCPU, now)}); err != nil {
		t.Fatalf("seed Put: %v", err)
	}
	later := now.Add(time.Minute)
	if err := s1.PutBuffered("srv-1", []servercollect.MetricValue{testValue(servercollect.MetricCPU, later)}); err != nil {
		t.Fatalf("PutBuffered: %v", err)
	}
	var hooked int32
	s1.flushPreCommit = func() {
		if atomic.AddInt32(&hooked, 1) == 1 {
			s1.flushPreCommit = nil
			// Change the disk record so s1's Expected guard fails.
			_ = s2.Put("srv-1", []servercollect.MetricValue{testValue(servercollect.MetricMemory, later)})
		}
	}
	if err := s1.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if atomic.LoadInt32(&hooked) != 1 {
		t.Fatalf("interfering write ran %d times, want 1", hooked)
	}
	latest, _, err := s2.Get("srv-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if v, ok := latest.Get(servercollect.MetricCPU); !ok || !v.CollectedAt.Equal(later) {
		t.Fatalf("buffered CPU value lost after conflict retry: ok=%v value=%+v", ok, v)
	}
	if _, ok := latest.Get(servercollect.MetricMemory); !ok {
		t.Fatal("interfering memory write lost after conflict retry")
	}
}

// TestStoreFlushFailureKeepsDirty: when Flush cannot commit, the dirty
// mark survives and the next Flush retries the same values.
func TestStoreFlushFailureKeepsDirty(t *testing.T) {
	raw := openTestRaw(t)
	s1 := NewStore(raw)
	s2 := NewStore(raw)
	now := time.Now()
	if err := s1.PutBuffered("srv-1", []servercollect.MetricValue{testValue(servercollect.MetricCPU, now)}); err != nil {
		t.Fatalf("PutBuffered: %v", err)
	}
	// Fail every commit attempt with a conflict that never resolves:
	// the hook rewrites the record with a different value before each
	// attempt, so the Expected guard fails every time.
	n := 0
	s1.flushPreCommit = func() {
		n++
		_ = s2.Put("srv-1", []servercollect.MetricValue{testValue(servercollect.MetricMemory, now.Add(time.Duration(n)*time.Second))})
	}
	if err := s1.Flush(); err == nil {
		t.Fatal("Flush with a permanent conflict succeeded, want an error")
	}
	s1.flushPreCommit = nil
	if !s1.dirty["srv-1"] {
		t.Fatal("failed Flush cleared the dirty mark: the value would be lost")
	}
	if err := s1.Flush(); err != nil {
		t.Fatalf("retry Flush: %v", err)
	}
	latest, _, err := s2.Get("srv-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, ok := latest.Get(servercollect.MetricCPU); !ok {
		t.Fatal("retried Flush did not write the buffered value")
	}
}

// TestStoreDeleteDropsBuffer: deleting a server removes its buffered
// values too, so a later Flush cannot resurrect them.
func TestStoreDeleteDropsBuffer(t *testing.T) {
	s := openTestStore(t)
	now := time.Now()
	if err := s.Put("srv-1", []servercollect.MetricValue{testValue(servercollect.MetricCPU, now)}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s.PutBuffered("srv-1", []servercollect.MetricValue{testValue(servercollect.MetricMemory, now)}); err != nil {
		t.Fatalf("PutBuffered: %v", err)
	}
	if err := s.Delete("srv-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := s.Flush(); err != nil {
		t.Fatalf("Flush after Delete: %v", err)
	}
	if _, found, _ := s.Get("srv-1"); found {
		t.Fatal("deleted server resurrected by Flush")
	}
}

// --- template assignments ---

func TestStoreTemplateAssignments(t *testing.T) {
	raw := openTestRaw(t)
	s := NewStore(raw)
	if got, err := s.ListTemplateAssignments(); err != nil || len(got) != 0 {
		t.Fatalf("ListTemplateAssignments on empty store: %v err=%v", got, err)
	}
	if err := s.SaveTemplateAssignment("srv-1", "fast"); err != nil {
		t.Fatalf("SaveTemplateAssignment: %v", err)
	}
	// Overwriting replaces the old assignment.
	if err := s.SaveTemplateAssignment("srv-1", "slow"); err != nil {
		t.Fatalf("overwrite SaveTemplateAssignment: %v", err)
	}
	if err := s.SaveTemplateAssignment("srv-2", "fast"); err != nil {
		t.Fatalf("SaveTemplateAssignment srv-2: %v", err)
	}
	got, err := s.ListTemplateAssignments()
	if err != nil || len(got) != 2 || got["srv-1"] != "slow" || got["srv-2"] != "fast" {
		t.Fatalf("ListTemplateAssignments: %v err=%v", got, err)
	}
	// A fresh Store over the same database reads the assignments back:
	// they survive a restart.
	restarted := NewStore(raw)
	got, err = restarted.ListTemplateAssignments()
	if err != nil || len(got) != 2 || got["srv-1"] != "slow" {
		t.Fatalf("restarted ListTemplateAssignments: %v err=%v", got, err)
	}
	// Assigning "default" leaves no record behind.
	if err := s.SaveTemplateAssignment("srv-1", "default"); err != nil {
		t.Fatalf("SaveTemplateAssignment default: %v", err)
	}
	got, err = s.ListTemplateAssignments()
	if err != nil || len(got) != 1 || got["srv-2"] != "fast" {
		t.Fatalf("ListTemplateAssignments after default: %v err=%v", got, err)
	}
	// Deleting an assignment is a no-op when there is none.
	if err := s.DeleteTemplateAssignment("srv-1"); err != nil {
		t.Fatalf("DeleteTemplateAssignment absent: %v", err)
	}
	if err := s.DeleteTemplateAssignment("srv-2"); err != nil {
		t.Fatalf("DeleteTemplateAssignment: %v", err)
	}
	got, err = s.ListTemplateAssignments()
	if err != nil || len(got) != 0 {
		t.Fatalf("ListTemplateAssignments after delete: %v err=%v", got, err)
	}
	// Validation.
	if err := s.SaveTemplateAssignment("", "fast"); err == nil {
		t.Fatal("SaveTemplateAssignment with empty server id succeeded")
	}
	if err := s.SaveTemplateAssignment("srv-1", ""); err == nil {
		t.Fatal("SaveTemplateAssignment with empty template id succeeded")
	}
	if err := s.DeleteTemplateAssignment(""); err == nil {
		t.Fatal("DeleteTemplateAssignment with empty server id succeeded")
	}
}

// TestStoreDeleteRemovesAssignment: deleting a server's metrics also
// removes its template assignment in the same commit.
func TestStoreDeleteRemovesAssignment(t *testing.T) {
	s := openTestStore(t)
	now := time.Now()
	if err := s.Put("srv-1", []servercollect.MetricValue{testValue(servercollect.MetricCPU, now)}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s.SaveTemplateAssignment("srv-1", "fast"); err != nil {
		t.Fatalf("SaveTemplateAssignment: %v", err)
	}
	if err := s.Delete("srv-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	got, err := s.ListTemplateAssignments()
	if err != nil || len(got) != 0 {
		t.Fatalf("assignment survived Delete: %v err=%v", got, err)
	}
}
