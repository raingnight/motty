package workbench

// Servers page (phase 2): State wiring.
//
// The page keeps its sub-state on State.servers. The asset store handle is
// lazy: the first open takes the window's shared config store when it
// exists (state.config, opened by openConfig) and otherwise opens the same
// file with configstore.OpenShared, then wraps it with
// serverassets.NewStore. That OpenShared call is reference-counted — see
// configstore/store.go Close: the database closes with the last holder —
// and the handle cached here is deliberately NOT closed by State.Close:
// it is the same registry entry the window's own config handle owns, and
// state.config.Close() releases it. Closing it here as well would only
// drop the count one holder early; never closing it here keeps the rule
// "one owner closes the shared store" in one place.
//
// Phase 3 adds the collection engine (internal/serversched) on the same
// lazy pattern: the first open of the servers page starts it, and it keeps
// running while the window lives, on its own goroutines, never on the UI
// thread. It is stopped by closeServers (State.Close) and resumed on wake
// from State.Wake; see serversEngine and connection.go.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/vivaui/viva/app"
	"github.com/vivaui/viva/gfx"
	"github.com/vivaui/viva/text"
	"github.com/vivaui/viva/ui"
	"motty/internal/configstore"
	"motty/internal/serveralerts"
	"motty/internal/serverassets"
	"motty/internal/servercollect"
	"motty/internal/serverconn"
	"motty/internal/serversched"
	"motty/internal/sshconfig"
	"motty/internal/sshconn"
)

// Servers page hit identities. The sidebar row itself is hitServers (the
// shell feature, beside hitHosts); everything on the page is serversHits.
var (
	hitServersSearch        = tree.ID(serversHits, "search")
	hitServersRefresh       = tree.ID(serversHits, "refresh")
	hitServersNew           = tree.ID(serversHits, "new")
	hitServersTestAll       = tree.ID(serversHits, "test-all")
	hitServersJumpHosts     = tree.ID(serversHits, "jump-hosts")
	hitServersImport        = tree.ID(serversHits, "import")
	hitServersShowDeleted   = tree.ID(serversHits, "show-deleted")
	hitServersRowList       = tree.Sub(serversHits, tree.Name("row"))
	hitServersTest          = tree.ID(serversHits, "test")
	hitServersOpenTerminal  = tree.ID(serversHits, "open-terminal")
	hitServersEdit          = tree.ID(serversHits, "edit")
	hitServersDelete        = tree.ID(serversHits, "delete")
	hitServersRestore       = tree.ID(serversHits, "restore")
	hitServersDialogDismiss = tree.ID(serversHits, "dialog-dismiss")
	hitServersDialogClose   = tree.ID(serversHits, "dialog-close")
	hitServersDialogCancel  = tree.ID(serversHits, "dialog-cancel")
	hitServersDialogConfirm = tree.ID(serversHits, "dialog-confirm")
	hitServersJumpNew       = tree.ID(serversHits, "jump-new")
	hitServersJumpDelete    = tree.ID(serversHits, "jump-delete")
	hitServersImportPreview = tree.ID(serversHits, "import-preview")
	hitServersImportApply   = tree.ID(serversHits, "import-apply")
	hitServersFingerprintOK = tree.ID(serversHits, "fingerprint-confirm")
	// Servers overview (phase 4): its quick actions.
	hitServersHomeAdd        = tree.ID(serversHits, "home-add")
	hitServersHomeCollectAll = tree.ID(serversHits, "home-collect-all")
	hitServersHomeBatchCmd   = tree.ID(serversHits, "home-batch-command")
	hitServersHomeTerminal   = tree.ID(serversHits, "home-terminal")
	// Alert center (phase 4): its actions and rows.
	hitServerAlertsMarkAllRead = tree.ID(serversHits, "alerts-mark-all-read")
	hitServerAlertsRowList     = tree.Sub(serversHits, tree.Name("alert-row"))
	// Servers list pagination (phase 4): the footer's previous/next.
	hitServersPrevPage = tree.ID(serversHits, "prev-page")
	hitServersNextPage = tree.ID(serversHits, "next-page")
)

// serversText is the page's wording without a messages-catalog entry: the
// catalog covers the sidebar label (NavServers); the page itself follows
// the notes pattern of picking English or Chinese by language.
func (state *State) serversText(en, zh string) string {
	return state.noteText(en, zh)
}

