package workbench

// Batch operations page (phase 5): run one command on many managed
// servers, with risk confirmation, live results, history and templates.
//
// The page is a thin UI over internal/serverbatch: ClassifyRisk grades
// the command, Executor runs it (concurrency 20, per-server timeout,
// retry of failures only) on a background goroutine, and the template and
// history stores persist through the shared config store. The UI never
// dials SSH itself: the executor's DialFunc is serversDialConn, the
// serverconn.Dial wrapper that carries the jump host, the fingerprint
// check and the vault-backed credentials. Execution never blocks the
// window thread — Executor.Run runs in a goroutine and its onUpdate
// callback posts snapshots to the window thread via serversPost.
//
// Risk flow (spec §10.3): Normal runs at once (the page already shows
// the command, the target count and the target list before executing);
// High opens a second confirmation dialog; Critical opens a dialog that
// requires typing serverbatch.ConfirmPhrase ("确认执行"); Blocked is
// refused with its reasons and never reaches the executor (which also
// refuses it defensively). The risk grade is advisory — the page says
// identification cannot cover every risky command.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/vivaui/viva/app"
	"github.com/vivaui/viva/components"
	"github.com/vivaui/viva/gfx"
	"github.com/vivaui/viva/text"
	"github.com/vivaui/viva/ui"
	"motty/internal/serverassets"
	"motty/internal/serverbatch"
)

// Batch page hit identities, all under the servers hit feature.
var (
	hitServersBatchCommand               = tree.ID(serversHits, "batch-command")
	hitServersBatchViewRun               = tree.ID(serversHits, "batch-view-run")
	hitServersBatchViewHistory           = tree.ID(serversHits, "batch-view-history")
	hitServersBatchViewTemplates         = tree.ID(serversHits, "batch-view-templates")
	hitServersBatchSelectAll             = tree.ID(serversHits, "batch-select-all")
	hitServersBatchSelectNone            = tree.ID(serversHits, "batch-select-none")
	hitServersBatchRun                   = tree.ID(serversHits, "batch-run")
	hitServersBatchAbort                 = tree.ID(serversHits, "batch-abort")
	hitServersBatchRetry                 = tree.ID(serversHits, "batch-retry")
	hitServersBatchTargetList            = tree.Sub(serversHits, tree.Name("batch-target"))
	hitServersBatchResultList            = tree.Sub(serversHits, tree.Name("batch-result"))
	hitServersBatchHistoryRefresh        = tree.ID(serversHits, "batch-history-refresh")
	hitServersBatchExportCSV             = tree.ID(serversHits, "batch-export-csv")
	hitServersBatchExportXLSX            = tree.ID(serversHits, "batch-export-xlsx")
	hitServersBatchExportTXT             = tree.ID(serversHits, "batch-export-txt")
	hitServersBatchHistoryList           = tree.Sub(serversHits, tree.Name("batch-history"))
	hitServersBatchHistoryRerun          = tree.ID(serversHits, "batch-history-rerun")
	hitServersBatchHistoryDelete         = tree.ID(serversHits, "batch-history-delete")
	hitServersBatchTemplateNew           = tree.ID(serversHits, "batch-template-new")
	hitServersBatchTemplateEdit          = tree.ID(serversHits, "batch-template-edit")
	hitServersBatchTemplateDelete        = tree.ID(serversHits, "batch-template-delete")
	hitServersBatchTemplateList          = tree.Sub(serversHits, tree.Name("batch-template"))
	hitServersBatchDialogCancel          = tree.ID(serversHits, "batch-dialog-cancel")
	hitServersBatchDialogConfirm         = tree.ID(serversHits, "batch-dialog-confirm")
	hitServersBatchConfirmPhrase         = tree.ID(serversHits, "batch-confirm-phrase")
	hitServersBatchTemplateCancel        = tree.ID(serversHits, "batch-template-cancel")
	hitServersBatchTemplateSave          = tree.ID(serversHits, "batch-template-save")
	hitServersBatchTemplateDeleteCancel  = tree.ID(serversHits, "batch-template-delete-cancel")
	hitServersBatchTemplateDeleteConfirm = tree.ID(serversHits, "batch-template-delete-confirm")
	hitServersBatchDlgName               = tree.ID(serversHits, "batch-dlg-name")
	hitServersBatchDlgCommand            = tree.ID(serversHits, "batch-dlg-command")
	hitServersBatchDlgDescription        = tree.ID(serversHits, "batch-dlg-description")
	hitServersBatchDlgOS                 = tree.ID(serversHits, "batch-dlg-os")
)

// serversBatchSectionUI is what the batch page's declaration leaves for
// its paint (the empty state's icon slot, as on the servers page).
type serversBatchSectionUI struct {
	empty     bool
	emptyIcon uint8
	emptySlot ui.Box
	draw      func(*ui.DrawContext)
}

// batchConfirmState is a run waiting on its confirmation dialog: stage 1
// is the High second confirmation, stage 2 is the Critical type-the-phrase
// confirmation. The targets are resolved before the dialog opens, so the
// dialog shows exactly what will run.
type batchConfirmState struct {
	stage      int // 0 none, 1 high, 2 critical
	command    string
	templateID string
	targets    []serverassets.Server
}

// batchTemplateDialog is the user-template editor.
type batchTemplateDialog struct {
	open         bool
	editingID    string
	name         text.Editor
	command      text.Editor
	desc         text.Editor
	os           text.Editor
	nameField    ui.Field
	commandField ui.Field
	descField    ui.Field
	osField      ui.Field
	bound        bool
	err          string
}

// serversBatchState is the batch page's sub-state on State.servers.
type serversBatchState struct {
	view string // "run", "history", "templates"

	// Target selection: group/tag/OS filters plus an explicit set of
	// selected server ids. The list is derived from the loaded rows on
	// every paint, so a filter change never leaves a stale selection.
	filterGroup string
	filterTag   string
	filterOS    string
	selected    map[string]bool

	// Command entry.
	command       text.Editor
	commandField  ui.Field
	commandBound  bool
	templateID    string
	templates     []serverbatch.Template
	templateStore *serverbatch.TemplateStore

	// Confirmation dialog state.
	confirm            batchConfirmState
	confirmPhrase      text.Editor
	confirmPhraseField ui.Field
	confirmPhraseBound bool
	confirmErr         string

	// The current or last run. run is the executor's latest snapshot
	// (a deep copy, safe to read in Paint); running tracks the
	// background goroutine, executor is its handle for Abort.
	running  bool
	run      *serverbatch.Run
	executor *serverbatch.Executor
	runErr   string
	detailID string // ServerResult.ServerID whose full output is shown

	// History view.
	historyStore    *serverbatch.HistoryStore
	history         []*serverbatch.Run
	historyServerID string
	historyState    string // "", "success", "failed", "timeout", "cancelled", "skipped"
	historyDetailID string
	historyErr      string

	// Template management view.
	templateSelectedID string
	templateDlg        batchTemplateDialog
	templateDeleteID   string

	status    string
	errorText string

	targetList      ui.ListState
	targetRowDecl   func(ui.ListRow)
	resultList      ui.ListState
	resultRowDecl   func(ui.ListRow)
	historyList     ui.ListState
	historyRowDecl  func(ui.ListRow)
	templateList    ui.ListState
	templateRowDecl func(ui.ListRow)
}

// --- pure helpers (unit-tested) ---

// batchConfirmStage maps a risk level to the confirmation the UI must
// show: -1 refused, 0 run at once, 1 second confirmation, 2 type the
// phrase.
func batchConfirmStage(level serverbatch.RiskLevel) int {
	switch level {
	case serverbatch.RiskBlocked:
		return -1
	case serverbatch.RiskHigh:
		return 1
	case serverbatch.RiskCritical:
		return 2
	default:
		return 0
	}
}

// batchRiskLabel names a risk level in Chinese. serverbatch's own
// String is English; the page shows Chinese.
func batchRiskLabel(level serverbatch.RiskLevel) string {
	switch level {
	case serverbatch.RiskNormal:
		return "普通"
	case serverbatch.RiskHigh:
		return "高风险"
	case serverbatch.RiskCritical:
		return "极高风险"
	case serverbatch.RiskBlocked:
		return "已禁止"
	default:
		return "未知"
	}
}

// batchResultStateText names one server's result state in Chinese.
func batchResultStateText(state serverbatch.ResultState) string {
	switch state {
	case serverbatch.StateSuccess:
		return "成功"
	case serverbatch.StateFailed:
		return "失败"
	case serverbatch.StateTimeout:
		return "超时"
	case serverbatch.StateCancelled:
		return "已取消"
	case serverbatch.StateSkipped:
		return "已跳过"
	default:
		return "未知"
	}
}

