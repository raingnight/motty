package workbench

import (
	"strings"
	"testing"
	"time"

	"github.com/vivaui/viva/gfx"
	"motty/internal/i18n"
	"motty/internal/serverassets"
	"motty/internal/serverbatch"
)

// batchTestPort hands each test server its own loopback port: the store
// rejects two servers on the same address and port, and every port here
// is closed, so dials fail fast.
var batchTestPort = 20000

// addBatchTestServer stores one unreachable server (loopback, closed port,
// so dials fail fast) and reloads the rows.
func addBatchTestServer(t *testing.T, state *State, name, groupID, tag, os string) serverassets.Server {
	t.Helper()
	store := state.serversStore()
	if store == nil {
		t.Fatal("serversStore() = nil")
	}
	batchTestPort++
	server, err := store.CreateServer(serverassets.Server{
		Name: name, Address: "127.0.0.1", Port: batchTestPort, User: "root",
		AuthMethod: serverassets.AuthPassword,
		GroupID:    groupID, Tags: []string{tag}, OS: os,
	})
	if err != nil {
		t.Fatalf("CreateServer(%s): %v", name, err)
	}
	state.refreshServers(false)
	return server
}

// installTestPost routes window-thread posts through a channel the test
// drains: with no window, serversPost would otherwise run the posted
// work on the background goroutine that posted it, racing the test's
// reads of the same state. Draining on the test goroutine keeps every
// state mutation on one goroutine. Call the returned drain function
// inside any wait loop before reading state.
func installTestPost(t *testing.T, state *State) func() {
	t.Helper()
	posted := make(chan func(), 128)
	state.SetPost(func(fn func()) { posted <- fn })
	return func() {
		for {
			select {
			case fn := <-posted:
				fn()
			default:
				return
			}
		}
	}
}

func waitBatchDone(t *testing.T, state *State, drain func()) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		drain()
		if !state.servers.batch.running {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	drain()
	t.Fatal("batch run did not finish within 30s")
}

// The risk grade decides the confirmation: normal runs at once, high
// needs a second confirmation, critical needs the phrase, blocked is
// refused.
func TestBatchConfirmStage(t *testing.T) {
	cases := []struct {
		level serverbatch.RiskLevel
		stage int
	}{
		{serverbatch.RiskNormal, 0},
		{serverbatch.RiskHigh, 1},
		{serverbatch.RiskCritical, 2},
		{serverbatch.RiskBlocked, -1},
	}
	for _, tc := range cases {
		if got := batchConfirmStage(tc.level); got != tc.stage {
			t.Fatalf("batchConfirmStage(%v) = %d, want %d", tc.level, got, tc.stage)
		}
	}
	if got := batchRiskLabel(serverbatch.RiskCritical); got != "极高风险" {
		t.Fatalf("batchRiskLabel(critical) = %q, want 极高风险", got)
	}
	if got := batchResultStateText(serverbatch.StateTimeout); got != "超时" {
		t.Fatalf("batchResultStateText(timeout) = %q, want 超时", got)
	}
}

// Target filtering reuses the servers list's rules: group, tag and OS,
// non-deleted only, name order.
func TestBatchTargetServers(t *testing.T) {
	rows := []serverassets.Server{
		{ID: "b", Name: "beta", GroupID: "g1", Tags: []string{"web"}, OS: "Ubuntu"},
		{ID: "a", Name: "alpha", GroupID: "g1", Tags: []string{"web"}, OS: "Ubuntu"},
		{ID: "c", Name: "gamma", GroupID: "g2", Tags: []string{"db"}, OS: "Debian"},
		{ID: "d", Name: "gone", GroupID: "g1", Tags: []string{"web"}, OS: "Ubuntu", Deleted: true},
	}
	got := batchTargetServers(rows, "g1", "", "")
	if len(got) != 2 || got[0].Name != "alpha" || got[1].Name != "beta" {
		t.Fatalf("group filter = %v, want [alpha beta]", got)
	}
	if got := batchTargetServers(rows, "", "db", ""); len(got) != 1 || got[0].Name != "gamma" {
		t.Fatalf("tag filter = %v, want [gamma]", got)
	}
	if got := batchTargetServers(rows, "", "", "Debian"); len(got) != 1 || got[0].Name != "gamma" {
		t.Fatalf("os filter = %v, want [gamma]", got)
	}
}