// serversState is the servers page's sub-state on State.
type serversState struct {
	store       *serverassets.Store
	sharedStore *configstore.Store // shared handle; never closed here, see above
	storeErr    string

	// engine is the phase 3 collection engine, started lazily by the first
	// open of the servers page and stopped by closeServers. It owns its own
	// SSH connections (never shared with terminal sessions) and its own
	// goroutines; the page only reads its latest values for the resource
	// columns.
	engine *serversched.Engine

	// alertsStore is the phase 4 alert store over the same shared config
	// store as the assets (never closed here, same rule as sharedStore).
	// alertsWatcher evaluates thresholds on an interval and reports new
	// alerts; it is started lazily with the engine and stopped by
	// closeServers.
	alertsStore   *serveralerts.Store
	alertsWatcher *serveralerts.Watcher

	// health is the per-server health derived from the last check, the
	// latest collected values, the collection errors and the active
	// alerts; stats is the overview's aggregate over the same inputs.
	// Both are caches: refreshServersHealth recomputes them on the window
	// thread (page open, engine/watcher notify), Paint only reads them.
	health       map[string]serveralerts.ServerHealth
	stats        serveralerts.Stats
	healthAt     time.Time
	alertsLoaded bool
	// alerts is every alert the store holds, newest first.
	alerts []serveralerts.Alert

	rows    []serverassets.Server
	visible []serverassets.Server
	groups  []serverassets.ServerGroup
	jumps   []serverassets.JumpHost

	filter      serverassets.Filter
	search      text.Editor
	searchField ui.Field
	searchBound bool

	// healthFilter and alertFilter are the phase 4 list filters, kept
	// apart from serverassets.Filter: one of the serversHealth* /
	// serversAlert* values, "" for all.
	healthFilter string
	alertFilter  string
	// page is the list's 0-based page; pageSize is 0 for the default (50),
	// -1 for all, or a page size. pageSizeChoice is what the footer's
	// switcher edits (0 for all). filterSig is the last filter the page
	// reset for, so a filter change returns to page 1.
	page           int
	pageSize       int
	pageSizeChoice int
	filterSig      string
	pageText       string
	pageNumberText string

	// collectingAll is a batch collection in flight (see
	// runServersCollectAll): collectDone/collectTotal/collectFailed are
	// its progress, shown in the status line.
	collectingAll bool
	collectDone   int
	collectTotal  int
	collectFailed int

	// alertsFilterRead ("", "unread", "read") and alertsFilterKind (""
	// or an AlertKind) filter the alert center; alertsVisible is the
	// filtered list, newest first, and alertsList its table.
	alertsFilterRead string
	alertsFilterKind string
	alertsVisible    []serveralerts.Alert
	alertsList       ui.ListState
	alertsRowDecl    func(ui.ListRow)
	alertsCountText  string

	paginationItems [3]ui.PaginationItem

	selectedID string
	status     string
	errorText  string
	countText  string

	// batch is the phase 5 batch operations page's sub-state
	// (servers_batch.go); sftp is the phase 5 SFTP file management
	// page's sub-state (servers_sftp.go). Both live under servers so
	// they share the asset store, the vault credentials and the
	// serversPost window-thread hop.
	batch serversBatchState
	sftp  serversSFTPState

	testing    map[string]bool
	results    map[string]serverconn.TestResult
	testingAll bool

	editor    serversEditorDialog
	jumpsDlg  serversJumpDialog
	importDlg serversImportDialog
	deleteID  string // pending delete confirm, "" when none
	pendingFP *serversFingerprintConfirm

	list    ui.ListState
	rowDecl func(ui.ListRow)
}

// serversFingerprintConfirm is a fingerprint a test just saw that differs
// from the stored one (or the first one ever seen). It waits for the user
// to confirm before anything is saved; dismissing it leaves the stored
// fingerprint untouched.
type serversFingerprintConfirm struct {
	serverID    string
	serverName  string
	fingerprint string
	known       string // stored fingerprint, "" on first sight
}

// closeServers drops the page's cached view and stops the collection
// engine and the alert watcher. It closes nothing else: the shared store
// handle is released by state.config.Close() (see the file header).
// Phase 5: it also aborts any batch run in flight (its executor's Abort
// cancels the run context; the run's completion post then finds the
// executor gone and only clears state) and closes the SFTP browser.
func (state *State) closeServers() {
	if state.servers.batch.executor != nil {
		state.servers.batch.executor.Abort()
		state.servers.batch.executor = nil
		state.servers.batch.running = false
	}
	state.closeServersSFTPBrowser()
	if state.servers.alertsWatcher != nil {
		state.servers.alertsWatcher.Stop()
		state.servers.alertsWatcher = nil
	}
	state.servers.alertsStore = nil
	if state.servers.engine != nil {
		state.servers.engine.Stop()
		state.servers.engine = nil
	}
	state.servers.sharedStore = nil
	state.servers.store = nil
}

// serversEngine returns the collection engine, starting it on first use.
// The engine dials with the same serverconn.Dial the connection test uses,
// reads credentials from the vault through serversCredentials, and stores
// latest values in the shared config store under the server-metrics kind.
// New values arrive on the engine's goroutines; its notify callback only
// asks the window to repaint, so collection never touches UI state
// directly. It returns nil when no config store is available; callers
// report that through storeErr instead of panicking. Start is idempotent
// and the first tick collects every kind for every known server, so
// baselines are established without waiting a full interval.
func (state *State) serversEngine() *serversched.Engine {
	view := &state.servers
	if view.engine != nil {
		return view.engine
	}
	cfg := state.serversConfigStore()
	if cfg == nil {
		return nil
	}
	assets := state.serversStore()
	if assets == nil {
		return nil
	}
	view.engine = serversched.New(serversched.Config{
		Store:          cfg,
		Assets:         assets,
		Collector:      servercollect.New(),
		Creds:          state.serversCredentials(),
		KnownHostsPath: state.options.KnownHostsPath,
		Notify: func() {
			// New values arrived on the engine's goroutines: refresh the
			// cached health/stats on the window thread, then repaint.
			// Paint itself never derives health or stats.
			state.serversPost(func() {
				state.refreshServersHealth()
				state.syncServersVisible()
				state.serversInvalidate()
			})
		},
	})
	view.engine.Start()
	return view.engine
}

// serversLatest returns one server's latest collected values, if the
// engine has started and has any. It never starts the engine: rows are
// painted before the page opens the engine, and paint must stay cheap.
func (state *State) serversLatest(serverID string) (servercollect.Latest, bool) {
	if state.servers.engine == nil {
		return servercollect.Latest{}, false
	}
	return state.servers.engine.Latest(serverID)
}

// serversAlertsStore returns the alert store, creating it on first use
// from the shared config store — the same handle the asset store uses,
// never closed here. It returns nil when no config store is available.
func (state *State) serversAlertsStore() *serveralerts.Store {
	view := &state.servers
	if view.alertsStore != nil {
		return view.alertsStore
	}
	cfg := state.serversConfigStore()
	if cfg == nil {
		return nil
	}
	view.alertsStore = serveralerts.NewStore(cfg)
	return view.alertsStore
}