// batchRunStateText names a run's overall state in Chinese.
func batchRunStateText(state serverbatch.RunState) string {
	switch state {
	case serverbatch.RunRunning:
		return "执行中"
	case serverbatch.RunFinished:
		return "已完成"
	case serverbatch.RunAborted:
		return "已中止"
	default:
		return "未知"
	}
}

// batchResultStateFromKey parses the history filter's state key.
func batchResultStateFromKey(key string) serverbatch.ResultState {
	switch key {
	case "failed":
		return serverbatch.StateFailed
	case "timeout":
		return serverbatch.StateTimeout
	case "cancelled":
		return serverbatch.StateCancelled
	case "skipped":
		return serverbatch.StateSkipped
	default:
		return serverbatch.StateSuccess
	}
}

// batchTargetServers filters the loaded rows to the batch page's live
// target candidates: group/tag/OS filters, non-deleted, name order. The
// health lookup is nil — the batch page does not filter by health.
func batchTargetServers(rows []serverassets.Server, groupID, tag, os string) []serverassets.Server {
	filtered := filterManagedServers(rows, serverassets.Filter{GroupID: groupID, Tag: tag, OS: os}, "", "", nil)
	sortManagedServers(filtered, nil)
	return filtered
}

// batchOutputSummary is a result's output as one short line: the first
// non-empty line, capped at 60 characters.
func batchOutputSummary(output string) string {
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if len([]rune(trimmed)) > 60 {
			return string([]rune(trimmed)[:60]) + "…"
		}
		return trimmed
	}
	return "—"
}

// batchRunSummary counts a run's outcomes in one status line.
func batchRunSummary(run *serverbatch.Run) string {
	if run == nil {
		return ""
	}
	counts := map[serverbatch.ResultState]int{}
	for _, target := range run.Targets {
		counts[target.State]++
	}
	return fmt.Sprintf("执行完成：成功 %d 台，失败 %d 台，超时 %d 台，取消 %d 台，跳过 %d 台",
		counts[serverbatch.StateSuccess], counts[serverbatch.StateFailed],
		counts[serverbatch.StateTimeout], counts[serverbatch.StateCancelled],
		counts[serverbatch.StateSkipped])
}

// batchSelectedServers resolves the page's explicit selection back to
// live servers, in list order.
func (state *State) batchSelectedServers() []serverassets.Server {
	view := &state.servers.batch
	var out []serverassets.Server
	for _, server := range state.servers.rows {
		if server.Deleted {
			continue
		}
		if view.selected[server.ID] {
			out = append(out, server)
		}
	}
	return out
}

// batchFailedServers resolves a run's failed or timed-out targets back to
// live servers, reporting the names that no longer resolve.
func (state *State) batchFailedServers(run *serverbatch.Run) (targets []serverassets.Server, missing []string) {
	for _, target := range run.Targets {
		if target.State != serverbatch.StateFailed && target.State != serverbatch.StateTimeout {
			continue
		}
		if server, ok := state.serverByID(target.ServerID); ok && !server.Deleted {
			targets = append(targets, server)
		} else {
			missing = append(missing, target.ServerName)
		}
	}
	return targets, missing
}

// batchIsBuiltinTemplate reports whether a template id is a built-in one
// (prefix "builtin-", the A-road's reservation); built-ins cannot be
// edited or deleted.
func batchIsBuiltinTemplate(id string) bool {
	return strings.HasPrefix(id, "builtin-")
}

// --- stores ---

// serversBatchTemplates returns the user-template store, creating it on
// first use from the shared config store. It returns nil when no config
// store is available.
func (state *State) serversBatchTemplates() *serverbatch.TemplateStore {
	view := &state.servers.batch
	if view.templateStore != nil {
		return view.templateStore
	}
	cfg := state.serversConfigStore()
	if cfg == nil {
		return nil
	}
	view.templateStore = serverbatch.NewTemplateStore(cfg)
	return view.templateStore
}

// serversBatchHistory returns the run-history store, creating it on first
// use from the shared config store.
func (state *State) serversBatchHistory() *serverbatch.HistoryStore {
	view := &state.servers.batch
	if view.historyStore != nil {
		return view.historyStore
	}
	cfg := state.serversConfigStore()
	if cfg == nil {
		return nil
	}
	view.historyStore = serverbatch.NewHistoryStore(cfg)
	return view.historyStore
}

// refreshServersBatchTemplates reloads the template list: built-ins first,
// then the user's, by name.
func (state *State) refreshServersBatchTemplates() {
	view := &state.servers.batch
	templates := serverbatch.BuiltinTemplates()
	if store := state.serversBatchTemplates(); store != nil {
		templates = append(templates, store.List()...)
	}
	view.templates = templates
}

// refreshServersBatchHistory reloads the history list under the current
// filters (server, result state), newest first, capped at 200 rows.
func (state *State) refreshServersBatchHistory() {
	view := &state.servers.batch
	store := state.serversBatchHistory()
	if store == nil {
		view.historyErr = viewErrorText(state)
		return
	}
	filter := serverbatch.HistoryFilter{ServerID: view.historyServerID, Limit: 200}
	if view.historyState != "" {
		filter.HasState = true
		filter.State = batchResultStateFromKey(view.historyState)
	}
	view.history = store.ListRuns(filter)
	view.historyErr = ""
}

func viewErrorText(state *State) string {
	if state.servers.storeErr != "" {
		return state.servers.storeErr
	}
	return state.serversText("Storage is unavailable", "存储不可用")
}

// openServersBatch shows the batch operations page: the same load as the
// servers page, plus the template and history lists.
func (state *State) openServersBatch() {
	state.activeSection = sectionServersBatch
	state.refreshServers(true)
	state.refreshServersBatchTemplates()
	state.refreshServersBatchHistory()
	if state.servers.batch.view == "" {
		state.servers.batch.view = "run"
	}
}

// --- run lifecycle ---

// requestServersBatchRun is the single entry point for starting a run:
// it grades the command and either refuses it, opens the confirmation
// dialog the grade requires, or starts it at once. The page already
// shows the command, the target count and the target list, so a Normal
// run needs no further confirmation.
func (state *State) requestServersBatchRun(command, templateID string, targets []serverassets.Server) {
	view := &state.servers.batch
	if state.appAccessBlocked() {
		view.errorText = state.messages.AppLock.LockedNotice
		return
	}
	if view.running {
		return
	}
	command = strings.TrimSpace(command)
	if command == "" {
		view.errorText = state.serversText("Enter a command first.", "请先输入命令。")
		return
	}
	if len(targets) == 0 {
		view.errorText = state.serversText("Select at least one server first.", "请先选择至少一台服务器。")
		return
	}
	risk := serverbatch.ClassifyRisk(command)
	switch batchConfirmStage(risk.Level) {
	case -1:
		view.errorText = fmt.Sprintf(state.serversText("This command is blocked: %s", "命令已被禁止：%s"), strings.Join(risk.Reasons, "；"))
		return
	case 1, 2:
		view.confirm = batchConfirmState{
			stage: batchConfirmStage(risk.Level), command: command,
			templateID: templateID, targets: targets,
		}
		setEditor(&view.confirmPhrase, "")
		view.confirmErr = ""
		return
	default:
		state.startServersBatchRun(command, templateID, targets)
	}
}

// startServersBatchRun runs a command on a background goroutine: the
// executor is synchronous, so the window thread never waits on it. Its
// onUpdate snapshots arrive through serversPost and refresh the results
// table; when the run ends the finished run is saved to history.
func (state *State) startServersBatchRun(command, templateID string, targets []serverassets.Server) {
	view := &state.servers.batch
	view.confirm = batchConfirmState{}
	view.confirmErr = ""
	executor := serverbatch.NewExecutor(state.serversDialConn, serverbatch.DefaultRunOptions(), func(run *serverbatch.Run) {
		state.serversPost(func() {
			state.applyServersBatchUpdate(run)
		})
	})
	view.executor = executor
	view.running = true
	view.run = nil
	view.detailID = ""
	view.runErr = ""
	view.errorText = ""
	view.status = fmt.Sprintf(state.serversText("Running… (0/%d)", "正在执行（0/%d）…"), len(targets))
	state.serversInvalidate()
	go func() {
		run, err := executor.Run(context.Background(), targets, command, templateID)
		state.serversPost(func() {
			view.running = false
			view.executor = nil
			if err != nil {
				view.runErr = err.Error()
				view.status = ""
				state.serversInvalidate()
				return
			}
			view.run = run
			if store := state.serversBatchHistory(); store != nil {
				if saveErr := store.SaveRun(run); saveErr != nil {
					view.errorText = saveErr.Error()
				}
				// History keeps 30 days; prune the overflow in the
				// background so the window thread never waits on it.
				go func() { _, _ = store.Prune() }()
			}
			view.status = batchRunSummary(run)
			state.refreshServersBatchHistory()
			state.serversInvalidate()
		})
	}()
}