// Output summaries show the first non-empty line, capped; the run
// summary counts every outcome.
func TestBatchRunSummaryAndOutputSummary(t *testing.T) {
	if got := batchOutputSummary("\n\n  df -h output line  \nsecond"); got != "df -h output line" {
		t.Fatalf("output summary = %q, want the first line", got)
	}
	if got := batchOutputSummary(""); got != "—" {
		t.Fatalf("empty output summary = %q, want —", got)
	}
	long := strings.Repeat("x", 100)
	if got := batchOutputSummary(long); len([]rune(got)) != 61 || !strings.HasSuffix(got, "…") {
		t.Fatalf("long output summary = %q, want 60 chars + …", got)
	}
	run := &serverbatch.Run{Targets: []serverbatch.ServerResult{
		{State: serverbatch.StateSuccess},
		{State: serverbatch.StateFailed},
		{State: serverbatch.StateTimeout},
		{State: serverbatch.StateCancelled},
		{State: serverbatch.StateSkipped},
	}}
	summary := batchRunSummary(run)
	for _, want := range []string{"成功 1 台", "失败 1 台", "超时 1 台", "取消 1 台", "跳过 1 台"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("run summary = %q, want it to contain %q", summary, want)
		}
	}
}

// A blocked command is refused with its reasons and never starts.
func TestBatchRequestBlockedRefused(t *testing.T) {
	state := newTestState(t, testOptions(t))
	state.openServersBatch()
	addBatchTestServer(t, state, "web", "", "web", "Ubuntu")
	targets := state.batchSelectedServers()
	if len(targets) != 0 {
		t.Fatal("nothing is selected yet")
	}
	server, _ := state.serverByID(state.servers.rows[0].ID)
	state.requestServersBatchRun("rm -rf /", "", []serverassets.Server{server})
	view := &state.servers.batch
	if view.running || view.executor != nil {
		t.Fatal("a blocked command started a run")
	}
	if !strings.Contains(view.errorText, "blocked") {
		t.Fatalf("blocked error = %q, want it to say the command is blocked", view.errorText)
	}
}

// A high-risk command waits on the second confirmation dialog, then
// runs; the finished run is saved to history.
func TestBatchHighConfirmFlow(t *testing.T) {
	state := newTestState(t, testOptions(t))
	drain := installTestPost(t, state)
	state.openServersBatch()
	server := addBatchTestServer(t, state, "web", "", "web", "Ubuntu")
	state.requestServersBatchRun("sudo systemctl restart nginx", "", []serverassets.Server{server})
	view := &state.servers.batch
	if view.confirm.stage != 1 {
		t.Fatalf("confirm stage = %d, want 1 for a high-risk command", view.confirm.stage)
	}
	if !state.clickServersBatch(hitServersBatchDialogConfirm) {
		t.Fatal("confirm click was not handled")
	}
	if !view.running {
		t.Fatal("confirming a high-risk command did not start the run")
	}
	waitBatchDone(t, state, drain)
	if view.run == nil {
		t.Fatal("finished run is nil")
	}
	if len(view.run.Targets) != 1 || view.run.Targets[0].State != serverbatch.StateFailed {
		t.Fatalf("run targets = %+v, want one failed target (unreachable server)", view.run.Targets)
	}
	state.refreshServersBatchHistory()
	if len(view.history) != 1 {
		t.Fatalf("history = %d runs, want 1 after a finished run", len(view.history))
	}
}

// A critical-risk command needs the exact phrase: a wrong phrase stays
// in the dialog, the right one starts the run.
func TestBatchCriticalPhraseFlow(t *testing.T) {
	state := newTestState(t, testOptions(t))
	drain := installTestPost(t, state)
	state.openServersBatch()
	server := addBatchTestServer(t, state, "web", "", "web", "Ubuntu")
	state.requestServersBatchRun("rm -rf /tmp/cache", "", []serverassets.Server{server})
	view := &state.servers.batch
	if view.confirm.stage != 2 {
		t.Fatalf("confirm stage = %d, want 2 for a critical command", view.confirm.stage)
	}
	setEditor(&view.confirmPhrase, "wrong words")
	if !state.clickServersBatch(hitServersBatchDialogConfirm) {
		t.Fatal("confirm click was not handled")
	}
	if view.confirm.stage != 2 || view.confirmErr == "" {
		t.Fatal("a wrong phrase did not keep the dialog open with an error")
	}
	if view.running {
		t.Fatal("a wrong phrase started the run")
	}
	setEditor(&view.confirmPhrase, serverbatch.ConfirmPhrase)
	if !state.clickServersBatch(hitServersBatchDialogConfirm) {
		t.Fatal("confirm click was not handled")
	}
	if !view.running {
		t.Fatal("the exact phrase did not start the run")
	}
	waitBatchDone(t, state, drain)
}