// serversAlerts returns the alert watcher, starting it on first use. The
// watcher evaluates thresholds on its own interval; its Assets callback
// lists the live (non-deleted) servers, Latest reads the engine's memory
// and LastError the engine's per-kind failures. Both callbacks run on the
// watcher's goroutine, so OnNewAlert and Notify only post work to the
// window thread: a new alert notifies the desktop (unless notifications
// are off) and both refresh the cached health/stats and repaint.
func (state *State) serversAlerts() *serveralerts.Watcher {
	view := &state.servers
	if view.alertsWatcher != nil {
		return view.alertsWatcher
	}
	store := state.serversAlertsStore()
	if store == nil {
		return nil
	}
	view.alertsWatcher = serveralerts.NewWatcher(serveralerts.WatcherConfig{
		Store: store,
		Assets: func() ([]serverassets.Server, error) {
			assets := state.serversStore()
			if assets == nil {
				return nil, nil
			}
			return assets.ListServers(serverassets.Filter{})
		},
		Latest: func() []servercollect.Latest {
			if engine := state.servers.engine; engine != nil {
				return engine.AllLatest()
			}
			return nil
		},
		LastError: func(serverID string, kind servercollect.MetricKind) (string, bool) {
			if engine := state.servers.engine; engine != nil {
				return engine.LastError(serverID, kind)
			}
			return "", false
		},
		Interval: 30 * time.Second,
		OnNewAlert: func(alert serveralerts.Alert) {
			state.serversPost(func() {
				state.notifyServerAlert(alert)
				state.refreshServersHealth()
				state.syncServersVisible()
				state.serversInvalidate()
			})
		},
		Notify: func() {
			state.serversPost(func() {
				state.refreshServersHealth()
				state.syncServersVisible()
				state.serversInvalidate()
			})
		},
	})
	view.alertsWatcher.Start()
	return view.alertsWatcher
}

// notifyServerAlert shows one new alert as a desktop notification: the
// title names the server and the alert kind, the body is the alert's
// message. It runs on the window thread; with notifications off it stays
// silent, and a notification failure is only a status line, never a
// dialog. Secrets never reach a notification: Alert.Message carries only
// the threshold breach, never credentials.
func (state *State) notifyServerAlert(alert serveralerts.Alert) {
	if state.notifyOff {
		return
	}
	name := alert.ServerName
	if name == "" {
		name = alert.ServerID
	}
	title := name + " · " + state.serverAlertKindLabel(alert.Kind)
	if notify := state.options.Notify; notify != nil {
		if err := notify(app.Notification{Title: title, Body: alert.Message}); err != nil {
			state.servers.status = err.Error()
		}
	}
}

// loadServerAlerts reloads every alert from the store, newest first, into
// the cache and rebuilds the alert center's filtered list. With no store
// (or a read error) it keeps the cached alerts. It runs on the window
// thread; callers are refreshServersHealth and refreshServerAlerts.
func (state *State) loadServerAlerts() []serveralerts.Alert {
	view := &state.servers
	store := state.serversAlertsStore()
	if store == nil {
		return view.alerts
	}
	listed, err := store.List()
	if err != nil {
		return view.alerts
	}
	sort.Slice(listed, func(i, j int) bool {
		if !listed[i].CreatedAt.Equal(listed[j].CreatedAt) {
			return listed[i].CreatedAt.After(listed[j].CreatedAt)
		}
		return listed[i].ID < listed[j].ID
	})
	view.alerts = listed
	view.alertsLoaded = true
	state.syncServerAlertsVisible()
	return listed
}

// refreshServerAlerts reloads the alerts for the alert center page.
func (state *State) refreshServerAlerts() {
	state.loadServerAlerts()
}

// syncServerAlertsVisible rebuilds the alert center's filtered list from
// the cached alerts and its unread count.
func (state *State) syncServerAlertsVisible() {
	view := &state.servers
	unread := 0
	for _, alert := range view.alerts {
		if !alert.Read {
			unread++
		}
	}
	visible := make([]serveralerts.Alert, 0, len(view.alerts))
	for _, alert := range view.alerts {
		if view.alertsFilterRead == "unread" && alert.Read {
			continue
		}
		if view.alertsFilterRead == "read" && !alert.Read {
			continue
		}
		if view.alertsFilterKind != "" && string(alert.Kind) != view.alertsFilterKind {
			continue
		}
		visible = append(visible, alert)
	}
	view.alertsVisible = visible
	view.alertsCountText = fmt.Sprintf(state.serversText("%d alerts · %d unread", "共 %d 条告警 · 未读 %d 条"), len(view.alerts), unread)
}