// applyServersBatchUpdate applies one executor snapshot on the window
// thread: the run table repaints with the servers that have finished so
// far. It never panics on a nil run.
func (state *State) applyServersBatchUpdate(run *serverbatch.Run) {
	view := &state.servers.batch
	if run == nil {
		return
	}
	view.run = run
	done := 0
	for _, target := range run.Targets {
		if !target.FinishedAt.IsZero() {
			done++
		}
	}
	total := len(run.Targets)
	if run.State == serverbatch.RunRunning {
		view.status = fmt.Sprintf(state.serversText("Running… (%d/%d)", "正在执行（%d/%d）…"), done, total)
	}
	state.serversInvalidate()
}

// abortServersBatchRun asks the executor to stop: in-flight servers are
// cancelled (their SSH sessions close) and unstarted ones are skipped.
// A remote process that detached from its session may keep running — the
// results say Cancelled, never that the remote process was killed.
func (state *State) abortServersBatchRun() {
	view := &state.servers.batch
	if !view.running || view.executor == nil {
		return
	}
	view.executor.Abort()
	view.status = state.serversText("Aborting…", "正在中止…")
}

// retryServersBatchFailed re-runs only the last run's failed or timed-out
// servers, through the same confirmation flow as a fresh run (the risk
// grade is re-checked on the same command).
func (state *State) retryServersBatchFailed() {
	view := &state.servers.batch
	if view.run == nil || view.running {
		return
	}
	targets, missing := state.batchFailedServers(view.run)
	if len(missing) > 0 {
		view.status = fmt.Sprintf(state.serversText("%d server(s) no longer exist and will be skipped: %s", "%d 台服务器已不存在或已删除，将跳过：%s"), len(missing), strings.Join(missing, "、"))
	}
	if len(targets) == 0 {
		view.errorText = state.serversText("No failed servers to retry.", "没有可重试的失败服务器。")
		return
	}
	state.requestServersBatchRun(view.run.Command, view.run.TemplateID, targets)
}

// rerunServersBatchHistory re-runs one history run's command on its
// original targets (resolved live), again through the confirmation flow
// so the target scope is confirmed a second time.
func (state *State) rerunServersBatchHistory(run *serverbatch.Run) {
	view := &state.servers.batch
	if run == nil || view.running {
		return
	}
	var targets []serverassets.Server
	var missing []string
	for _, target := range run.Targets {
		if server, ok := state.serverByID(target.ServerID); ok && !server.Deleted {
			targets = append(targets, server)
		} else {
			missing = append(missing, target.ServerName)
		}
	}
	if len(missing) > 0 {
		view.status = fmt.Sprintf(state.serversText("%d server(s) no longer exist and will be skipped: %s", "%d 台服务器已不存在或已删除，将跳过：%s"), len(missing), strings.Join(missing, "、"))
	}
	if len(targets) == 0 {
		view.errorText = state.serversText("No live servers left to re-run on.", "没有可重新执行的服务器。")
		return
	}
	view.view = "run"
	state.requestServersBatchRun(run.Command, run.TemplateID, targets)
}

// --- template management ---

// openServersBatchTemplateDialog opens the template editor for a new
// template (id "") or an existing user template. Built-ins are refused:
// they are read-only.
func (state *State) openServersBatchTemplateDialog(id string) {
	view := &state.servers.batch
	dlg := &view.templateDlg
	dlg.open = true
	dlg.editingID = id
	dlg.err = ""
	setEditor(&dlg.name, "")
	setEditor(&dlg.command, "")
	setEditor(&dlg.desc, "")
	setEditor(&dlg.os, "")
	if id == "" {
		return
	}
	if batchIsBuiltinTemplate(id) {
		dlg.open = false
		view.errorText = state.serversText("Built-in templates cannot be edited.", "内置模板不可编辑。")
		return
	}
	store := state.serversBatchTemplates()
	if store == nil {
		dlg.open = false
		view.errorText = viewErrorText(state)
		return
	}
	tmpl, ok := store.Get(id)
	if !ok {
		dlg.open = false
		view.errorText = state.serversText("Template not found.", "模板不存在。")
		return
	}
	setEditor(&dlg.name, tmpl.Name)
	setEditor(&dlg.command, tmpl.Command)
	setEditor(&dlg.desc, tmpl.Description)
	setEditor(&dlg.os, tmpl.OS)
}

// saveServersBatchTemplate validates the editor and writes the template.
// The store re-derives the risk grade from the command on save.
func (state *State) saveServersBatchTemplate() {
	view := &state.servers.batch
	dlg := &view.templateDlg
	store := state.serversBatchTemplates()
	if store == nil {
		dlg.err = viewErrorText(state)
		return
	}
	tmpl := serverbatch.Template{
		ID:          dlg.editingID,
		Name:        strings.TrimSpace(dlg.name.String()),
		Command:     strings.TrimSpace(dlg.command.String()),
		Description: strings.TrimSpace(dlg.desc.String()),
		OS:          strings.TrimSpace(dlg.os.String()),
	}
	if tmpl.Name == "" || tmpl.Command == "" {
		dlg.err = state.serversText("Name and command are required.", "名称和命令必填。")
		return
	}
	if tmpl.ID == "" {
		if _, err := store.Create(tmpl); err != nil {
			dlg.err = err.Error()
			return
		}
	} else {
		if err := store.Update(tmpl); err != nil {
			dlg.err = err.Error()
			return
		}
	}
	dlg.open = false
	view.status = state.serversText("Template saved.", "模板已保存。")
	state.refreshServersBatchTemplates()
}

// deleteServersBatchTemplate deletes the template awaiting confirmation.
func (state *State) deleteServersBatchTemplate() {
	view := &state.servers.batch
	id := view.templateDeleteID
	view.templateDeleteID = ""
	if id == "" || batchIsBuiltinTemplate(id) {
		return
	}
	store := state.serversBatchTemplates()
	if store == nil {
		view.errorText = viewErrorText(state)
		return
	}
	if err := store.Delete(id); err != nil {
		view.errorText = err.Error()
		return
	}
	if view.templateSelectedID == id {
		view.templateSelectedID = ""
	}
	view.status = state.serversText("Template deleted.", "模板已删除。")
	state.refreshServersBatchTemplates()
}

// --- history export ---

// exportServersBatchHistory writes the filtered history list to a file
// the user picks in the save dialog. Commands and outputs pass through
// serverbatch.Redact inside the exporters.
func (state *State) exportServersBatchHistory(format string) {
	view := &state.servers.batch
	var buf bytes.Buffer
	var ext, title string
	switch format {
	case "csv":
		ext, title = "csv", state.serversText("Export history as CSV", "导出历史为 CSV")
		if err := serverbatch.ExportRunsCSV(view.history, &buf); err != nil {
			view.errorText = err.Error()
			return
		}
	case "xlsx":
		ext, title = "xlsx", state.serversText("Export history as Excel", "导出历史为 Excel")
		if err := serverbatch.ExportRunsXLSX(view.history, &buf); err != nil {
			view.errorText = err.Error()
			return
		}
	default:
		ext, title = "txt", state.serversText("Export history as text", "导出历史为文本")
		if err := serverbatch.ExportRunsTXT(view.history, &buf); err != nil {
			view.errorText = err.Error()
			return
		}
	}
	path, err := state.options.SaveFile(app.SaveFileOptions{
		Title:            title,
		SuggestedName:    "batch-runs-" + time.Now().Format("20060102-150405") + "." + ext,
		DefaultExtension: ext,
		Filters:          []app.FileFilter{{Name: strings.ToUpper(ext), Pattern: "*." + ext}},
	})
	if errors.Is(err, app.ErrCanceled) || err == nil && path == "" {
		return
	}
	if err == nil {
		err = os.WriteFile(path, buf.Bytes(), 0o600)
	}
	if err != nil {
		view.errorText = err.Error()
		return
	}
	view.status = fmt.Sprintf(state.serversText("Exported to %s", "已导出到 %s"), path)
}

// --- page declaration ---