// A normal command runs at once with no dialog, and retry re-runs only
// the failed servers.
func TestBatchNormalRunAndRetry(t *testing.T) {
	state := newTestState(t, testOptions(t))
	drain := installTestPost(t, state)
	state.openServersBatch()
	server := addBatchTestServer(t, state, "web", "", "web", "Ubuntu")
	state.requestServersBatchRun("uptime", "", []serverassets.Server{server})
	view := &state.servers.batch
	if view.confirm.stage != 0 {
		t.Fatal("a normal command opened a confirmation dialog")
	}
	waitBatchDone(t, state, drain)
	if view.run == nil || len(view.run.Targets) != 1 {
		t.Fatalf("run = %+v, want one target", view.run)
	}
	if view.run.Targets[0].State != serverbatch.StateFailed {
		t.Fatalf("target state = %v, want failed (unreachable server)", view.run.Targets[0].State)
	}
	if !state.clickServersBatch(hitServersBatchRetry) {
		t.Fatal("retry click was not handled")
	}
	if !view.running {
		t.Fatal("retry did not start a run")
	}
	waitBatchDone(t, state, drain)
	if view.run == nil || len(view.run.Targets) != 1 {
		t.Fatalf("retried run = %+v, want one target", view.run)
	}
}

// Target selection: select-all, clear and single-row toggle through the
// click dispatcher.
func TestBatchTargetSelectionClicks(t *testing.T) {
	state := newTestState(t, testOptions(t))
	state.openServersBatch()
	addBatchTestServer(t, state, "alpha", "", "web", "Ubuntu")
	addBatchTestServer(t, state, "beta", "", "web", "Ubuntu")
	view := &state.servers.batch
	if !state.clickServersBatch(hitServersBatchSelectAll) {
		t.Fatal("select-all click was not handled")
	}
	if len(state.batchSelectedServers()) != 2 {
		t.Fatalf("selected = %d, want 2 after select-all", len(state.batchSelectedServers()))
	}
	if !state.clickServersBatch(hitServersBatchTargetList.At(0)) {
		t.Fatal("target row click was not handled")
	}
	if len(state.batchSelectedServers()) != 1 {
		t.Fatalf("selected = %d, want 1 after toggling one row off", len(state.batchSelectedServers()))
	}
	if !state.clickServersBatch(hitServersBatchSelectNone) {
		t.Fatal("clear click was not handled")
	}
	if len(state.batchSelectedServers()) != 0 {
		t.Fatalf("selected = %d, want 0 after clear", len(state.batchSelectedServers()))
	}
	_ = view
}

// History filters by result state: a run whose only target failed shows
// under "failed" and not under "success".
func TestBatchHistoryFilter(t *testing.T) {
	state := newTestState(t, testOptions(t))
	drain := installTestPost(t, state)
	state.openServersBatch()
	server := addBatchTestServer(t, state, "web", "", "web", "Ubuntu")
	state.requestServersBatchRun("uptime", "", []serverassets.Server{server})
	waitBatchDone(t, state, drain)
	view := &state.servers.batch
	view.historyState = "failed"
	state.refreshServersBatchHistory()
	if len(view.history) != 1 {
		t.Fatalf("failed filter = %d runs, want 1", len(view.history))
	}
	view.historyState = "success"
	state.refreshServersBatchHistory()
	if len(view.history) != 0 {
		t.Fatalf("success filter = %d runs, want 0", len(view.history))
	}
	view.historyState = ""
	state.refreshServersBatchHistory()
	if len(view.history) != 1 {
		t.Fatalf("cleared filter = %d runs, want 1", len(view.history))
	}
}

// User templates save, reload and delete; built-ins refuse the editor.
func TestBatchTemplateCRUD(t *testing.T) {
	state := newTestState(t, testOptions(t))
	state.openServersBatch()
	view := &state.servers.batch
	state.openServersBatchTemplateDialog("")
	dlg := &view.templateDlg
	if !dlg.open {
		t.Fatal("new-template dialog did not open")
	}
	setEditor(&dlg.name, "Check disk")
	setEditor(&dlg.command, "df -h")
	setEditor(&dlg.desc, "disk usage")
	state.saveServersBatchTemplate()
	if dlg.open {
		t.Fatalf("template dialog did not close: %s", dlg.err)
	}
	found := false
	for _, tmpl := range view.templates {
		if tmpl.Name == "Check disk" && tmpl.Command == "df -h" {
			found = true
			view.templateSelectedID = tmpl.ID
		}
	}
	if !found {
		t.Fatalf("saved template is not listed in %v", view.templates)
	}
	// Built-ins are read-only.
	state.openServersBatchTemplateDialog("builtin-service-restart")
	if dlg.open {
		t.Fatal("a built-in template opened in the editor")
	}
	if view.errorText == "" {
		t.Fatal("editing a built-in template reported no error")
	}
	// Delete the user template through the confirmation state.
	view.templateDeleteID = view.templateSelectedID
	state.deleteServersBatchTemplate()
	for _, tmpl := range view.templates {
		if tmpl.Name == "Check disk" {
			t.Fatal("deleted template is still listed")
		}
	}
}