// refreshServersHealth recomputes the cached per-server health and the
// overview stats from the loaded rows, the engine's latest values, the
// engine's per-kind collection errors and the active alerts. It runs on
// the window thread (page open, engine/watcher notify); Paint only reads
// the cache. With no engine yet the latest values and errors are empty
// and health falls back to the last check and the alerts.
func (state *State) refreshServersHealth() {
	view := &state.servers
	now := time.Now()
	latestByID := map[string]servercollect.Latest{}
	if engine := view.engine; engine != nil {
		for _, latest := range engine.AllLatest() {
			latestByID[latest.ServerID] = latest
		}
	}
	alerts := state.loadServerAlerts()
	activeByServer := map[string][]serveralerts.Alert{}
	for _, alert := range alerts {
		if alert.State == serveralerts.StateActive {
			activeByServer[alert.ServerID] = append(activeByServer[alert.ServerID], alert)
		}
	}
	health := make(map[string]serveralerts.ServerHealth, len(view.rows))
	live := make([]serverassets.Server, 0, len(view.rows))
	for _, server := range view.rows {
		latest, hasLatest := latestByID[server.ID]
		failures := map[servercollect.MetricKind]string{}
		if engine := view.engine; engine != nil {
			for _, kind := range servercollect.AllMetricKinds {
				if msg, ok := engine.LastError(server.ID, kind); ok && msg != "" {
					failures[kind] = msg
				}
			}
		}
		health[server.ID] = serveralerts.DeriveHealth(serveralerts.HealthInput{
			Server:       server,
			Latest:       latest,
			HasLatest:    hasLatest,
			LastErrors:   failures,
			ActiveAlerts: activeByServer[server.ID],
			Now:          now,
		})
		if !server.Deleted {
			live = append(live, server)
		}
	}
	view.health = health
	view.stats = serveralerts.ComputeStats(live, latestByID, health, now)
	view.healthAt = now
}

// serverHealth is one server's cached health, or unknown when nothing has
// been derived for it yet.
func (state *State) serverHealth(id string) serveralerts.ServerHealth {
	if health, ok := state.servers.health[id]; ok {
		return health
	}
	return serveralerts.ServerHealth{Level: serveralerts.HealthUnknown}
}

// serverHealthFunc is the health lookup the sort/filter helpers take.
func (state *State) serverHealthFunc() func(serverassets.Server) serveralerts.ServerHealth {
	return func(server serverassets.Server) serveralerts.ServerHealth {
		return state.serverHealth(server.ID)
	}
}

// serversPageSize is the list's effective page size: the default 50 when
// unset, all rows when the user chose "all".
func (state *State) serversPageSize() int {
	size := state.servers.pageSize
	if size == -1 {
		return max(1, len(state.servers.visible))
	}
	if size <= 0 {
		return 50
	}
	return size
}

// serversPageSizeChoice is the value the footer's switcher shows: 0 for
// "all", the page size otherwise, the default 50 when unset.
func (state *State) serversPageSizeChoice() int {
	switch size := state.servers.pageSize; {
	case size == -1:
		return 0
	case size <= 0:
		return 50
	default:
		return size
	}
}

// serversPages is how many pages the filtered list fills.
func (state *State) serversPages() int {
	size := state.serversPageSize()
	return max(1, (len(state.servers.visible)+size-1)/size)
}

// serversPageRange is the filtered list's slice for the current page.
func (state *State) serversPageRange() (int, int) {
	view := &state.servers
	size := state.serversPageSize()
	start := min(view.page*size, len(view.visible))
	return start, min(start+size, len(view.visible))
}

// clampServersPage keeps the page inside the filtered list.
func (state *State) clampServersPage() {
	view := &state.servers
	pages := state.serversPages()
	view.page = min(max(0, view.page), pages-1)
}

// setServersPageSize applies a footer switcher choice (0 is "all") and
// returns to the first page.
func (state *State) setServersPageSize(choice int) {
	valid := false
	for _, option := range serversPageSizes {
		if option == choice {
			valid = true
			break
		}
	}
	if !valid {
		return
	}
	view := &state.servers
	size := choice
	if choice == 0 {
		size = -1
	}
	if view.pageSize == size {
		return
	}
	view.pageSize = size
	view.page = 0
	view.list.Select(-1)
	view.list.ScrollTo(0, ui.ListStart)
	state.syncServersVisible()
}

// serversIndexForHit maps a page-relative row hit to the filtered list.
func (state *State) serversIndexForHit(id gfx.ID) int {
	start, _ := state.serversPageRange()
	return start + item(id)
}

// locateServer opens the servers page with one server selected, on the
// page that holds its row; the selection survives paging because it is
// kept by id, not by row. It reports whether the server is listed.
func (state *State) locateServer(id string) bool {
	state.openServers()
	view := &state.servers
	if _, ok := state.serverByID(id); !ok {
		return false
	}
	view.selectedID = id
	state.syncServersVisible()
	for index, server := range view.visible {
		if server.ID == id {
			view.page = index / state.serversPageSize()
			state.clampServersPage()
			view.list.Select(-1)
			view.list.ScrollTo(0, ui.ListStart)
			state.syncServersVisible()
			return true
		}
	}
	return false
}

// runServersCollectAll collects every live server, one after another, on
// a background goroutine: CollectNow is synchronous, so the window thread
// never waits on it. Progress shows in the status line and the button
// stays disabled until the run ends; one server's failure never stops
// the others.
func (state *State) runServersCollectAll() {
	view := &state.servers
	if view.collectingAll {
		return
	}
	engine := state.serversEngine()
	if engine == nil {
		view.status = state.serversText("Collection is unavailable: no config store", "采集不可用：没有配置存储")
		return
	}
	var targets []serverassets.Server
	for _, server := range view.rows {
		if !server.Deleted {
			targets = append(targets, server)
		}
	}
	if len(targets) == 0 {
		view.status = state.serversText("No servers to collect", "没有可采集的服务器")
		return
	}
	view.collectingAll = true
	view.collectTotal = len(targets)
	view.collectDone = 0
	view.collectFailed = 0
	view.status = fmt.Sprintf(state.serversText("Collecting… (0/%d)", "正在批量采集（0/%d）…"), len(targets))
	go func() {
		for _, server := range targets {
			_, err := engine.CollectNow(server.ID)
			state.serversPost(func() {
				view.collectDone++
				if err != nil {
					view.collectFailed++
				}
				view.status = fmt.Sprintf(state.serversText("Collecting… (%d/%d)", "正在批量采集（%d/%d）…"), view.collectDone, view.collectTotal)
				state.serversInvalidate()
			})
		}
		state.serversPost(func() {
			view.collectingAll = false
			view.status = fmt.Sprintf(state.serversText("Collection finished: %d ok, %d failed", "批量采集完成：成功 %d 台，失败 %d 台"), view.collectTotal-view.collectFailed, view.collectFailed)
			state.refreshServersHealth()
			state.syncServersVisible()
			state.serversInvalidate()
		})
	}()
}

