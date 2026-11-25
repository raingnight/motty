package serveralerts

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"motty/internal/configstore"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	raw, err := configstore.Open(filepath.Join(t.TempDir(), "config.db"), 1)
	if err != nil {
		t.Fatalf("open configstore: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	return NewStore(raw)
}

func testAlert(id, serverID string) Alert {
	return Alert{
		ID: id, ServerID: serverID, ServerName: "web-1", Kind: KindCPU,
		State: StateActive, Message: "cpu high", Value: 90, Threshold: CPUAlertThreshold,
	}
}

func TestStoreSaveListUpsert(t *testing.T) {
	s := openTestStore(t)
	a := testAlert(AlertID("srv-1", KindCPU, ""), "srv-1")
	if err := s.Save(a); err != nil {
		t.Fatalf("Save: %v", err)
	}
	list, err := s.List()
	if err != nil || len(list) != 1 {
		t.Fatalf("List: got %d alerts err=%v, want 1", len(list), err)
	}
	if list[0].CreatedAt.IsZero() || list[0].UpdatedAt.IsZero() {
		t.Fatalf("Save must fill CreatedAt/UpdatedAt: %+v", list[0])
	}
	createdAt := list[0].CreatedAt
	// Upserting the same id updates the record but preserves CreatedAt.
	time.Sleep(2 * time.Millisecond)
	a.Value = 95
	a.Message = "cpu higher"
	if err := s.Save(a); err != nil {
		t.Fatalf("Save upsert: %v", err)
	}
	list, err = s.List()
	if err != nil || len(list) != 1 {
		t.Fatalf("List after upsert: got %d alerts err=%v, want 1", len(list), err)
	}
	if list[0].Value != 95 || !list[0].CreatedAt.Equal(createdAt) {
		t.Fatalf("upsert must update value and preserve CreatedAt: %+v (want created %v)", list[0], createdAt)
	}
	if !list[0].UpdatedAt.After(createdAt) && !list[0].UpdatedAt.Equal(createdAt) {
		t.Fatalf("upsert must refresh UpdatedAt: %+v", list[0])
	}
}

func TestStoreSaveValidation(t *testing.T) {
	s := openTestStore(t)
	if err := s.Save(Alert{ServerID: "srv-1", Kind: KindCPU}); err == nil {
		t.Fatal("Save without id: want validation error")
	}
	if err := s.Save(Alert{ID: "x"}); err == nil {
		t.Fatal("Save without server/kind: want validation error")
	}
	if err := s.MarkRead(""); err == nil {
		t.Fatal("MarkRead without id: want validation error")
	}
	if err := s.MarkRead("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("MarkRead missing: got %v, want ErrNotFound", err)
	}
}

func TestStoreMarkReadAndMarkAllRead(t *testing.T) {
	s := openTestStore(t)
	for _, id := range []string{"a1", "a2"} {
		if err := s.Save(testAlert(id, "srv-1")); err != nil {
			t.Fatalf("Save %s: %v", id, err)
		}
	}
	if err := s.MarkRead("a1"); err != nil {
		t.Fatalf("MarkRead: %v", err)
	}
	list, _ := s.List()
	byID := map[string]Alert{}
	for _, a := range list {
		byID[a.ID] = a
	}
	if !byID["a1"].Read || byID["a2"].Read {
		t.Fatalf("MarkRead must read only a1: %+v", byID)
	}
	// Marking an already-read alert is a no-op, not an error.
	if err := s.MarkRead("a1"); err != nil {
		t.Fatalf("MarkRead again: %v", err)
	}
	if err := s.MarkAllRead(); err != nil {
		t.Fatalf("MarkAllRead: %v", err)
	}
	list, _ = s.List()
	for _, a := range list {
		if !a.Read {
			t.Fatalf("MarkAllRead: alert %s still unread", a.ID)
		}
	}
}

func TestStorePruneRecovered(t *testing.T) {
	s := openTestStore(t)
	now := time.Now().UTC()
	old := now.Add(-100 * 24 * time.Hour)
	recent := now.Add(-time.Hour)
	oldAlert := testAlert("old", "srv-1")
	oldAlert.State = StateRecovered
	oldAlert.RecoveredAt = &old
	recentAlert := testAlert("recent", "srv-1")
	recentAlert.State = StateRecovered
	recentAlert.RecoveredAt = &recent
	activeAlert := testAlert("active", "srv-1")
	for _, a := range []Alert{oldAlert, recentAlert, activeAlert} {
		if err := s.Save(a); err != nil {
			t.Fatalf("Save %s: %v", a.ID, err)
		}
	}
	if err := s.PruneRecovered(90 * 24 * time.Hour); err != nil {
		t.Fatalf("PruneRecovered: %v", err)
	}
	list, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	ids := map[string]bool{}
	for _, a := range list {
		ids[a.ID] = true
	}
	if ids["old"] {
		t.Fatal("prune must delete the 100-day-old recovered alert")
	}
	if !ids["recent"] || !ids["active"] {
		t.Fatalf("prune must keep recent recovered and active alerts: %v", ids)
	}
}

func TestStoreListNewestFirst(t *testing.T) {
	s := openTestStore(t)
	if err := s.Save(testAlert("first", "srv-1")); err != nil {
		t.Fatalf("Save first: %v", err)
	}
	time.Sleep(2 * time.Millisecond)
	if err := s.Save(testAlert("second", "srv-1")); err != nil {
		t.Fatalf("Save second: %v", err)
	}
	list, err := s.List()
	if err != nil || len(list) != 2 {
		t.Fatalf("List: got %d err=%v, want 2", len(list), err)
	}
	if list[0].ID != "second" || list[1].ID != "first" {
		t.Fatalf("List must be newest first: got %s, %s", list[0].ID, list[1].ID)
	}
}