// Executor snapshots refresh the run table without panicking, including
// a nil update.
func TestBatchApplyUpdate(t *testing.T) {
	state := newTestState(t, testOptions(t))
	state.openServersBatch()
	state.applyServersBatchUpdate(nil)
	if state.servers.batch.run != nil {
		t.Fatal("a nil update set a run")
	}
	run := &serverbatch.Run{
		ID: "run-1", Command: "uptime", State: serverbatch.RunRunning,
		Targets: []serverbatch.ServerResult{
			{ServerID: "a", ServerName: "alpha", State: serverbatch.StateSuccess, FinishedAt: time.Now()},
			{ServerID: "b", ServerName: "beta", State: serverbatch.StateSkipped},
		},
	}
	state.applyServersBatchUpdate(run)
	if state.servers.batch.run != run {
		t.Fatal("applyServersBatchUpdate did not keep the run")
	}
	if !strings.Contains(state.servers.batch.status, "(1/2)") {
		t.Fatalf("status = %q, want it to show 1 of 2 targets done", state.servers.batch.status)
	}
}

// The batch page paints: run controls, sidebar rows, and the history and
// template sub-views. The home page's batch button opens the page.
func TestBatchPagesPaint(t *testing.T) {
	state := newTestState(t, testOptions(t))
	state.setLanguage(i18n.Chinese)
	state.openServersBatch()
	size := gfx.Size{Width: 1400, Height: 900}
	canvas := &gfx.Canvas{}
	state.Paint(canvas, size)
	if _, ok := hitByID(canvas, hitServersBatchRun); !ok {
		t.Fatal("the batch run button is not painted")
	}
	if _, ok := hitByID(canvas, hitServersBatch); !ok {
		t.Fatal("the Batch Ops sidebar row is not painted")
	}
	if _, ok := hitByID(canvas, hitServersSFTP); !ok {
		t.Fatal("the SFTP sidebar row is not painted")
	}
	if label := state.navPageLabel(navPageServersBatch); label != "批量运维" {
		t.Fatalf("navPageLabel(batch) = %q, want 批量运维", label)
	}
	if label := state.navPageLabel(navPageServersSFTP); label != "SFTP 文件" {
		t.Fatalf("navPageLabel(sftp) = %q, want SFTP 文件", label)
	}
	state.setLanguage(i18n.English)
	if label := state.navPageLabel(navPageServersBatch); label != "Batch Ops" {
		t.Fatalf("navPageLabel(batch) = %q, want Batch Ops", label)
	}
	// History sub-view.
	state.servers.batch.view = "history"
	canvas = &gfx.Canvas{}
	state.Paint(canvas, size)
	if _, ok := hitByID(canvas, hitServersBatchExportCSV); !ok {
		t.Fatal("the history export button is not painted")
	}
	// Template sub-view.
	state.servers.batch.view = "templates"
	canvas = &gfx.Canvas{}
	state.Paint(canvas, size)
	if _, ok := hitByID(canvas, hitServersBatchTemplateNew); !ok {
		t.Fatal("the new-template button is not painted")
	}
	// The home page's batch button opens the batch page.
	state.openServersHome()
	canvas = &gfx.Canvas{}
	state.Paint(canvas, size)
	if _, ok := hitByID(canvas, hitServersHomeBatchCmd); !ok {
		t.Fatal("the overview batch-command button is not painted")
	}
	if !state.clickServersHome(hitServersHomeBatchCmd) {
		t.Fatal("the overview batch-command click was not handled")
	}
	if state.activeSection != sectionServersBatch {
		t.Fatalf("activeSection = %d, want sectionServersBatch", state.activeSection)
	}
}

// The confirmation dialog paints with its phrase field for a critical
// command.
func TestBatchConfirmDialogPaints(t *testing.T) {
	state := newTestState(t, testOptions(t))
	state.openServersBatch()
	server := addBatchTestServer(t, state, "web", "", "web", "Ubuntu")
	state.requestServersBatchRun("rm -rf /tmp/cache", "", []serverassets.Server{server})
	if state.servers.batch.confirm.stage != 2 {
		t.Fatal("critical command did not open the phrase dialog")
	}
	size := gfx.Size{Width: 1400, Height: 900}
	canvas := &gfx.Canvas{}
	state.Paint(canvas, size)
	if _, ok := hitByID(canvas, hitServersBatchDialogConfirm); !ok {
		t.Fatal("the confirm dialog's confirm button is not painted")
	}
	if _, ok := hitByID(canvas, hitServersBatchConfirmPhrase); !ok {
		t.Fatal("the confirm dialog's phrase field is not painted")
	}
}