// declareServersBatchWorkspace declares the batch operations page.
func (state *State) declareServersBatchWorkspace(c ui.C) {
	view, pal := &state.servers.batch, state.pal
	bounds := state.catalogSectionBounds()
	layout := &state.sectionBodies.serversBatch
	layout.empty, layout.emptyIcon = false, uint8(iconTerminal)
	if layout.draw == nil {
		layout.draw = func(d *ui.DrawContext) { state.paintServersBatchWorkspace(d.Canvas) }
	}
	if view.view == "" {
		view.view = "run"
	}
	body := ui.Column(c).Fill().PadSides(12, serversSidePad, 0, serversSidePad)
	body.Children(func() {
		ui.Column(c).MarginSides(4, 0, 12, 0).Gap(3).Children(func() {
			ui.Text(c, state.messages.NavServersBatch).FontSize(18).Weight(gfx.WeightSemibold).Color(pal.text).Ellipsis()
			ui.Paragraph(c, state.serversText("Run one command on many servers, with risk confirmation and a saved history.", "在一台或多台服务器上执行同一命令，带风险确认，并保存执行历史。"), 2).FontSize(metrics.FontSizeS).Color(pal.muted)
		})
		ui.Row(c).Height(40).Gap(8).Items(ui.Center).Children(func() {
			state.declareButtonSized(c, hitServersBatchViewRun, state.serversText("Run", "执行"), iconTerminal, view.view == "run", false, true, toolbarFont, false).Size(96, toolbarHeight)
			state.declareButtonSized(c, hitServersBatchViewHistory, state.serversText("History", "执行历史"), iconFileClock, view.view == "history", false, true, toolbarFont, false).Size(128, toolbarHeight)
			state.declareButtonSized(c, hitServersBatchViewTemplates, state.serversText("Templates", "模板管理"), iconSnippets, view.view == "templates", false, true, toolbarFont, false).Size(128, toolbarHeight)
			ui.Spacer(c)
		})
		switch view.view {
		case "history":
			state.declareServersBatchHistory(c)
		case "templates":
			state.declareServersBatchTemplates(c)
		default:
			state.declareServersBatchRun(c)
		}
		ui.Divider(c).Background(pal.line)
		ui.Row(c).Height(49).Gap(10).Items(ui.Center).Children(func() {
			ui.Text(c, view.status).FontSize(metrics.FontSizeS).Color(pal.muted).Grow(1).Ellipsis()
			if view.errorText != "" {
				ui.Text(c, view.errorText).FontSize(metrics.FontSizeS).Color(pal.danger).Grow(1).Ellipsis()
			}
			if view.runErr != "" {
				ui.Text(c, view.runErr).FontSize(metrics.FontSizeS).Color(pal.danger).Grow(1).Ellipsis()
			}
		})
	})
	_ = bounds
	body.Clip().DrawOver(layout.draw)
}

// declareServersBatchRun declares the run view: target selection, the
// command box with its template picker and risk badge, the run controls
// and the live results table.
func (state *State) declareServersBatchRun(c ui.C) {
	view, pal := &state.servers.batch, state.pal
	targets := batchTargetServers(state.servers.rows, view.filterGroup, view.filterTag, view.filterOS)
	selected := state.batchSelectedServers()
	if view.targetRowDecl == nil {
		view.targetRowDecl = state.declareServersBatchTargetRow
		view.targetList.Key = func(row int) int { return row }
		view.targetList.Label = func(row int) string {
			if row >= 0 && row < len(targets) {
				return targets[row].Name
			}
			return ""
		}
	}
	if view.resultRowDecl == nil {
		view.resultRowDecl = state.declareServersBatchResultRow
		view.resultList.Key = func(row int) int { return row }
		view.resultList.Label = func(row int) string {
			if view.run != nil && row >= 0 && row < len(view.run.Targets) {
				return view.run.Targets[row].ServerName
			}
			return ""
		}
	}
	// The target list's keys and labels close over the filtered list:
	// refresh them every declaration, as the servers page does with its
	// page-relative mapping.
	view.targetList.Key = func(row int) int { return row }
	view.targetList.Label = func(row int) string {
		if row >= 0 && row < len(targets) {
			return targets[row].Name
		}
		return ""
	}
	if !view.commandBound {
		view.commandField.Bind(&view.command)
		view.commandBound = true
	}
	command := strings.TrimSpace(view.command.String())
	risk := serverbatch.ClassifyRisk(command)

	ui.Row(c).Height(36).Gap(8).Items(ui.Center).MarginSides(8, 0, 4, 0).Children(func() {
		state.declareServersBatchFilterChoices(c)
		ui.Spacer(c)
		state.declareButtonSized(c, hitServersBatchSelectAll, state.serversText("Select all", "全选"), iconCheck, false, false, len(targets) > 0 && !view.running, toolbarFont, false).Size(96, toolbarHeight)
		state.declareButtonSized(c, hitServersBatchSelectNone, state.serversText("Clear", "清除选择"), iconTrash, false, false, len(selected) > 0 && !view.running, toolbarFont, false).Size(112, toolbarHeight)
		ui.Text(c, fmt.Sprintf(state.serversText("%d of %d servers selected", "已选 %d / %d 台"), len(selected), len(targets))).FontSize(metrics.FontSizeS).Color(pal.muted).Shrink(0)
	})
	ui.Column(c).Height(190).Shrink(0).MarginSides(4, 0, 4, 0).Children(func() {
		ui.List(c, &view.targetList, len(targets), view.targetRowDecl).Grow(1).Shrink(1).MinHeight(0).FillWidth().
			Scrollbars().Label(state.serversText("Target servers", "目标服务器"))
	})
	ui.Row(c).Height(40).Gap(8).Items(ui.Center).MarginSides(8, 0, 4, 0).Children(func() {
		ui.TextField(c, &view.commandField, state.serversText("Command", "命令"), state.serversText("Command to run on every selected server", "要在每台选中服务器上执行的命令"), ui.WithID(hitServersBatchCommand)).
			Grow(1).MinWidth(serversSearchLeast).Height(40)
		templateOptions := []string{""}
		for _, tmpl := range view.templates {
			templateOptions = append(templateOptions, tmpl.ID)
		}
		if ui.Choice(c, &view.templateID, templateOptions, func(id string) string {
			if id == "" {
				return state.serversText("No template", "不使用模板")
			}
			for _, tmpl := range view.templates {
				if tmpl.ID == id {
					return tmpl.Name
				}
			}
			return id
		}).Height(40).Changed() {
			if view.templateID != "" {
				for _, tmpl := range view.templates {
					if tmpl.ID == view.templateID {
						setEditor(&view.command, tmpl.Command)
						break
					}
				}
			}
		}
	})
	// The risk badge: the grade and its reasons, plus the standing
	// disclaimer that identification cannot cover every risky command.
	riskColor := pal.text
	if risk.Level == serverbatch.RiskBlocked || risk.Level == serverbatch.RiskCritical {
		riskColor = pal.danger
	}
	ui.Row(c).Height(30).Gap(8).Items(ui.Center).MarginSides(8, 0, 4, 0).Children(func() {
		ui.Text(c, fmt.Sprintf(state.serversText("Risk: %s", "风险：%s"), batchRiskLabel(risk.Level))).FontSize(metrics.FontSizeS).Weight(gfx.WeightMedium).Color(riskColor).Shrink(0)
		if len(risk.Reasons) > 0 {
			ui.Text(c, strings.Join(risk.Reasons, "；")).FontSize(metrics.FontSizeS).Color(pal.muted).Ellipsis()
		} else if command != "" {
			ui.Text(c, state.serversText("No risk rules matched.", "未匹配到风险规则。")).FontSize(metrics.FontSizeS).Color(pal.muted).Ellipsis()
		}
		ui.Spacer(c)
	})
	ui.Text(c, state.serversText("Risk identification cannot cover every risky command — check the targets and the command before running.", "风险识别无法覆盖全部风险命令，执行前请核对目标与命令。")).FontSize(metrics.FontSizeS).Color(pal.muted).Ellipsis()
	ui.Row(c).Height(40).Gap(8).Items(ui.Center).MarginSides(8, 0, 4, 0).Children(func() {
		canRun := command != "" && len(selected) > 0 && !view.running && risk.Level != serverbatch.RiskBlocked
		state.declareButtonSized(c, hitServersBatchRun, state.serversText("Run", "执行"), iconTerminal, true, false, canRun, toolbarFont, false).Size(112, toolbarHeight)
		state.declareButtonSized(c, hitServersBatchAbort, state.serversText("Abort", "中止"), iconAlert, false, true, view.running, toolbarFont, false).Size(112, toolbarHeight)
		hasFailed := false
		if view.run != nil && !view.running {
			targets, _ := state.batchFailedServers(view.run)
			hasFailed = len(targets) > 0
		}
		state.declareButtonSized(c, hitServersBatchRetry, state.serversText("Retry failed", "重试失败台"), iconRefresh, false, false, hasFailed, toolbarFont, false).Size(144, toolbarHeight)
		ui.Spacer(c)
		if view.run != nil {
			ui.Text(c, fmt.Sprintf(state.serversText("%s · %s", "%s · %s"), batchRunStateText(view.run.State), batchRunSummary(view.run))).FontSize(metrics.FontSizeS).Color(pal.muted).Ellipsis()
		}
	})
	state.declareServersBatchResults(c)
}

