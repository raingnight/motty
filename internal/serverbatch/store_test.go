package serverbatch

import (
	"path/filepath"
	"testing"
	"time"

	"motty/internal/configstore"
)

func openTestStore(t *testing.T) *configstore.Store {
	t.Helper()
	raw, err := configstore.Open(filepath.Join(t.TempDir(), "config.db"), 1)
	if err != nil {
		t.Fatalf("open configstore: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	return raw
}

func TestBuiltinTemplates(t *testing.T) {
	builtins := BuiltinTemplates()
	if len(builtins) != 4 {
		t.Fatalf("BuiltinTemplates() = %d, want 4", len(builtins))
	}
	names := map[string]bool{}
	for _, tpl := range builtins {
		if tpl.ID == "" || tpl.Name == "" || tpl.Command == "" {
			t.Errorf("builtin template %+v has an empty field", tpl)
		}
		if names[tpl.Name] {
			t.Errorf("duplicate builtin name %q", tpl.Name)
		}
		names[tpl.Name] = true
		// The stored grade must match a fresh classification.
		if tpl.Risk != ClassifyRisk(tpl.Command).Level {
			t.Errorf("builtin %q risk %v != ClassifyRisk %v", tpl.Name, tpl.Risk, ClassifyRisk(tpl.Command).Level)
		}
	}
	if !names["系统更新"] || !names["服务重启"] || !names["日志查看"] || !names["磁盘清理"] {
		t.Errorf("builtin names = %v", names)
	}
}

func TestTemplateStoreCRUD(t *testing.T) {
	s := NewTemplateStore(openTestStore(t))

	// Validation.
	if _, err := s.Create(Template{Name: "", Command: "ls"}); err == nil {
		t.Error("Create with empty name should fail")
	}
	if _, err := s.Create(Template{Name: "x", Command: ""}); err == nil {
		t.Error("Create with empty command should fail")
	}

	created, err := s.Create(Template{Name: "重启 nginx", Command: "systemctl restart nginx", Description: "d"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.ID == "" {
		t.Fatal("ID is empty")
	}
	// Risk is re-derived from the command, not taken from the caller.
	if created.Risk != RiskHigh {
		t.Errorf("created Risk = %v, want high", created.Risk)
	}
	if created.CreatedAt.IsZero() || created.UpdatedAt.IsZero() {
		t.Error("timestamps not set")
	}

	got, ok := s.Get(created.ID)
	if !ok || got.Name != "重启 nginx" {
		t.Fatalf("Get = %+v, %v", got, ok)
	}
	if _, ok := s.Get("missing"); ok {
		t.Error("Get of a missing id should return false")
	}

	// Update re-grades and preserves CreatedAt.
	created.Command = "ls /tmp"
	created.Name = "列出 tmp"
	if err := s.Update(created); err != nil {
		t.Fatalf("Update: %v", err)
	}
	updated, ok := s.Get(created.ID)
	if !ok {
		t.Fatal("Get after Update failed")
	}
	if updated.Risk != RiskNormal {
		t.Errorf("updated Risk = %v, want normal", updated.Risk)
	}
	if !updated.CreatedAt.Equal(created.CreatedAt) {
		t.Error("CreatedAt changed on update")
	}
	if err := s.Update(Template{ID: "missing", Name: "x", Command: "ls"}); err == nil {
		t.Error("Update of a missing id should fail")
	}

	// List is sorted by name; builtins are never stored.
	if _, err := s.Create(Template{Name: "AAA", Command: "uptime"}); err != nil {
		t.Fatalf("Create AAA: %v", err)
	}
	list := s.List()
	if len(list) != 2 {
		t.Fatalf("List = %d templates, want 2", len(list))
	}
	if list[0].Name != "AAA" || list[1].Name != "列出 tmp" {
		t.Errorf("List order = %q, %q", list[0].Name, list[1].Name)
	}

	if err := s.Delete(created.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := s.Get(created.ID); ok {
		t.Error("template still present after Delete")
	}
	if err := s.Delete("missing"); err != nil {
		t.Errorf("Delete of a missing id should be a no-op, got %v", err)
	}
	// Built-in templates are virtual: they cannot be deleted or updated.
	if err := s.Delete("builtin-system-update"); err == nil {
		t.Error("Delete of a builtin id should fail")
	}
	if err := s.Update(Template{ID: "builtin-system-update", Name: "x", Command: "ls"}); err == nil {
		t.Error("Update of a builtin id should fail")
	}
}

func TestTemplateStoreIsolatedKinds(t *testing.T) {
	// A history record in the same store must not leak into templates.
	raw := openTestStore(t)
	tplStore := NewTemplateStore(raw)
	histStore := NewHistoryStore(raw)
	run := &Run{ID: newUUID(), Command: "ls", CreatedAt: nowUTC(), FinishedAt: nowUTC()}
	if err := histStore.SaveRun(run); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}
	if list := tplStore.List(); len(list) != 0 {
		t.Errorf("templates leaked history records: %d", len(list))
	}
	if runs := histStore.ListRuns(HistoryFilter{}); len(runs) != 1 {
		t.Errorf("history = %d runs, want 1", len(runs))
	}
}

func testRun(id string, created time.Time, targets ...ServerResult) *Run {
	return &Run{
		ID: id, Command: "uptime", CreatedAt: created,
		StartedAt: created, FinishedAt: created.Add(time.Second),
		State: RunFinished, Targets: targets,
	}
}

func TestHistoryStoreSaveGetList(t *testing.T) {
	s := NewHistoryStore(openTestStore(t))
	now := nowUTC()
	r1 := testRun("run-1", now.Add(-2*time.Hour),
		ServerResult{ServerID: "srv-a", ServerName: "a", State: StateSuccess},
		ServerResult{ServerID: "srv-b", ServerName: "b", State: StateFailed, Error: "拨号失败"},
	)
	r2 := testRun("run-2", now.Add(-time.Hour),
		ServerResult{ServerID: "srv-a", ServerName: "a", State: StateTimeout},
	)
	for _, r := range []*Run{r1, r2} {
		if err := s.SaveRun(r); err != nil {
			t.Fatalf("SaveRun %s: %v", r.ID, err)
		}
	}
	if err := s.SaveRun(nil); err == nil {
		t.Error("SaveRun(nil) should fail")
	}
	if err := s.SaveRun(&Run{Command: "ls"}); err == nil {
		t.Error("SaveRun without an id should fail")
	}

	got, ok := s.GetRun("run-1")
	if !ok || got.Command != "uptime" || len(got.Targets) != 2 {
		t.Fatalf("GetRun = %+v, %v", got, ok)
	}
	if _, ok := s.GetRun("missing"); ok {
		t.Error("GetRun of a missing id should return false")
	}

	// Newest first.
	runs := s.ListRuns(HistoryFilter{})
	if len(runs) != 2 || runs[0].ID != "run-2" || runs[1].ID != "run-1" {
		t.Fatalf("ListRuns order = %v", runIDs(runs))
	}
	// Limit.
	if runs := s.ListRuns(HistoryFilter{Limit: 1}); len(runs) != 1 || runs[0].ID != "run-2" {
		t.Errorf("Limit 1 = %v", runIDs(runs))
	}
	// Server filter.
	if runs := s.ListRuns(HistoryFilter{ServerID: "srv-b"}); len(runs) != 1 || runs[0].ID != "run-1" {
		t.Errorf("ServerID srv-b = %v", runIDs(runs))
	}
	if runs := s.ListRuns(HistoryFilter{ServerID: "srv-missing"}); len(runs) != 0 {
		t.Errorf("ServerID missing = %v", runIDs(runs))
	}
	// State filter: a run matches when any target has that state.
	if runs := s.ListRuns(HistoryFilter{State: StateFailed, HasState: true}); len(runs) != 1 || runs[0].ID != "run-1" {
		t.Errorf("State failed = %v", runIDs(runs))
	}
	if runs := s.ListRuns(HistoryFilter{State: StateSuccess, HasState: true}); len(runs) != 1 || runs[0].ID != "run-1" {
		t.Errorf("State success = %v", runIDs(runs))
	}
	// Time filter on CreatedAt.
	from := now.Add(-90 * time.Minute)
	if runs := s.ListRuns(HistoryFilter{From: from}); len(runs) != 1 || runs[0].ID != "run-2" {
		t.Errorf("From = %v", runIDs(runs))
	}
	to := now.Add(-90 * time.Minute)
	if runs := s.ListRuns(HistoryFilter{To: to}); len(runs) != 1 || runs[0].ID != "run-1" {
		t.Errorf("To = %v", runIDs(runs))
	}

	// SaveRun upserts: re-saving the same id replaces it.
	r1.Command = "df -h"
	if err := s.SaveRun(r1); err != nil {
		t.Fatalf("SaveRun upsert: %v", err)
	}
	got, _ = s.GetRun("run-1")
	if got.Command != "df -h" {
		t.Errorf("upsert did not replace, command = %q", got.Command)
	}
	if runs := s.ListRuns(HistoryFilter{}); len(runs) != 2 {
		t.Errorf("upsert duplicated runs: %d", len(runs))
	}

	if err := s.DeleteRun("run-1"); err != nil {
		t.Fatalf("DeleteRun: %v", err)
	}
	if _, ok := s.GetRun("run-1"); ok {
		t.Error("run still present after DeleteRun")
	}
	if err := s.DeleteRun("missing"); err != nil {
		t.Errorf("DeleteRun of a missing id should be a no-op, got %v", err)
	}
}

func runIDs(runs []*Run) []string {
	ids := make([]string, 0, len(runs))
	for _, r := range runs {
		ids = append(ids, r.ID)
	}
	return ids
}

func TestHistoryStorePruneBoundary(t *testing.T) {
	s := NewHistoryStore(openTestStore(t))
	now := nowUTC()
	// 1 second inside retention: kept. 30 days + 1 second: deleted.
	// (Exact equality cannot be tested against the wall clock: the
	// milliseconds between SaveRun and Prune would move it over the
	// line. The implementation keeps FinishedAt == cutoff via
	// !Before, i.e. exactly 30 days old is retained.)
	kept := testRun("run-kept", now.Add(-30*24*time.Hour))
	kept.FinishedAt = now.Add(-30*24*time.Hour + time.Second)
	old := testRun("run-old", now.Add(-31*24*time.Hour))
	old.FinishedAt = now.Add(-30*24*time.Hour - time.Second)
	fresh := testRun("run-fresh", now)
	unfinished := &Run{ID: "run-unfinished", Command: "sleep 30", CreatedAt: now.Add(-40 * 24 * time.Hour), State: RunRunning}
	for _, r := range []*Run{kept, old, fresh, unfinished} {
		if err := s.SaveRun(r); err != nil {
			t.Fatalf("SaveRun %s: %v", r.ID, err)
		}
	}
	deleted, err := s.Prune()
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("Prune deleted %d, want 1", deleted)
	}
	if _, ok := s.GetRun("run-old"); ok {
		t.Error("old run was not pruned")
	}
	if _, ok := s.GetRun("run-kept"); !ok {
		t.Error("run finished exactly 30 days ago was pruned, want kept")
	}
	if _, ok := s.GetRun("run-fresh"); !ok {
		t.Error("fresh run was pruned")
	}
	if _, ok := s.GetRun("run-unfinished"); !ok {
		t.Error("unfinished run was pruned, want kept")
	}
	deleted, err = s.Prune()
	if err != nil || deleted != 0 {
		t.Errorf("second Prune = %d, %v, want 0, nil", deleted, err)
	}
}