// serversConfigStore returns the shared config store for the serverassets
// store: the window's own handle when present, otherwise an OpenShared
// handle to the same state file, cached for the window's life.
func (state *State) serversConfigStore() *configstore.Store {
	view := &state.servers
	if state.config != nil {
		return state.config
	}
	if view.sharedStore != nil {
		return view.sharedStore
	}
	store, err := configstore.OpenShared(state.options.StatePath, configFormat)
	if err != nil {
		view.storeErr = err.Error()
		return nil
	}
	view.sharedStore = store
	return store
}

// serversStore returns the asset store, creating it on first use from the
// shared config store. It returns nil when no config store is available;
// callers report that through storeErr instead of panicking.
func (state *State) serversStore() *serverassets.Store {
	view := &state.servers
	if view.store != nil {
		return view.store
	}
	cfg := state.serversConfigStore()
	if cfg == nil {
		return nil
	}
	view.store = serverassets.NewStore(cfg)
	return view.store
}

// serversCredentials is the vault-backed serverconn.CredentialProvider:
// passwords under "server-credential:<asset UUID>", key passphrases under
// "server-passphrase:<asset UUID>". Secrets never enter config.db, logs or
// error strings.
type serversVaultCredentials struct {
	state *State
}

func (c *serversVaultCredentials) Password(id string) (string, bool, error) {
	return c.state.serverSecret(serverassets.PasswordCredentialName(id))
}

func (c *serversVaultCredentials) KeyPassphrase(id string) (string, bool, error) {
	return c.state.serverSecret(serverassets.KeyPassphraseName(id))
}

func (state *State) serverSecret(name string) (string, bool, error) {
	vault := state.secrets.vault
	if vault == nil {
		return "", false, fmt.Errorf("vault is not open")
	}
	value, ok, err := vault.Get(name)
	if err != nil {
		return "", false, err
	}
	return value, ok, nil
}

// saveServerSecret writes a password ("credential") or key passphrase
// ("passphrase") to the vault. The id is the asset UUID; the name is never
// the secret itself.
func (state *State) saveServerSecret(kind, id, value string) error {
	vault := state.secrets.vault
	if vault == nil {
		return fmt.Errorf("vault is not open")
	}
	name := serverassets.PasswordCredentialName(id)
	if kind == "passphrase" {
		name = serverassets.KeyPassphraseName(id)
	}
	if value == "" {
		return vault.Delete(name)
	}
	return vault.Set(name, value)
}

func (state *State) serversCredentials() serverconn.CredentialProvider {
	return &serversVaultCredentials{state: state}
}

// serversDialConn dials one managed server non-interactively: the jump
// host (when the asset names one), the stored-fingerprint check and the
// vault-backed credentials all come from serverconn.Dial, the same path
// the connection test and the collection scheduler use. It is the
// DialFunc the batch executor and the SFTP browser share (both packages
// declare the same signature). A failed dial reports its classified
// reason, never a credential.
func (state *State) serversDialConn(ctx context.Context, server serverassets.Server) (*sshconn.Conn, error) {
	jump := state.serversJumpFor(server)
	conn, result := serverconn.Dial(ctx, server, jump, state.serversCredentials(), state.options.KnownHostsPath)
	if !result.OK {
		if result.Error != nil {
			return nil, result.Error
		}
		return nil, fmt.Errorf("serverconn: dial failed: %s", result.Kind)
	}
	return conn, nil
}

// openServers shows the servers page and loads it. It also starts the
// collection engine on first open, so background collection runs while the
// window lives.
func (state *State) openServers() {
	state.activeSection = sectionServers
	state.refreshServers(true)
	state.serversEngine()
}

// openServersHome shows the servers overview page: the same load as the
// servers page, plus the engine, the alert watcher and a health/stats
// refresh, so the numbers it paints are current.
func (state *State) openServersHome() {
	state.activeSection = sectionServersHome
	state.refreshServers(true)
	state.serversEngine()
	state.serversAlerts()
	state.refreshServersHealth()
}

// openServerAlerts shows the alert center: the same load as the servers
// page, plus the engine, the alert watcher and a health/stats refresh,
// so the list it paints is current.
func (state *State) openServerAlerts() {
	state.activeSection = sectionServerAlerts
	state.refreshServers(true)
	state.serversEngine()
	state.serversAlerts()
	state.refreshServersHealth()
	state.refreshServerAlerts()
}

// refreshServers reloads the groups, jump hosts and servers, then rebuilds
// the visible list. A store error leaves the old rows and reports itself.
func (state *State) refreshServers(report bool) {
	view := &state.servers
	store := state.serversStore()
	if store == nil {
		view.errorText = view.storeErr
		return
	}
	groups, err := store.ListGroups()
	if err != nil {
		view.errorText = err.Error()
		return
	}
	jumps, err := store.ListJumpHosts()
	if err != nil {
		view.errorText = err.Error()
		return
	}
	rows, err := store.ListServers(serverassets.Filter{IncludeDeleted: true})
	if err != nil {
		view.errorText = err.Error()
		return
	}
	view.groups, view.jumps, view.rows = groups, jumps, rows
	view.errorText = ""
	// Health and stats come from the rows just loaded, so they refresh
	// here too; syncServersVisible then sorts by them.
	state.refreshServersHealth()
	state.syncServersVisible()
	if report {
		view.status = ""
	}
}