// declareServersBatchFilterChoices declares the group/tag/OS target
// filters, reusing the servers list's option sources.
func (state *State) declareServersBatchFilterChoices(c ui.C) {
	view := &state.servers.batch
	groupOptions := []string{""}
	for _, group := range state.servers.groups {
		groupOptions = append(groupOptions, group.ID)
	}
	tagOptions := []string{""}
	seenTags := map[string]bool{}
	for _, server := range state.servers.rows {
		for _, tag := range server.Tags {
			if tag != "" && !seenTags[tag] {
				seenTags[tag] = true
				tagOptions = append(tagOptions, tag)
			}
		}
	}
	osOptions := []string{""}
	seenOS := map[string]bool{}
	for _, server := range state.servers.rows {
		if server.OS != "" && !seenOS[server.OS] {
			seenOS[server.OS] = true
			osOptions = append(osOptions, server.OS)
		}
	}
	if ui.Choice(c, &view.filterGroup, groupOptions, func(id string) string {
		if id == "" {
			return state.serversText("All groups", "全部分组")
		}
		return state.serversGroupName(id)
	}).Height(30).Changed() {
		state.serversInvalidate()
	}
	if ui.Choice(c, &view.filterTag, tagOptions, func(tag string) string {
		if tag == "" {
			return state.serversText("All tags", "全部标签")
		}
		return tag
	}).Height(30).Changed() {
		state.serversInvalidate()
	}
	if ui.Choice(c, &view.filterOS, osOptions, func(os string) string {
		if os == "" {
			return state.serversText("All systems", "全部系统")
		}
		return os
	}).Height(30).Changed() {
		state.serversInvalidate()
	}
}

// declareServersBatchTargetRow declares one target row: a check mark,
// the server name, its address and its OS. Clicking toggles selection.
func (state *State) declareServersBatchTargetRow(row ui.ListRow) {
	view := &state.servers.batch
	targets := batchTargetServers(state.servers.rows, view.filterGroup, view.filterTag, view.filterOS)
	if row.Index < 0 || row.Index >= len(targets) {
		return
	}
	server := targets[row.Index]
	theme := state.Theme()
	height := components.TableRowHeight(theme)
	pad := theme.Metrics.GapItem
	address := server.Address
	if address == "" {
		address = server.Hostname
	}
	mark := "○ "
	if view.selected[server.ID] {
		mark = "✓ "
	}
	c := row.C
	element := row.Element.Height(height).Shrink(0).FillWidth().Gap(0).Items(ui.Center)
	if view.selected[server.ID] {
		element = element.Background(theme.Muted)
	} else {
		element = element.
			Hover(ui.Look{}.Background(theme.Hover)).
			Pressed(ui.Look{}.Background(theme.Muted))
	}
	element.Children(func() {
		ui.Text(c, mark+server.Name).PadX(pad).Width(260).Shrink(0).Ellipsis()
		ui.Text(c, address).PadX(pad).Width(180).Shrink(0).Ellipsis()
		ui.Text(c, server.OS).PadX(pad).Grow(1).Basis(0).Ellipsis()
		ui.Slot(c).Absolute().Left(0).Right(0).Bottom(0).Height(1).Background(theme.Border)
	})
	element.Hit(hitServersBatchTargetList.At(row.Index), server.Name+" · "+address)
}

// declareServersBatchResults declares the results table and, below it,
// the selected server's full output.
func (state *State) declareServersBatchResults(c ui.C) {
	view, pal := &state.servers.batch, state.pal
	theme := state.Theme()
	headerHeight := components.TableHeaderHeight(theme)
	headerPad := theme.Metrics.GapItem
	headings := []string{
		state.serversText("Server", "服务器"),
		state.serversText("State", "状态"),
		state.serversText("Exit code", "退出码"),
		state.serversText("Duration", "耗时"),
		state.serversText("Output", "输出摘要"),
	}
	widths := []float32{180, 80, 80, 100, 0}
	ui.Column(c).Grow(1).Shrink(1).MarginSides(8, 0, 0, 0).Children(func() {
		ui.Row(c).Height(headerHeight).Shrink(0).FillWidth().Gap(0).Items(ui.Center).Children(func() {
			for column, heading := range headings {
				text := ui.Text(c, heading).Weight(gfx.WeightMedium).PadX(headerPad)
				if widths[column] > 0 {
					text = text.Width(widths[column]).Shrink(0)
				} else {
					text = text.Grow(1).Basis(0)
				}
				text.Ellipsis()
			}
			ui.Slot(c).Absolute().Left(0).Right(0).Bottom(0).Height(1).Background(theme.Border)
		})
		count := 0
		if view.run != nil {
			count = len(view.run.Targets)
		}
		ui.List(c, &view.resultList, count, view.resultRowDecl).Grow(1).Shrink(1).MinHeight(0).FillWidth().
			Scrollbars().Label(state.serversText("Results", "执行结果"))
	})
	if view.run != nil && view.detailID != "" {
		for _, target := range view.run.Targets {
			if target.ServerID != view.detailID {
				continue
			}
			ui.Column(c).MarginSides(8, 0, 4, 0).Gap(4).Children(func() {
				ui.Text(c, fmt.Sprintf(state.serversText("%s — full output", "%s — 完整输出"), target.ServerName)).FontSize(metrics.FontSizeS).Weight(gfx.WeightMedium).Color(pal.text).Ellipsis()
				if target.Error != "" {
					ui.Text(c, fmt.Sprintf(state.serversText("Error: %s", "错误：%s"), target.Error)).FontSize(metrics.FontSizeS).Color(pal.danger).Ellipsis()
				}
				output := target.Output
				if output == "" {
					output = state.serversText("(no output)", "（无输出）")
				}
				if len(output) > 32768 {
					output = output[:32768] + "\n…"
				}
				ui.Paragraph(c, output, 8).FontSize(metrics.FontSizeS).Color(pal.text)
			})
		}
	}
}

// declareServersBatchResultRow declares one result row. A target that has
// not started yet (the executor's Skipped placeholder) shows "等待执行".
func (state *State) declareServersBatchResultRow(row ui.ListRow) {
	view := &state.servers.batch
	if view.run == nil || row.Index < 0 || row.Index >= len(view.run.Targets) {
		return
	}
	target := view.run.Targets[row.Index]
	theme := state.Theme()
	height := components.TableRowHeight(theme)
	pad := theme.Metrics.GapItem
	exitCode := "—"
	if target.ExitCode != nil {
		exitCode = fmt.Sprintf("%d", *target.ExitCode)
	}
	duration := "—"
	if !target.FinishedAt.IsZero() {
		duration = target.Duration.Round(time.Millisecond).String()
	}
	stateText := batchResultStateText(target.State)
	if target.StartedAt.IsZero() && target.State == serverbatch.StateSkipped {
		stateText = state.serversText("Waiting", "等待执行")
	}
	cells := []string{
		target.ServerName, stateText, exitCode, duration,
		batchOutputSummary(target.Output),
	}
	if target.OutputTruncated {
		cells[4] += state.serversText(" (truncated)", "（已截断）")
	}
	widths := []float32{180, 80, 80, 100, 0}
	c := row.C
	element := row.Element.Height(height).Shrink(0).FillWidth().Gap(0).Items(ui.Center)
	if target.ServerID == view.detailID {
		element = element.Background(theme.Muted)
	} else {
		element = element.
			Hover(ui.Look{}.Background(theme.Hover)).
			Pressed(ui.Look{}.Background(theme.Muted))
	}
	element.Children(func() {
		for column, cell := range cells {
			text := ui.Text(c, cell).PadX(pad)
			if widths[column] > 0 {
				text = text.Width(widths[column]).Shrink(0)
			} else {
				text = text.Grow(1).Basis(0)
			}
			if column == 1 && target.State == serverbatch.StateFailed {
				text = text.Color(state.pal.danger)
			}
			text.Ellipsis()
		}
		ui.Slot(c).Absolute().Left(0).Right(0).Bottom(0).Height(1).Background(theme.Border)
	})
	element.Hit(hitServersBatchResultList.At(row.Index), target.ServerName+" · "+stateText)
}