// syncServersVisible rebuilds the filtered, abnormal-first visible list
// from the search box and the filter choices. A change of any filter
// returns to the first page; a page the filtered list no longer fills
// clamps to the last page. It runs on the window thread and on every
// paint, so it only reads the cached health map — it never derives health
// or touches the store.
func (state *State) syncServersVisible() {
	view := &state.servers
	filter := view.filter
	filter.Query = view.search.String()
	sig := strings.Join([]string{
		filter.Query, filter.GroupID, filter.Tag, filter.OS, filter.Status,
		strconv.FormatBool(filter.IncludeDeleted), view.healthFilter, view.alertFilter,
	}, "\x00")
	if view.filterSig != "" && sig != view.filterSig {
		view.page = 0
	}
	view.filterSig = sig
	health := state.serverHealthFunc()
	filtered := filterManagedServers(view.rows, filter, view.healthFilter, view.alertFilter, health)
	sortManagedServers(filtered, health)
	view.visible = filtered
	state.clampServersPage()
	if view.selectedID != "" {
		if _, ok := state.serverByID(view.selectedID); !ok {
			view.selectedID = ""
		}
	}
	pages := state.serversPages()
	view.countText = fmt.Sprintf(state.serversText("Page %d of %d · %d servers", "第 %d/%d 页 · 共 %d 台"), view.page+1, pages, len(view.visible))
	view.pageText = fmt.Sprintf(state.serversText("Page %d of %d", "第 %d/%d 页"), view.page+1, pages)
	view.pageNumberText = strconv.Itoa(view.page + 1)
}

// serverByID finds a loaded server by asset id.
func (state *State) serverByID(id string) (serverassets.Server, bool) {
	for _, server := range state.servers.rows {
		if server.ID == id {
			return server, true
		}
	}
	return serverassets.Server{}, false
}

// serversStatusLabel is the status column's wording for a server.
func (state *State) serversStatusLabel(server serverassets.Server) string {
	switch serverStatusOf(server) {
	case "ok":
		return state.serversText("Online", "在线")
	case "failed":
		return state.serversText("Offline", "离线")
	default:
		return state.serversText("Unknown", "未知")
	}
}

// serversJumpFor returns the jump host a server routes through, if any.
func (state *State) serversJumpFor(server serverassets.Server) *serverassets.JumpHost {
	if server.JumpHostID == "" {
		return nil
	}
	for i := range state.servers.jumps {
		if state.servers.jumps[i].ID == server.JumpHostID {
			return &state.servers.jumps[i]
		}
	}
	return nil
}

// serversPost runs work on the window thread when the app gave us a Post
// (the real window), or directly when there is none (headless tests, which
// are single-threaded). Background test goroutines always go through it so
// they never mutate State from a worker.
func (state *State) serversPost(fn func()) {
	if state.post != nil {
		state.post(fn)
		return
	}
	fn()
}

func (state *State) serversInvalidate() {
	if state.options.Invalidate != nil {
		state.options.Invalidate()
	}
}

// runServerTest tests one server in the background; the result is applied
// on the window thread and the page repaints through the invalidator.
func (state *State) runServerTest(id string) {
	server, ok := state.serverByID(id)
	if !ok || state.servers.testing[id] {
		return
	}
	if state.servers.testing == nil {
		state.servers.testing = map[string]bool{}
	}
	state.servers.testing[id] = true
	jump := state.serversJumpFor(server)
	creds := state.serversCredentials()
	knownHostsPath := state.options.KnownHostsPath
	go func(server serverassets.Server, jump *serverassets.JumpHost) {
		result := serverconn.TestConnection(context.Background(), server, jump, creds, knownHostsPath)
		state.serversPost(func() {
			delete(state.servers.testing, server.ID)
			state.applyServerTestResult(result)
			state.serversInvalidate()
		})
	}(server, jump)
}

// runServerTestsAll tests every visible, non-deleted server in the
// background (four at a time). The results arrive together through the
// same path as a single test.
func (state *State) runServerTestsAll() {
	if state.servers.testingAll {
		return
	}
	state.servers.testingAll = true
	servers := append([]serverassets.Server(nil), state.servers.visible...)
	jumps := map[string]*serverassets.JumpHost{}
	for i := range state.servers.jumps {
		jumps[state.servers.jumps[i].ID] = &state.servers.jumps[i]
	}
	creds := state.serversCredentials()
	knownHostsPath := state.options.KnownHostsPath
	go func() {
		results := serverconn.TestConnections(context.Background(), servers, jumps, creds, knownHostsPath, 4)
		state.serversPost(func() {
			state.servers.testingAll = false
			for _, result := range results {
				state.applyServerTestResult(result)
			}
			state.serversInvalidate()
		})
	}()
}