// declareServersBatchHistory declares the history sub-view: server and
// result filters, exports, the run list and the selected run's detail.
func (state *State) declareServersBatchHistory(c ui.C) {
	view, pal := &state.servers.batch, state.pal
	if view.historyRowDecl == nil {
		view.historyRowDecl = state.declareServersBatchHistoryRow
		view.historyList.Key = func(row int) int { return row }
		view.historyList.Label = func(row int) string {
			if row >= 0 && row < len(view.history) {
				return view.history[row].Command
			}
			return ""
		}
	}
	view.historyList.Key = func(row int) int { return row }
	view.historyList.Label = func(row int) string {
		if row >= 0 && row < len(view.history) {
			return view.history[row].Command
		}
		return ""
	}
	serverOptions := []string{""}
	for _, server := range state.servers.rows {
		if !server.Deleted {
			serverOptions = append(serverOptions, server.ID)
		}
	}
	stateOptions := []string{"", "success", "failed", "timeout", "cancelled", "skipped"}
	ui.Row(c).Height(36).Gap(8).Items(ui.Center).MarginSides(8, 0, 4, 0).Children(func() {
		if ui.Choice(c, &view.historyServerID, serverOptions, func(id string) string {
			if id == "" {
				return state.serversText("All servers", "全部服务器")
			}
			if server, ok := state.serverByID(id); ok {
				return server.Name
			}
			return id
		}).Height(30).Changed() {
			state.refreshServersBatchHistory()
		}
		if ui.Choice(c, &view.historyState, stateOptions, func(key string) string {
			if key == "" {
				return state.serversText("Any result", "全部结果")
			}
			return batchResultStateText(batchResultStateFromKey(key))
		}).Height(30).Changed() {
			state.refreshServersBatchHistory()
		}
		ui.Spacer(c)
		state.declareButtonSized(c, hitServersBatchHistoryRefresh, state.serversText("Refresh", "刷新"), iconRefresh, false, false, true, toolbarFont, false).Size(96, toolbarHeight)
	})
	ui.Row(c).Height(40).Gap(8).Items(ui.Center).MarginSides(8, 0, 4, 0).Children(func() {
		ui.Text(c, fmt.Sprintf(state.serversText("%d runs", "共 %d 条记录"), len(view.history))).FontSize(metrics.FontSizeS).Weight(gfx.WeightMedium).Color(pal.faint).Ellipsis()
		ui.Spacer(c)
		state.declareButtonSized(c, hitServersBatchExportCSV, state.serversText("Export CSV", "导出 CSV"), iconDownload, false, false, len(view.history) > 0, toolbarFont, false).Size(128, toolbarHeight)
		state.declareButtonSized(c, hitServersBatchExportXLSX, state.serversText("Export Excel", "导出 Excel"), iconDownload, false, false, len(view.history) > 0, toolbarFont, false).Size(144, toolbarHeight)
		state.declareButtonSized(c, hitServersBatchExportTXT, state.serversText("Export text", "导出文本"), iconDownload, false, false, len(view.history) > 0, toolbarFont, false).Size(128, toolbarHeight)
	})
	if view.historyErr != "" {
		ui.Text(c, view.historyErr).FontSize(metrics.FontSizeS).Color(pal.danger).Ellipsis()
	}
	ui.Column(c).Grow(1).Shrink(1).MarginSides(8, 0, 0, 0).Children(func() {
		ui.List(c, &view.historyList, len(view.history), view.historyRowDecl).Grow(1).Shrink(1).MinHeight(0).FillWidth().
			Scrollbars().Label(state.serversText("History", "执行历史"))
	})
	if view.historyDetailID != "" {
		for _, run := range view.history {
			if run.ID != view.historyDetailID {
				continue
			}
			state.declareServersBatchHistoryDetail(c, run)
		}
	}
}

// declareServersBatchHistoryRow declares one history row.
func (state *State) declareServersBatchHistoryRow(row ui.ListRow) {
	view := &state.servers.batch
	if row.Index < 0 || row.Index >= len(view.history) {
		return
	}
	run := view.history[row.Index]
	theme := state.Theme()
	height := components.TableRowHeight(theme)
	pad := theme.Metrics.GapItem
	command := run.Command
	if len([]rune(command)) > 48 {
		command = string([]rune(command)[:48]) + "…"
	}
	text := fmt.Sprintf("%s · %s · %d 台 · %s", run.CreatedAt.Format("2006-01-02 15:04"), command, len(run.Targets), batchRunStateText(run.State))
	c := row.C
	element := row.Element.Height(height).Shrink(0).FillWidth().Gap(0).Items(ui.Center)
	if run.ID == view.historyDetailID {
		element = element.Background(theme.Muted)
	} else {
		element = element.
			Hover(ui.Look{}.Background(theme.Hover)).
			Pressed(ui.Look{}.Background(theme.Muted))
	}
	element.Children(func() {
		ui.Text(c, text).PadX(pad).Grow(1).Basis(0).Ellipsis()
		ui.Text(c, batchRunSummary(run)).PadX(pad).Shrink(0).Ellipsis()
		ui.Slot(c).Absolute().Left(0).Right(0).Bottom(0).Height(1).Background(theme.Border)
	})
	element.Hit(hitServersBatchHistoryList.At(row.Index), run.Command)
}

// declareServersBatchHistoryDetail declares one history run's detail:
// its per-server outcomes and the re-run / delete actions.
func (state *State) declareServersBatchHistoryDetail(c ui.C, run *serverbatch.Run) {
	view, pal := &state.servers.batch, state.pal
	ui.Column(c).MarginSides(8, 0, 4, 0).Gap(4).Children(func() {
		ui.Text(c, fmt.Sprintf(state.serversText("Run %s · %s", "任务 %s · %s"), run.CreatedAt.Format("2006-01-02 15:04:05"), batchRunStateText(run.State))).FontSize(metrics.FontSizeS).Weight(gfx.WeightMedium).Color(pal.text).Ellipsis()
		ui.Text(c, run.Command).FontSize(metrics.FontSizeS).Color(pal.muted).Ellipsis()
		shown := 0
		for _, target := range run.Targets {
			if shown >= 50 {
				break
			}
			exitCode := "—"
			if target.ExitCode != nil {
				exitCode = fmt.Sprintf("%d", *target.ExitCode)
			}
			line := fmt.Sprintf("%s · %s · %s · %s", target.ServerName, batchResultStateText(target.State), exitCode, batchOutputSummary(target.Output))
			if target.Error != "" {
				line += " · " + target.Error
			}
			ui.Text(c, line).FontSize(metrics.FontSizeS).Color(pal.text).Ellipsis()
			shown++
		}
		if len(run.Targets) > shown {
			ui.Text(c, fmt.Sprintf(state.serversText("…and %d more servers", "……另有 %d 台"), len(run.Targets)-shown)).FontSize(metrics.FontSizeS).Color(pal.muted).Ellipsis()
		}
		ui.Row(c).Height(40).Gap(8).Items(ui.Center).MarginSides(4, 0, 0, 0).Children(func() {
			state.declareButtonSized(c, hitServersBatchHistoryRerun, state.serversText("Re-run", "重新执行"), iconRefresh, false, false, !view.running, toolbarFont, false).Size(128, toolbarHeight)
			state.declareButtonSized(c, hitServersBatchHistoryDelete, state.serversText("Delete record", "删除记录"), iconTrash, false, false, true, toolbarFont, false).Size(128, toolbarHeight)
			ui.Spacer(c)
		})
	})
}

// declareServersBatchTemplates declares the template management sub-view.
func (state *State) declareServersBatchTemplates(c ui.C) {
	view, pal := &state.servers.batch, state.pal
	if view.templateRowDecl == nil {
		view.templateRowDecl = state.declareServersBatchTemplateRow
		view.templateList.Key = func(row int) int { return row }
		view.templateList.Label = func(row int) string {
			if row >= 0 && row < len(view.templates) {
				return view.templates[row].Name
			}
			return ""
		}
	}
	view.templateList.Key = func(row int) int { return row }
	view.templateList.Label = func(row int) string {
		if row >= 0 && row < len(view.templates) {
			return view.templates[row].Name
		}
		return ""
	}
	selected, selectedOK := state.batchSelectedTemplate()
	canEdit := selectedOK && !batchIsBuiltinTemplate(selected.ID)
	ui.Row(c).Height(40).Gap(8).Items(ui.Center).MarginSides(8, 0, 4, 0).Children(func() {
		state.declareButtonSized(c, hitServersBatchTemplateNew, state.serversText("New template", "新增模板"), iconPlus, true, false, true, toolbarFont, false).Size(144, toolbarHeight)
		state.declareButtonSized(c, hitServersBatchTemplateEdit, state.serversText("Edit", "编辑"), iconPencil, false, false, canEdit, toolbarFont, false).Size(96, toolbarHeight)
		state.declareButtonSized(c, hitServersBatchTemplateDelete, state.serversText("Delete", "删除"), iconTrash, false, false, canEdit, toolbarFont, false).Size(96, toolbarHeight)
		ui.Spacer(c)
		ui.Text(c, state.serversText("Built-in templates cannot be edited or deleted.", "内置模板不可编辑或删除。")).FontSize(metrics.FontSizeS).Color(pal.muted).Ellipsis()
	})
	ui.Column(c).Grow(1).Shrink(1).MarginSides(8, 0, 0, 0).Children(func() {
		ui.List(c, &view.templateList, len(view.templates), view.templateRowDecl).Grow(1).Shrink(1).MinHeight(0).FillWidth().
			Scrollbars().Label(state.serversText("Templates", "命令模板"))
	})
}

// batchSelectedTemplate returns the template the management list has
// selected, if any.
func (state *State) batchSelectedTemplate() (serverbatch.Template, bool) {
	view := &state.servers.batch
	for _, tmpl := range view.templates {
		if tmpl.ID == view.templateSelectedID {
			return tmpl, true
		}
	}
	return serverbatch.Template{}, false
}

// declareServersBatchTemplateRow declares one template row.
func (state *State) declareServersBatchTemplateRow(row ui.ListRow) {
	view := &state.servers.batch
	if row.Index < 0 || row.Index >= len(view.templates) {
		return
	}
	tmpl := view.templates[row.Index]
	theme := state.Theme()
	height := components.TableRowHeight(theme)
	pad := theme.Metrics.GapItem
	kind := state.serversText("Custom", "自定义")
	if batchIsBuiltinTemplate(tmpl.ID) {
		kind = state.serversText("Built-in", "内置")
	}
	command := tmpl.Command
	if len([]rune(command)) > 48 {
		command = string([]rune(command)[:48]) + "…"
	}
	c := row.C
	element := row.Element.Height(height).Shrink(0).FillWidth().Gap(0).Items(ui.Center)
	if tmpl.ID == view.templateSelectedID {
		element = element.Background(theme.Muted)
	} else {
		element = element.
			Hover(ui.Look{}.Background(theme.Hover)).
			Pressed(ui.Look{}.Background(theme.Muted))
	}
	element.Children(func() {
		ui.Text(c, tmpl.Name).PadX(pad).Width(180).Shrink(0).Ellipsis()
		ui.Text(c, command).PadX(pad).Grow(1).Basis(0).Ellipsis()
		ui.Text(c, batchRiskLabel(tmpl.Risk)).PadX(pad).Width(80).Shrink(0).Ellipsis()
		ui.Text(c, kind).PadX(pad).Width(70).Shrink(0).Ellipsis()
		ui.Slot(c).Absolute().Left(0).Right(0).Bottom(0).Height(1).Background(theme.Border)
	})
	element.Hit(hitServersBatchTemplateList.At(row.Index), tmpl.Name+" · "+tmpl.Command)
}

// --- dialogs ---

// declareServersBatchDialogs declares whichever batch dialog is on top:
// the run confirmation, the template editor or the template delete
// confirmation. It is called from declareServersDialogs.
func (state *State) declareServersBatchDialogs(c ui.C) {
	view := &state.servers.batch
	switch {
	case view.confirm.stage != 0:
		state.declareServersBatchConfirmDialog(c)
	case view.templateDlg.open:
		state.declareServersBatchTemplateDialog(c)
	case view.templateDeleteID != "":
		state.declareServersBatchTemplateDeleteDialog(c)
	}
}

// declareServersBatchConfirmDialog declares the run confirmation dialog:
// the command, the target count and names, and the risk reasons. A High
// run needs one more click; a Critical run needs the phrase typed.
func (state *State) declareServersBatchConfirmDialog(c ui.C) {
	view := &state.servers.batch
	confirm := view.confirm
	if !view.confirmPhraseBound {
		view.confirmPhraseField.Bind(&view.confirmPhrase)
		view.confirmPhraseBound = true
	}
	risk := serverbatch.ClassifyRisk(confirm.command)
	names := make([]string, 0, len(confirm.targets))
	for _, target := range confirm.targets {
		names = append(names, target.Name)
	}
	shown := names
	extra := ""
	if len(names) > 10 {
		shown = names[:10]
		extra = fmt.Sprintf(state.serversText("…and %d more servers", "……另有 %d 台"), len(names)-10)
	}
	title := state.serversText("Confirm batch run", "确认批量执行")
	if confirm.stage == 2 {
		title = state.serversText("Confirm critical batch run", "确认执行极高风险命令")
	}
	description := fmt.Sprintf(state.serversText("Run on %d servers: %s", "将在 %d 台服务器上执行：%s"), len(confirm.targets), confirm.command)
	viewport := dialogViewport(c)
	bodyHeight := float32(180)
	if confirm.stage == 2 {
		bodyHeight = 250
	}
	height := dialogContentHeight(c.Canvas(), *c.Theme(), viewport, 620, description, bodyHeight)
	dialog := ui.Dialog(c, ui.DialogSpec{
		Title: title, Description: description,
		Width: 620, Height: height, Layer: dialogLayer,
		Scope: dialogScope, DismissID: hitServersBatchDialogCancel, CloseID: hitServersBatchDialogCancel,
		InitialFocusID: hitServersBatchDialogCancel,
	})
	dialog.Body(func() {
		ui.Column(c).Fill().Gap(8).Children(func() {
			ui.Text(c, fmt.Sprintf(state.serversText("Risk: %s — %s", "风险：%s — %s"), batchRiskLabel(risk.Level), strings.Join(risk.Reasons, "；"))).FontSize(metrics.FontSizeS).Weight(gfx.WeightMedium).Color(state.pal.danger).LineHeight(24)
			ui.Text(c, fmt.Sprintf(state.serversText("Targets: %s %s", "目标：%s %s"), strings.Join(shown, "、"), extra)).FontSize(metrics.FontSizeS).Color(state.pal.muted).LineHeight(24)
			if confirm.stage == 2 {
				ui.Text(c, fmt.Sprintf(state.serversText("This is a critical-risk command. Type %q to confirm.", "这是极高风险命令。请输入 %q 确认。"), serverbatch.ConfirmPhrase)).FontSize(metrics.FontSizeS).Color(state.pal.text).LineHeight(24)
				ui.TextField(c, &view.confirmPhraseField, state.serversText("Confirmation phrase", "确认文字"), serverbatch.ConfirmPhrase, ui.WithID(hitServersBatchConfirmPhrase)).Height(36)
				if view.confirmErr != "" {
					ui.Text(c, view.confirmErr).FontSize(metrics.FontSizeS).Color(state.pal.danger).LineHeight(24)
				}
			} else {
				ui.Text(c, state.serversText("Check the command and the targets once more before running.", "执行前请再核对一次命令与目标。")).FontSize(metrics.FontSizeS).Color(state.pal.muted).LineHeight(24)
			}
		})
	})
	confirmAction := components.ButtonAction{ID: hitServersBatchDialogConfirm, Label: state.serversText("Run", "确认执行")}
	if confirm.stage == 2 {
		confirmAction.Variant = components.ButtonDestructive
	}
	dialog.Footer(func() {
		state.dialogActionFooter(c,
			components.ButtonAction{ID: hitServersBatchDialogCancel, Label: state.messages.Cancel, Variant: components.ButtonSecondary},
			confirmAction,
		)
	})
}

// declareServersBatchTemplateDialog declares the template editor dialog.
func (state *State) declareServersBatchTemplateDialog(c ui.C) {
	view := &state.servers.batch
	dlg := &view.templateDlg
	if !dlg.bound {
		dlg.nameField.Bind(&dlg.name)
		dlg.commandField.Bind(&dlg.command)
		dlg.descField.Bind(&dlg.desc)
		dlg.osField.Bind(&dlg.os)
		dlg.bound = true
	}
	title := state.serversText("New template", "新增模板")
	if dlg.editingID != "" {
		title = state.serversText("Edit template", "编辑模板")
	}
	description := state.serversText("A reusable batch command.", "一条可复用的批量命令。")
	risk := serverbatch.ClassifyRisk(strings.TrimSpace(dlg.command.String()))
	viewport := dialogViewport(c)
	height := dialogContentHeight(c.Canvas(), *c.Theme(), viewport, 620, description, 300)
	dialog := ui.Dialog(c, ui.DialogSpec{
		Title: title, Description: description,
		Width: 620, Height: height, Layer: dialogLayer,
		Scope: dialogScope, DismissID: hitServersBatchTemplateCancel, CloseID: hitServersBatchTemplateCancel,
		InitialFocusID: hitServersBatchTemplateCancel,
	})
	dialog.Body(func() {
		ui.Column(c).Fill().Gap(6).Children(func() {
			state.serversDialogField(c, state.serversText("Name", "名称"), &dlg.nameField, &dlg.name, hitServersBatchDlgName, "")
			state.serversDialogField(c, state.serversText("Command", "命令"), &dlg.commandField, &dlg.command, hitServersBatchDlgCommand, "")
			state.serversDialogField(c, state.serversText("Description", "描述"), &dlg.descField, &dlg.desc, hitServersBatchDlgDescription, "")
			state.serversDialogField(c, state.serversText("System", "适用系统"), &dlg.osField, &dlg.os, hitServersBatchDlgOS, "")
			if strings.TrimSpace(dlg.command.String()) != "" {
				ui.Text(c, fmt.Sprintf(state.serversText("Risk: %s", "风险：%s"), batchRiskLabel(risk.Level))).FontSize(metrics.FontSizeS).Color(state.pal.muted).LineHeight(24)
				if len(risk.Reasons) > 0 {
					ui.Text(c, strings.Join(risk.Reasons, "；")).FontSize(metrics.FontSizeS).Color(state.pal.muted).LineHeight(24)
				}
			}
			if dlg.err != "" {
				ui.Text(c, dlg.err).FontSize(metrics.FontSizeS).Color(state.pal.danger).LineHeight(24)
			}
		})
	})
	dialog.Footer(func() {
		state.dialogActionFooter(c,
			components.ButtonAction{ID: hitServersBatchTemplateCancel, Label: state.messages.Cancel, Variant: components.ButtonSecondary},
			components.ButtonAction{ID: hitServersBatchTemplateSave, Label: state.serversText("Save", "保存")},
		)
	})
}