// applyServerTestResult records one test outcome on the window thread. The
// last check is written back with SetCheckResult, but a fingerprint that
// differs from the stored one is never saved on its own: it waits in
// pendingFP for the user to confirm (or dismiss) it. A fingerprint that
// matches the stored one needs no confirmation even when serverconn still
// reports it unknown — the known-hosts file is a separate trust store and
// writing through to it is a follow-up, not this phase.
func (state *State) applyServerTestResult(result serverconn.TestResult) {
	view := &state.servers
	if view.results == nil {
		view.results = map[string]serverconn.TestResult{}
	}
	view.results[result.ServerID] = result
	server, ok := state.serverByID(result.ServerID)
	if !ok {
		return
	}
	errText := ""
	if result.Error != nil {
		errText = result.Error.Error()
	}
	changed := result.Fingerprint != "" && result.Fingerprint != server.HostKeyFingerprint
	if store := state.serversStore(); store != nil {
		if _, err := store.SetCheckResult(server.ID, serverassets.CheckResult{
			At:    time.Now(),
			OK:    result.OK,
			Stage: string(result.Stage),
			Error: errText,
		}); err != nil && !errors.Is(err, serverassets.ErrNotFound) {
			// A deleted server cannot be checked; that is not an error to
			// report — the row is already going away.
			view.errorText = err.Error()
			return
		}
	}
	if changed {
		view.pendingFP = &serversFingerprintConfirm{
			serverID:    server.ID,
			serverName:  server.Name,
			fingerprint: result.Fingerprint,
			known:       server.HostKeyFingerprint,
		}
		view.status = state.serversText("Host key changed — confirm the fingerprint", "主机指纹已变化 — 请确认指纹")
	} else if result.OK {
		view.status = fmt.Sprintf(state.serversText("%s is online", "%s 在线"), server.Name)
	} else {
		view.status = fmt.Sprintf(state.serversText("%s failed: %s", "%s 测试失败：%s"), server.Name, errText)
	}
	state.refreshServers(false)
}

// confirmServerFingerprint saves a pending fingerprint as the server's
// trusted one, after the user confirmed it in the dialog. This is the only
// place a fingerprint is written.
func (state *State) confirmServerFingerprint() {
	view := &state.servers
	pending := view.pendingFP
	if pending == nil {
		return
	}
	server, ok := state.serverByID(pending.serverID)
	if !ok {
		view.pendingFP = nil
		return
	}
	server.HostKeyFingerprint = pending.fingerprint
	store := state.serversStore()
	if store == nil {
		view.errorText = view.storeErr
		return
	}
	if _, err := store.UpdateServer(server); err != nil {
		view.errorText = err.Error()
		return
	}
	view.pendingFP = nil
	view.status = fmt.Sprintf(state.serversText("Fingerprint saved for %s", "已保存 %s 的指纹"), server.Name)
	state.refreshServers(false)
}

// restoreServer brings a soft-deleted server back, then re-tests it: the
// test writes the fresh LastCheck, and a changed fingerprint still waits
// for confirmation.
func (state *State) restoreServer(id string) {
	store := state.serversStore()
	if store == nil {
		state.servers.errorText = state.servers.storeErr
		return
	}
	if _, err := store.RestoreServer(id); err != nil {
		state.servers.errorText = err.Error()
		return
	}
	state.refreshServers(false)
	state.runServerTest(id)
}

// serverToHost converts a managed server into an ephemeral Host for
// openHostSession. It is never written to the host catalog: ID stays zero
// and CatalogID is derived ("server:<asset UUID>") so a terminal tab can
// tell it apart from a saved host. The key file travels in SSHOptions; a
// jump host is not applied here — the terminal path has no per-host jump
// route yet, and the test path (serverconn) is where jumps are honoured.
func serverToHost(server serverassets.Server) Host {
	address := server.Address
	if address == "" {
		address = server.Hostname
	}
	host := Host{
		CatalogID: "server:" + server.ID,
		Label:     server.Name,
		Address:   address,
		User:      server.User,
		Port:      uint16(serverPortOrDefault(server.Port)),
	}
	switch server.AuthMethod {
	case serverassets.AuthKey:
		host.Auth = authPrivateKey
	default:
		host.Auth = authPassword
	}
	if server.IdentityFile != "" {
		host.SSHOptions = &sshconfig.ConnectionOptions{IdentityFiles: []string{server.IdentityFile}}
	}
	host.finish()
	return host
}

// openServerTerminal opens a terminal tab for a server through the existing
// host path. A saved password is offered to that one connection only, via
// Host.linkPassword, which the connection clears when it is used; nothing
// is written to the catalog and no password is logged.
func (state *State) openServerTerminal(server serverassets.Server) {
	if state.appAccessBlocked() {
		state.servers.status = state.messages.AppLock.LockedNotice
		return
	}
	host := serverToHost(server)
	if server.AuthMethod == serverassets.AuthPassword {
		creds := state.serversCredentials()
		if password, found, err := creds.Password(server.ID); err == nil && found {
			host.linkPassword = password
		} else if server.CredentialRef != "" {
			if password, found, err := creds.Password(server.CredentialRef); err == nil && found {
				host.linkPassword = password
			}
		}
	}
	state.openHostSession(host)
}

// serversGroupName resolves a group id for display; "" is "ungrouped".
func (state *State) serversGroupName(id string) string {
	if id == "" {
		return state.serversText("Ungrouped", "未分组")
	}
	for _, group := range state.servers.groups {
		if group.ID == id {
			return group.Name
		}
	}
	return id
}