// declareServersBatchTemplateDeleteDialog declares the template delete
// confirmation.
func (state *State) declareServersBatchTemplateDeleteDialog(c ui.C) {
	view := &state.servers.batch
	name := view.templateDeleteID
	if tmpl, ok := state.batchSelectedTemplate(); ok && tmpl.ID == view.templateDeleteID {
		name = tmpl.Name
	}
	description := fmt.Sprintf(state.serversText("Delete template %s?", "删除模板 %s？"), name)
	viewport := dialogViewport(c)
	height := dialogContentHeight(c.Canvas(), *c.Theme(), viewport, 520, description, 24)
	dialog := ui.Dialog(c, ui.DialogSpec{
		Title: state.serversText("Delete template", "删除模板"), Description: description,
		Width: 520, Height: height, Layer: dialogLayer,
		Scope: dialogScope, DismissID: hitServersBatchTemplateDeleteCancel, CloseID: hitServersBatchTemplateDeleteCancel,
		InitialFocusID: hitServersBatchTemplateDeleteCancel,
	})
	dialog.Body(func() {
		ui.Column(c).Fill().Gap(8).Children(func() {
			ui.Text(c, state.serversText("Built-in templates are not affected.", "内置模板不受影响。")).FontSize(metrics.FontSizeS).Color(state.pal.muted).LineHeight(24)
		})
	})
	dialog.Footer(func() {
		state.dialogActionFooter(c,
			components.ButtonAction{ID: hitServersBatchTemplateDeleteCancel, Label: state.messages.Cancel, Variant: components.ButtonSecondary},
			components.ButtonAction{ID: hitServersBatchTemplateDeleteConfirm, Label: state.serversText("Delete", "删除"), Variant: components.ButtonDestructive},
		)
	})
}

// --- click dispatch ---

// clickServersBatch handles a click on the batch page or one of its
// dialogs. It returns false for identities it does not own.
func (state *State) clickServersBatch(id gfx.ID) bool {
	view := &state.servers.batch
	// Dialog buttons win over the page behind them.
	if view.confirm.stage != 0 {
		switch id {
		case hitServersBatchDialogCancel:
			view.confirm = batchConfirmState{}
			view.confirmErr = ""
			return true
		case hitServersBatchDialogConfirm:
			confirm := view.confirm
			if confirm.stage == 2 && strings.TrimSpace(view.confirmPhrase.String()) != serverbatch.ConfirmPhrase {
				view.confirmErr = state.serversText("The confirmation phrase is wrong — type it exactly.", "确认文字不正确，请原样输入。")
				return true
			}
			state.startServersBatchRun(confirm.command, confirm.templateID, confirm.targets)
			return true
		}
		return false
	}
	if view.templateDlg.open {
		switch id {
		case hitServersBatchTemplateSave:
			state.saveServersBatchTemplate()
			return true
		case hitServersBatchTemplateCancel:
			view.templateDlg.open = false
			return true
		}
		return false
	}
	if view.templateDeleteID != "" {
		switch id {
		case hitServersBatchTemplateDeleteConfirm:
			state.deleteServersBatchTemplate()
			return true
		case hitServersBatchTemplateDeleteCancel:
			view.templateDeleteID = ""
			return true
		}
		return false
	}
	switch id {
	case hitServersBatchViewRun:
		view.view = "run"
		return true
	case hitServersBatchViewHistory:
		view.view = "history"
		state.refreshServersBatchHistory()
		return true
	case hitServersBatchViewTemplates:
		view.view = "templates"
		state.refreshServersBatchTemplates()
		return true
	case hitServersBatchSelectAll:
		targets := batchTargetServers(state.servers.rows, view.filterGroup, view.filterTag, view.filterOS)
		if view.selected == nil {
			view.selected = map[string]bool{}
		}
		for _, target := range targets {
			view.selected[target.ID] = true
		}
		return true
	case hitServersBatchSelectNone:
		targets := batchTargetServers(state.servers.rows, view.filterGroup, view.filterTag, view.filterOS)
		for _, target := range targets {
			delete(view.selected, target.ID)
		}
		return true
	case hitServersBatchRun:
		state.requestServersBatchRun(view.command.String(), view.templateID, state.batchSelectedServers())
		return true
	case hitServersBatchAbort:
		state.abortServersBatchRun()
		return true
	case hitServersBatchRetry:
		state.retryServersBatchFailed()
		return true
	case hitServersBatchHistoryRefresh:
		state.refreshServersBatchHistory()
		return true
	case hitServersBatchExportCSV:
		state.exportServersBatchHistory("csv")
		return true
	case hitServersBatchExportXLSX:
		state.exportServersBatchHistory("xlsx")
		return true
	case hitServersBatchExportTXT:
		state.exportServersBatchHistory("txt")
		return true
	case hitServersBatchHistoryRerun:
		for _, run := range view.history {
			if run.ID == view.historyDetailID {
				state.rerunServersBatchHistory(run)
				return true
			}
		}
		return true
	case hitServersBatchHistoryDelete:
		if view.historyDetailID == "" {
			return true
		}
		store := state.serversBatchHistory()
		if store == nil {
			view.errorText = viewErrorText(state)
			return true
		}
		if err := store.DeleteRun(view.historyDetailID); err != nil {
			view.errorText = err.Error()
			return true
		}
		view.historyDetailID = ""
		view.status = state.serversText("History record deleted.", "历史记录已删除。")
		state.refreshServersBatchHistory()
		return true
	case hitServersBatchTemplateNew:
		state.openServersBatchTemplateDialog("")
		return true
	case hitServersBatchTemplateEdit:
		if view.templateSelectedID != "" {
			state.openServersBatchTemplateDialog(view.templateSelectedID)
		}
		return true
	case hitServersBatchTemplateDelete:
		if view.templateSelectedID != "" && !batchIsBuiltinTemplate(view.templateSelectedID) {
			view.templateDeleteID = view.templateSelectedID
		}
		return true
	}
	if id.Scope() == hitServersBatchTargetList {
		targets := batchTargetServers(state.servers.rows, view.filterGroup, view.filterTag, view.filterOS)
		if index := item(id); index >= 0 && index < len(targets) {
			if view.selected == nil {
				view.selected = map[string]bool{}
			}
			id := targets[index].ID
			if view.selected[id] {
				delete(view.selected, id)
			} else {
				view.selected[id] = true
			}
			return true
		}
	}
	if id.Scope() == hitServersBatchResultList {
		if view.run != nil {
			if index := item(id); index >= 0 && index < len(view.run.Targets) {
				target := view.run.Targets[index]
				if view.detailID == target.ServerID {
					view.detailID = ""
				} else {
					view.detailID = target.ServerID
				}
				return true
			}
		}
	}
	if id.Scope() == hitServersBatchHistoryList {
		if index := item(id); index >= 0 && index < len(view.history) {
			run := view.history[index]
			if view.historyDetailID == run.ID {
				view.historyDetailID = ""
			} else {
				view.historyDetailID = run.ID
			}
			return true
		}
	}
	if id.Scope() == hitServersBatchTemplateList {
		if index := item(id); index >= 0 && index < len(view.templates) {
			tmpl := view.templates[index]
			if view.templateSelectedID == tmpl.ID {
				view.templateSelectedID = ""
			} else {
				view.templateSelectedID = tmpl.ID
			}
			return true
		}
	}
	return false
}

// paintServersBatchWorkspace paints the batch page's empty state into the
// slot its declaration left. The page always has content, so it is
// normally a no-op.
func (state *State) paintServersBatchWorkspace(canvas *gfx.Canvas) {
	layout := &state.sectionBodies.serversBatch
	if layout.empty {
		state.drawReservedEmptyPage(canvas, layout.emptySlot, layout.emptyIcon)
	}
}