// serversEditorFor returns the text editor a dialog field id owns, so typing
// reaches the servers dialogs.
func (state *State) serversEditorFor(hit gfx.ID) *text.Editor {
	dlg := &state.servers.editor
	if !dlg.open {
		return nil
	}
	switch hit {
	case hitServersDlgName:
		return &dlg.name
	case hitServersDlgHostname:
		return &dlg.hostname
	case hitServersDlgAddress:
		return &dlg.address
	case hitServersDlgPort:
		return &dlg.port
	case hitServersDlgUser:
		return &dlg.user
	case hitServersDlgIdentity:
		return &dlg.identityFile
	case hitServersDlgPassword:
		return &dlg.password
	case hitServersDlgTags:
		return &dlg.tags
	case hitServersDlgNotes:
		return &dlg.notes
	}
	jumps := &state.servers.jumpsDlg
	if jumps.open {
		switch hit {
		case hitServersJumpName:
			return &jumps.name
		case hitServersJumpAddress:
			return &jumps.address
		case hitServersJumpPort:
			return &jumps.port
		case hitServersJumpUser:
			return &jumps.user
		}
	}
	imp := &state.servers.importDlg
	if imp.open {
		switch hit {
		case hitServersImportPath:
			return &imp.path
		}
	}
	// The batch page's command box and its dialogs, and the SFTP page's
	// path box and its dialogs, type through the same route.
	batch := &state.servers.batch
	switch hit {
	case hitServersBatchCommand:
		if state.activeSection == sectionServersBatch {
			return &batch.command
		}
	case hitServersBatchConfirmPhrase:
		if batch.confirm.stage == 2 {
			return &batch.confirmPhrase
		}
	case hitServersBatchDlgName:
		if batch.templateDlg.open {
			return &batch.templateDlg.name
		}
	case hitServersBatchDlgCommand:
		if batch.templateDlg.open {
			return &batch.templateDlg.command
		}
	case hitServersBatchDlgDescription:
		if batch.templateDlg.open {
			return &batch.templateDlg.desc
		}
	case hitServersBatchDlgOS:
		if batch.templateDlg.open {
			return &batch.templateDlg.os
		}
	}
	sftp := &state.servers.sftp
	switch hit {
	case hitServersSFTPPath:
		if state.activeSection == sectionServersSFTP {
			return &sftp.pathEditor
		}
	case hitServersSFTPDlgMkdirName:
		if sftp.mkdirDlg {
			return &sftp.mkdirName
		}
	case hitServersSFTPDlgRenameName:
		if sftp.renameDlg {
			return &sftp.renameName
		}
	}
	return nil
}

// anyDialogOpen reports whether any servers dialog is up, so the page's
// search field does not steal its typing.
func (view *serversState) anyDialogOpen() bool {
	return view.editor.open || view.jumpsDlg.open || view.importDlg.open ||
		view.deleteID != "" || view.pendingFP != nil ||
		view.batch.confirm.stage != 0 || view.batch.templateDlg.open ||
		view.batch.templateDeleteID != "" ||
		view.sftp.mkdirDlg || view.sftp.renameDlg || view.sftp.deleteName != ""
}

// serversKey handles Escape (close the topmost dialog) and Delete (ask to
// delete the selected server) on the servers page. The batch and SFTP
// pages share the Escape half: it closes their dialogs and clears their
// detail selections, and never touches the servers list.
func (state *State) serversKey(event gfx.KeyEvent) (gfx.EventResult, bool) {
	view := &state.servers
	if event.Repeat {
		return gfx.EventResult{}, false
	}
	switch state.activeSection {
	case sectionServersBatch:
		if event.Key != gfx.KeyEscape {
			return gfx.EventResult{}, false
		}
		if view.batch.confirm.stage != 0 {
			view.batch.confirm = batchConfirmState{}
			view.batch.confirmErr = ""
			return gfx.EventResult{Handled: true, Repaint: true}, true
		}
		if view.batch.templateDeleteID != "" {
			view.batch.templateDeleteID = ""
			return gfx.EventResult{Handled: true, Repaint: true}, true
		}
		if view.batch.templateDlg.open {
			view.batch.templateDlg.open = false
			return gfx.EventResult{Handled: true, Repaint: true}, true
		}
		if view.batch.detailID != "" {
			view.batch.detailID = ""
			return gfx.EventResult{Handled: true, Repaint: true}, true
		}
		if view.batch.historyDetailID != "" {
			view.batch.historyDetailID = ""
			return gfx.EventResult{Handled: true, Repaint: true}, true
		}
		return gfx.EventResult{}, false
	case sectionServersSFTP:
		if event.Key != gfx.KeyEscape {
			return gfx.EventResult{}, false
		}
		if view.sftp.mkdirDlg {
			view.sftp.mkdirDlg = false
			return gfx.EventResult{Handled: true, Repaint: true}, true
		}
		if view.sftp.renameDlg {
			view.sftp.renameDlg = false
			return gfx.EventResult{Handled: true, Repaint: true}, true
		}
		if view.sftp.deleteName != "" {
			view.sftp.deleteName = ""
			return gfx.EventResult{Handled: true, Repaint: true}, true
		}
		if view.sftp.selected != "" {
			view.sftp.selected = ""
			return gfx.EventResult{Handled: true, Repaint: true}, true
		}
		return gfx.EventResult{}, false
	case sectionServers:
	default:
		return gfx.EventResult{}, false
	}
	if event.Key == gfx.KeyEscape {
		if view.pendingFP != nil {
			view.pendingFP = nil
			return gfx.EventResult{Handled: true, Repaint: true}, true
		}
		if view.deleteID != "" {
			view.deleteID = ""
			return gfx.EventResult{Handled: true, Repaint: true}, true
		}
		if view.importDlg.open {
			view.importDlg.open = false
			return gfx.EventResult{Handled: true, Repaint: true}, true
		}
		if view.jumpsDlg.open {
			view.jumpsDlg.open = false
			return gfx.EventResult{Handled: true, Repaint: true}, true
		}
		if view.editor.open {
			view.editor.open = false
			return gfx.EventResult{Handled: true, Repaint: true}, true
		}
	}
	if event.Key == gfx.KeyDelete && view.selectedID != "" && !view.anyDialogOpen() {
		if _, ok := state.serverByID(view.selectedID); ok {
			view.deleteID = view.selectedID
			return gfx.EventResult{Handled: true, Repaint: true}, true
		}
	}
	return gfx.EventResult{}, false
}
