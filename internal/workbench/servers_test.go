package workbench

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/vivaui/viva/app"
	"github.com/vivaui/viva/gfx"
	"motty/internal/i18n"
	"motty/internal/serveralerts"
	"motty/internal/serverassets"
	"motty/internal/servercollect"
	"motty/internal/serverconn"
)

// The servers page is reachable by its saved key, its section does not
// collide with any other section, and it sits in the sidebar right after
// Hosts.
func TestNavServersKeyFallback(t *testing.T) {
	page, ok := navPageFromKey("servers")
	if !ok || page != navPageServers {
		t.Fatalf("navPageFromKey(%q) = %v, %v, want navPageServers", "servers", page, ok)
	}
	sections := []uint8{
		sectionHosts, sectionTerminal, sectionKeys, sectionForwards,
		sectionProxies, sectionSettings, sectionKnownHosts, sectionSnippets,
		sectionNotes, sectionDebug, sectionDashboard, sectionSessionLogs,
		sectionSFTP, sectionWorkspaces, sectionServers, sectionServersHome,
		sectionServerAlerts, sectionServersBatch, sectionServersSFTP,
	}
	seen := map[uint8]bool{}
	for _, section := range sections {
		if seen[section] {
			t.Fatalf("section %d collides with another section", section)
		}
		seen[section] = true
	}
	if len(sidebarPages) < 4 || sidebarPages[0].page != navPageHosts || sidebarPages[1].page != navPageServersHome ||
		sidebarPages[2].page != navPageServers || sidebarPages[3].page != navPageServersBatch {
		t.Fatalf("sidebarPages[0:4] = %v, want hosts, servers-home, servers, servers-batch", sidebarPages[:4])
	}
	if len(sidebarPages) < 6 || sidebarPages[4].page != navPageServersSFTP || sidebarPages[5].page != navPageServerAlerts {
		t.Fatalf("sidebarPages[4:6] = %v, want servers-sftp, server-alerts", sidebarPages[4:6])
	}
	if sidebarPages[2].section != sectionServers || sidebarPages[2].id != hitServers {
		t.Fatalf("servers sidebar row = %+v, want sectionServers and hitServers", sidebarPages[2])
	}
	if sidebarPages[1].section != sectionServersHome || sidebarPages[1].id != hitServersHome {
		t.Fatalf("overview sidebar row = %+v, want sectionServersHome and hitServersHome", sidebarPages[1])
	}
	if sidebarPages[3].section != sectionServersBatch || sidebarPages[3].id != hitServersBatch {
		t.Fatalf("batch sidebar row = %+v, want sectionServersBatch and hitServersBatch", sidebarPages[3])
	}
	if sidebarPages[4].section != sectionServersSFTP || sidebarPages[4].id != hitServersSFTP {
		t.Fatalf("sftp sidebar row = %+v, want sectionServersSFTP and hitServersSFTP", sidebarPages[4])
	}
	if sidebarPages[5].section != sectionServerAlerts || sidebarPages[5].id != hitServerAlerts {
		t.Fatalf("alerts sidebar row = %+v, want sectionServerAlerts and hitServerAlerts", sidebarPages[5])
	}
	if page, ok := navPageFromKey("servers-home"); !ok || page != navPageServersHome {
		t.Fatalf("navPageFromKey(%q) = %v, %v, want navPageServersHome", "servers-home", page, ok)
	}
	if page, ok := navPageFromKey("server-alerts"); !ok || page != navPageServerAlerts {
		t.Fatalf("navPageFromKey(%q) = %v, %v, want navPageServerAlerts", "server-alerts", page, ok)
	}
	if page, ok := navPageFromKey("servers-batch"); !ok || page != navPageServersBatch {
		t.Fatalf("navPageFromKey(%q) = %v, %v, want navPageServersBatch", "servers-batch", page, ok)
	}
	if page, ok := navPageFromKey("servers-sftp"); !ok || page != navPageServersSFTP {
		t.Fatalf("navPageFromKey(%q) = %v, %v, want navPageServersSFTP", "servers-sftp", page, ok)
	}
	state := newTestState(t, testOptions(t))
	state.setLanguage(i18n.Chinese)
	if label := state.navPageLabel(navPageServers); label != "服务器" {
		t.Fatalf("navPageLabel(servers) = %q, want 服务器", label)
	}
	if label := state.navPageLabel(navPageServersHome); label != "总览" {
		t.Fatalf("navPageLabel(servers-home) = %q, want 总览", label)
	}
	if label := state.navPageLabel(navPageServerAlerts); label != "告警" {
		t.Fatalf("navPageLabel(server-alerts) = %q, want 告警", label)
	}
	state.setLanguage(i18n.English)
	if label := state.navPageLabel(navPageServers); label != "Servers" {
		t.Fatalf("navPageLabel(servers) = %q, want Servers", label)
	}
	if label := state.navPageLabel(navPageServersHome); label != "Overview" {
		t.Fatalf("navPageLabel(servers-home) = %q, want Overview", label)
	}
	if label := state.navPageLabel(navPageServerAlerts); label != "Alerts" {
		t.Fatalf("navPageLabel(server-alerts) = %q, want Alerts", label)
	}
}

// The list sorts abnormal servers first (failed, then never tested) and
// filters by group, tag, OS, status and free text.
func TestServersFilterAndSort(t *testing.T) {
	now := time.Now()
	servers := []serverassets.Server{
		{Name: "web-ok", Address: "10.0.0.1", OS: "Ubuntu 24.04", GroupID: "g1", Tags: []string{"web"}, LastCheck: &serverassets.CheckResult{At: now, OK: true}},
		{Name: "db-down", Address: "10.0.0.2", OS: "Debian 12", GroupID: "g1", Tags: []string{"db"}, LastCheck: &serverassets.CheckResult{At: now, OK: false}},
		{Name: "cache-new", Address: "10.0.0.3", OS: "Ubuntu 24.04", GroupID: "g2", Tags: []string{"cache"}},
		{Name: "api-down", Address: "10.0.0.4", OS: "Ubuntu 24.04", GroupID: "g2", Tags: []string{"web"}, LastCheck: &serverassets.CheckResult{At: now, OK: false}},
	}
	// Health comes from the derived map now, not from the last check: the
	// two failed servers are offline, the untested one unknown, the
	// healthy one ok.
	healthByName := map[string]serveralerts.ServerHealth{
		"web-ok":    {Level: serveralerts.HealthOK},
		"db-down":   {Level: serveralerts.HealthNetwork},
		"cache-new": {Level: serveralerts.HealthUnknown},
		"api-down":  {Level: serveralerts.HealthNetwork},
	}
	health := func(server serverassets.Server) serveralerts.ServerHealth {
		return healthByName[server.Name]
	}
	sorted := append([]serverassets.Server(nil), servers...)
	sortManagedServers(sorted, health)
	if got := []string{sorted[0].Name, sorted[1].Name, sorted[2].Name, sorted[3].Name}; got[0] != "api-down" || got[1] != "db-down" || got[2] != "cache-new" || got[3] != "web-ok" {
		t.Fatalf("sorted order = %v, want [api-down db-down cache-new web-ok]", got)
	}
	if got := filterManagedServers(servers, serverassets.Filter{GroupID: "g1"}, "", "", health); len(got) != 2 {
		t.Fatalf("group filter = %d servers, want 2", len(got))
	}
	if got := filterManagedServers(servers, serverassets.Filter{Tag: "web"}, "", "", health); len(got) != 2 {
		t.Fatalf("tag filter = %d servers, want 2", len(got))
	}
	if got := filterManagedServers(servers, serverassets.Filter{OS: "Debian 12"}, "", "", health); len(got) != 1 || got[0].Name != "db-down" {
		t.Fatalf("OS filter = %v, want [db-down]", got)
	}
	if got := filterManagedServers(servers, serverassets.Filter{Status: "failed"}, "", "", health); len(got) != 2 {
		t.Fatalf("status filter = %d servers, want 2", len(got))
	}
	if got := filterManagedServers(servers, serverassets.Filter{Query: "10.0.0.3"}, "", "", health); len(got) != 1 || got[0].Name != "cache-new" {
		t.Fatalf("query filter = %v, want [cache-new]", got)
	}
}

// The store's soft delete hides a server until it is restored.
func TestServersStoreSoftDeleteAndRestore(t *testing.T) {
	state := newTestState(t, testOptions(t))
	store := state.serversStore()
	if store == nil {
		t.Fatal("no servers store")
	}
	created, err := store.CreateServer(serverassets.Server{Name: "db", Address: "10.0.0.2", Port: 22, User: "root"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteServer(created.ID); err != nil {
		t.Fatal(err)
	}
	if listed, _ := store.ListServers(serverassets.Filter{}); len(listed) != 0 {
		t.Fatalf("deleted server still listed without IncludeDeleted: %v", listed)
	}
	if listed, _ := store.ListServers(serverassets.Filter{IncludeDeleted: true}); len(listed) != 1 || !listed[0].Deleted {
		t.Fatalf("deleted server not listed with IncludeDeleted: %v", listed)
	}
	if _, err := store.RestoreServer(created.ID); err != nil {
		t.Fatal(err)
	}
	if listed, _ := store.ListServers(serverassets.Filter{}); len(listed) != 1 || listed[0].Deleted {
		t.Fatalf("restored server not listed: %v", listed)
	}
}

// The CSV import preview reports new, skipped and failed rows, and applying
// it creates only the new ones.
func TestServersImportPreviewAndApply(t *testing.T) {
	state := newTestState(t, testOptions(t))
	store := state.serversStore()
	if store == nil {
		t.Fatal("no servers store")
	}
	if _, err := store.CreateServer(serverassets.Server{Name: "web", Address: "10.0.0.1", Port: 22, User: "root"}); err != nil {
		t.Fatal(err)
	}
	csvData := "name,hostname,address,port,user,auth_method,group,tags,os,notes\n" +
		"web,web,10.0.0.1,22,root,password,,,Ubuntu,\n" +
		"db,db,10.0.0.2,22,root,password,,,Debian,\n" +
		",,,22,root,password,,,,\n"
	rows, err := serverassets.ParseCSV(strings.NewReader(csvData))
	if err != nil {
		t.Fatal(err)
	}
	preview, err := store.PreviewImport(rows)
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.ToAdd) != 1 || len(preview.Skipped) != 1 || len(preview.Failed) != 1 {
		t.Fatalf("preview = added %d skipped %d failed %d, want 1/1/1", len(preview.ToAdd), len(preview.Skipped), len(preview.Failed))
	}
	summary, err := store.ApplyImport(preview)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Added != 1 || summary.Skipped != 1 || summary.Failed != 1 {
		t.Fatalf("summary = %+v, want added 1 skipped 1 failed 1", summary)
	}
	if listed, _ := store.ListServers(serverassets.Filter{}); len(listed) != 2 {
		t.Fatalf("after import %d servers, want 2", len(listed))
	}
	if _, err := store.ImportXLSX([]byte("not a spreadsheet")); err == nil {
		t.Fatal("invalid xlsx import did not fail")
	}
}

// A server becomes an ephemeral host for the terminal: never a catalog
// entry, with its key file carried in SSHOptions.
func TestServerToHost(t *testing.T) {
	host := serverToHost(serverassets.Server{
		ID: "srv-9", Name: "db", Hostname: "db.internal", Address: "10.0.0.2",
		Port: 2222, User: "deploy", AuthMethod: serverassets.AuthKey, IdentityFile: "/home/u/.ssh/id_ed25519",
	})
	if host.ID != 0 {
		t.Fatalf("ephemeral host ID = %d, want 0", host.ID)
	}
	if host.CatalogID != "server:srv-9" {
		t.Fatalf("CatalogID = %q, want server:srv-9", host.CatalogID)
	}
	if host.Address != "10.0.0.2" || host.Port != 2222 || host.User != "deploy" || host.Auth != authPrivateKey {
		t.Fatalf("host = %+v, want address 10.0.0.2:2222 deploy key auth", host)
	}
	if host.SSHOptions == nil || len(host.SSHOptions.IdentityFiles) != 1 || host.SSHOptions.IdentityFiles[0] != "/home/u/.ssh/id_ed25519" {
		t.Fatalf("SSHOptions = %+v, want the identity file carried", host.SSHOptions)
	}
}

// A changed fingerprint is never saved by the test itself: it waits for the
// user to confirm, and only the confirm writes it.
func TestServersFingerprintConfirmFlow(t *testing.T) {
	state := newTestState(t, testOptions(t))
	store := state.serversStore()
	if store == nil {
		t.Fatal("no servers store")
	}
	created, err := store.CreateServer(serverassets.Server{Name: "db", Address: "10.0.0.2", Port: 22, User: "root", HostKeyFingerprint: "SHA256:old"})
	if err != nil {
		t.Fatal(err)
	}
	state.openServers()
	state.applyServerTestResult(serverconn.TestResult{ServerID: created.ID, OK: true, Fingerprint: "SHA256:new", Kind: serverconn.KindFingerprintChanged})
	if state.servers.pendingFP == nil || state.servers.pendingFP.fingerprint != "SHA256:new" {
		t.Fatal("a changed fingerprint did not wait for confirmation")
	}
	if server, _ := state.serverByID(created.ID); server.HostKeyFingerprint != "SHA256:old" {
		t.Fatalf("stored fingerprint = %q before confirmation, want SHA256:old", server.HostKeyFingerprint)
	}
	state.confirmServerFingerprint()
	if state.servers.pendingFP != nil {
		t.Fatal("confirmation did not clear the pending fingerprint")
	}
	if server, _ := state.serverByID(created.ID); server.HostKeyFingerprint != "SHA256:new" {
		t.Fatalf("stored fingerprint = %q after confirmation, want SHA256:new", server.HostKeyFingerprint)
	}
	// A second change, dismissed with Escape, leaves the stored key alone.
	state.applyServerTestResult(serverconn.TestResult{ServerID: created.ID, OK: true, Fingerprint: "SHA256:other", Kind: serverconn.KindFingerprintChanged})
	if state.servers.pendingFP == nil {
		t.Fatal("a second changed fingerprint did not wait for confirmation")
	}
	if result, handled := state.serversKey(gfx.KeyEvent{Key: gfx.KeyEscape}); !handled || !result.Handled {
		t.Fatal("Escape did not dismiss the fingerprint dialog")
	}
	if state.servers.pendingFP != nil {
		t.Fatal("Escape did not clear the pending fingerprint")
	}
	if server, _ := state.serverByID(created.ID); server.HostKeyFingerprint != "SHA256:new" {
		t.Fatalf("stored fingerprint = %q after dismiss, want SHA256:new", server.HostKeyFingerprint)
	}
}

// Server passwords live in the vault under server-credential:<asset UUID>,
// and the provider reads them back.
func TestServersVaultCredentials(t *testing.T) {
	state := newTestState(t, testOptions(t))
	if err := state.saveServerSecret("credential", "srv-1", "hunter2"); err != nil {
		t.Fatal(err)
	}
	got, found, err := state.serversCredentials().Password("srv-1")
	if err != nil || !found || got != "hunter2" {
		t.Fatalf("vault password = %q, found %v, %v, want hunter2", got, found, err)
	}
	if _, found, err := state.serversCredentials().Password("srv-missing"); err != nil || found {
		t.Fatalf("a missing vault secret = found %v, %v, want not found", found, err)
	}
}

// The page paints its search box and opens the editor from its New button;
// saving the editor creates the server.
func TestServersPagePaintsAndEditorSaves(t *testing.T) {
	state := newTestState(t, testOptions(t))
	state.openServers()
	size := gfx.Size{Width: 1400, Height: 900}
	canvas := &gfx.Canvas{}
	state.Paint(canvas, size)
	if _, ok := hitByID(canvas, hitServersSearch); !ok {
		t.Fatal("the servers search box is not painted")
	}
	if _, ok := hitByID(canvas, hitServers); !ok {
		t.Fatal("the Servers sidebar row is not painted")
	}
	state.Click(hitServersNew)
	if !state.servers.editor.open {
		t.Fatal("New did not open the server editor")
	}
	dlg := &state.servers.editor
	setEditor(&dlg.name, "web")
	setEditor(&dlg.address, "10.0.0.1")
	setEditor(&dlg.port, "22")
	setEditor(&dlg.user, "root")
	setEditor(&dlg.password, "hunter2")
	state.saveServersEditor()
	if dlg.open {
		t.Fatalf("editor did not close after save: %s", dlg.err)
	}
	if len(state.servers.visible) != 1 || state.servers.visible[0].Name != "web" {
		t.Fatalf("visible servers = %v, want [web]", state.servers.visible)
	}
	if got, found, err := state.serversCredentials().Password(state.servers.visible[0].ID); err != nil || !found || got != "hunter2" {
		t.Fatalf("saved vault password = %q, found %v, %v", got, found, err)
	}
}

// Resource cells show the latest collected values, never 0 for missing data,
// and flag expired values instead of showing them as current.
func TestServersResourceCells(t *testing.T) {
	pct := func(v float64) *float64 { return &v }
	now := time.Now()
	fresh := servercollect.Latest{ServerID: "s1", Values: map[servercollect.MetricKind]servercollect.MetricValue{
		servercollect.MetricCPU: {
			Kind: servercollect.MetricCPU, CollectedAt: now, StaleAfter: 3 * time.Minute,
			CPU: &servercollect.CPUInfo{Percent: pct(42)},
		},
		servercollect.MetricMemory: {
			Kind: servercollect.MetricMemory, CollectedAt: now, StaleAfter: 3 * time.Minute,
			Memory: &servercollect.MemoryInfo{Percent: pct(62)},
		},
		servercollect.MetricDiskUsage: {
			Kind: servercollect.MetricDiskUsage, CollectedAt: now, StaleAfter: 30 * time.Minute,
			DiskUsage: &servercollect.DiskUsageInfo{Filesystems: []servercollect.Filesystem{
				{Mount: "/", Total: 100, Used: 50},
				{Mount: "/data", Total: 300, Used: 150},
			}},
		},
	}}
	cpu, mem, disk, expired := serversResourceCells(fresh, now)
	if cpu != "42%" || mem != "62%" || disk != "50%" || expired {
		t.Fatalf("fresh cells = %q, %q, %q, expired=%v, want 42%%, 62%%, 50%%, false", cpu, mem, disk, expired)
	}

	// Ten minutes later the 1m kinds are past 3x their interval, but the
	// values are still shown — only flagged as expired.
	later := now.Add(10 * time.Minute)
	cpu, mem, disk, expired = serversResourceCells(fresh, later)
	if cpu != "42%" || mem != "62%" || disk != "50%" || !expired {
		t.Fatalf("stale cells = %q, %q, %q, expired=%v, want 42%%, 62%%, 50%%, true", cpu, mem, disk, expired)
	}

	// A server never collected successfully shows no numbers at all.
	cpu, mem, disk, expired = serversResourceCells(servercollect.Latest{}, now)
	if cpu != "—" || mem != "—" || disk != "—" || expired {
		t.Fatalf("missing cells = %q, %q, %q, expired=%v, want —, —, —, false", cpu, mem, disk, expired)
	}

	// A baseline-only CPU sample (percent not yet known) stays "—" and is
	// not flagged expired; an empty disk list stays "—" too.
	baseline := servercollect.Latest{ServerID: "s1", Values: map[servercollect.MetricKind]servercollect.MetricValue{
		servercollect.MetricCPU: {
			Kind: servercollect.MetricCPU, CollectedAt: now, StaleAfter: 3 * time.Minute,
			CPU: &servercollect.CPUInfo{},
		},
		servercollect.MetricDiskUsage: {
			Kind: servercollect.MetricDiskUsage, CollectedAt: now, StaleAfter: 30 * time.Minute,
			DiskUsage: &servercollect.DiskUsageInfo{},
		},
	}}
	cpu, mem, disk, expired = serversResourceCells(baseline, now)
	if cpu != "—" || mem != "—" || disk != "—" || expired {
		t.Fatalf("baseline cells = %q, %q, %q, expired=%v, want —, —, —, false", cpu, mem, disk, expired)
	}
}

// The engine starts lazily, reports no values for unknown servers, stops
// cleanly on closeServers, and starts again afterwards.
func TestServersEngineLifecycle(t *testing.T) {
	state := newTestState(t, testOptions(t))
	engine := state.serversEngine()
	if engine == nil {
		t.Fatal("serversEngine() = nil")
	}
	if _, ok := engine.Latest("no-such-server"); ok {
		t.Fatal("Latest(unknown server) returned a value")
	}
	if _, ok := state.serversLatest("no-such-server"); ok {
		t.Fatal("serversLatest(unknown server) returned a value")
	}
	state.closeServers()
	if state.servers.engine != nil {
		t.Fatal("closeServers did not clear the engine")
	}
	if state.serversEngine() == nil {
		t.Fatal("serversEngine() after close = nil")
	}
	state.closeServers()
}

// The list sorts in the spec §14.2 five tiers — offline, SSH abnormal,
// collection failed, resource alert, normal — each tier by name, and a row
// shows every abnormal identifier it carries.
func TestServersHealthSortFiveTiers(t *testing.T) {
	healthByID := map[string]serveralerts.ServerHealth{
		"off-b":   {Level: serveralerts.HealthSSH},
		"off-a":   {Level: serveralerts.HealthNetwork},
		"auth":    {Level: serveralerts.HealthAuth},
		"collect": {Level: serveralerts.HealthCollect, HasAlert: true},
		"alert":   {Level: serveralerts.HealthOK, HasAlert: true},
		"ok":      {Level: serveralerts.HealthOK},
		"new":     {Level: serveralerts.HealthUnknown},
	}
	health := func(server serverassets.Server) serveralerts.ServerHealth {
		return healthByID[server.ID]
	}
	servers := []serverassets.Server{
		{ID: "ok", Name: "ok"},
		{ID: "alert", Name: "alert"},
		{ID: "collect", Name: "collect"},
		{ID: "auth", Name: "auth"},
		{ID: "off-b", Name: "b-off"},
		{ID: "off-a", Name: "a-off"},
		{ID: "new", Name: "z-new"},
	}
	sorted := append([]serverassets.Server(nil), servers...)
	sortManagedServers(sorted, health)
	got := []string{sorted[0].ID, sorted[1].ID, sorted[2].ID, sorted[3].ID, sorted[4].ID, sorted[5].ID, sorted[6].ID}
	want := []string{"off-a", "off-b", "auth", "collect", "alert", "ok", "new"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sorted order = %v, want %v", got, want)
		}
	}

	state := newTestState(t, testOptions(t))
	state.setLanguage(i18n.Chinese)
	flags := state.serverFlagsText(
		serverassets.Server{Deleted: true},
		serveralerts.ServerHealth{Level: serveralerts.HealthNetwork, HasAlert: true}, true, true)
	if flags != "离线 · 资源告警 · 数据过期 · 指纹变化 · 已删除" {
		t.Fatalf("flags = %q, want 离线 · 资源告警 · 数据过期 · 指纹变化 · 已删除", flags)
	}
	if flags := state.serverFlagsText(serverassets.Server{}, serveralerts.ServerHealth{Level: serveralerts.HealthAuth}, false, false); flags != "认证失败" {
		t.Fatalf("auth flags = %q, want 认证失败", flags)
	}
	if flags := state.serverFlagsText(serverassets.Server{}, serveralerts.ServerHealth{Level: serveralerts.HealthCollect}, false, false); flags != "采集异常" {
		t.Fatalf("collect flags = %q, want 采集异常", flags)
	}
	if flags := state.serverFlagsText(serverassets.Server{}, serveralerts.ServerHealth{Level: serveralerts.HealthOK}, false, false); flags != "" {
		t.Fatalf("healthy flags = %q, want empty", flags)
	}
}

// The health and alert filters stack with the existing filters: a
// collection failure that also carries an alert is a collection failure,
// and the alert filter follows HasAlert across tiers.
func TestServersHealthAndAlertFilters(t *testing.T) {
	healthByID := map[string]serveralerts.ServerHealth{
		"off":     {Level: serveralerts.HealthNetwork},
		"auth":    {Level: serveralerts.HealthAuth},
		"collect": {Level: serveralerts.HealthCollect, HasAlert: true},
		"alert":   {Level: serveralerts.HealthOK, HasAlert: true},
		"ok":      {Level: serveralerts.HealthOK},
		"new":     {Level: serveralerts.HealthUnknown},
	}
	health := func(server serverassets.Server) serveralerts.ServerHealth {
		return healthByID[server.ID]
	}
	servers := []serverassets.Server{
		{ID: "off", Name: "off"}, {ID: "auth", Name: "auth"},
		{ID: "collect", Name: "collect"}, {ID: "alert", Name: "alert"},
		{ID: "ok", Name: "ok"}, {ID: "new", Name: "new"},
	}
	cases := []struct {
		healthFilter, alertFilter string
		want                      int
	}{
		{"offline", "", 1},
		{"auth", "", 1},
		{"collect", "", 1},
		{"alert", "", 1},
		{"ok", "", 2},
		{"", "has", 2},
		{"", "none", 4},
		{"collect", "has", 1},
		{"ok", "has", 0},
	}
	for _, tc := range cases {
		got := filterManagedServers(servers, serverassets.Filter{}, tc.healthFilter, tc.alertFilter, health)
		if len(got) != tc.want {
			t.Fatalf("filter health=%q alert=%q = %d servers, want %d", tc.healthFilter, tc.alertFilter, len(got), tc.want)
		}
	}
}

// Pagination slices the filtered list, a filter change returns to the
// first page, and the selected row survives paging.
func TestServersPagination(t *testing.T) {
	state := newTestState(t, testOptions(t))
	for i := 0; i < 55; i++ {
		state.servers.rows = append(state.servers.rows, serverassets.Server{
			ID: fmt.Sprintf("srv-%02d", i), Name: fmt.Sprintf("srv-%02d", i),
			Address: fmt.Sprintf("10.0.0.%d", i+1),
		})
	}
	state.syncServersVisible()
	if len(state.servers.visible) != 55 {
		t.Fatalf("visible = %d servers, want 55", len(state.servers.visible))
	}
	if state.serversPages() != 2 {
		t.Fatalf("pages = %d, want 2", state.serversPages())
	}
	if start, end := state.serversPageRange(); start != 0 || end != 50 {
		t.Fatalf("page range = (%d, %d), want (0, 50)", start, end)
	}
	if state.servers.countText != "Page 1 of 2 · 55 servers" {
		t.Fatalf("countText = %q, want Page 1 of 2 · 55 servers", state.servers.countText)
	}
	state.servers.page = 1
	if start, end := state.serversPageRange(); start != 50 || end != 55 {
		t.Fatalf("page 2 range = (%d, %d), want (50, 55)", start, end)
	}
	// A filter change returns to the first page.
	state.servers.healthFilter = "ok"
	state.syncServersVisible()
	if state.servers.page != 0 {
		t.Fatalf("page after filter change = %d, want 0", state.servers.page)
	}
	if state.servers.countText != "Page 1 of 2 · 55 servers" {
		t.Fatalf("countText after filter = %q, want Page 1 of 2 · 55 servers", state.servers.countText)
	}
	state.setServersPageSize(20)
	if state.serversPages() != 3 {
		t.Fatalf("pages at 20 per page = %d, want 3", state.serversPages())
	}
	state.setServersPageSize(0)
	if state.serversPages() != 1 {
		t.Fatalf("pages at all = %d, want 1", state.serversPages())
	}
	if start, end := state.serversPageRange(); start != 0 || end != 55 {
		t.Fatalf("all range = (%d, %d), want (0, 55)", start, end)
	}
	// The selected row survives paging: it is kept by id, not by row.
	state.servers.selectedID = "srv-54"
	state.servers.page = 0
	state.syncServersVisible()
	if state.servers.selectedID != "srv-54" {
		t.Fatalf("selectedID after paging = %q, want srv-54", state.servers.selectedID)
	}
	// The row hit maps through the page: page 3 at 20 per page starts
	// at srv-40.
	state.setServersPageSize(20)
	state.servers.page = 2
	if index := state.serversIndexForHit(hitServersRowList.At(0)); index != 40 {
		t.Fatalf("row hit on page 3 at 20 per page = %d, want 40", index)
	}
}

// Clicking an alert row marks it read and opens the servers page with
// that server selected.
func TestServerAlertsRowClickLocates(t *testing.T) {
	state := newTestState(t, testOptions(t))
	store := state.serversStore()
	if store == nil {
		t.Fatal("no servers store")
	}
	// A closed loopback port, so the engine the page starts dials and
	// fails at once instead of waiting out a network timeout.
	created, err := store.CreateServer(serverassets.Server{Name: "db", Address: "127.0.0.1", Port: 59999, User: "root"})
	if err != nil {
		t.Fatal(err)
	}
	alertsStore := state.serversAlertsStore()
	if alertsStore == nil {
		t.Fatal("no alerts store")
	}
	alert := serveralerts.Alert{
		ID: "a1", ServerID: created.ID, ServerName: "db",
		Kind: serveralerts.KindCPU, State: serveralerts.StateActive,
		Message: "CPU usage 90%", Value: 90, CreatedAt: time.Now(),
	}
	if err := alertsStore.Save(alert); err != nil {
		t.Fatal(err)
	}
	state.openServerAlerts()
	// The cache is the page's source of truth; seed it with the alert
	// whether or not the store round-tripped it.
	state.servers.alerts = []serveralerts.Alert{alert}
	state.syncServerAlertsVisible()
	if len(state.servers.alertsVisible) != 1 {
		t.Fatalf("alertsVisible = %d, want 1", len(state.servers.alertsVisible))
	}
	state.Click(hitServerAlertsRowList.At(0))
	if !state.servers.alerts[0].Read {
		t.Fatal("clicking the alert row did not mark it read")
	}
	if state.activeSection != sectionServers {
		t.Fatalf("activeSection = %d, want sectionServers", state.activeSection)
	}
	if state.servers.selectedID != created.ID {
		t.Fatalf("selectedID = %q, want %s", state.servers.selectedID, created.ID)
	}
}

// The overview's resource lines show "暂无有效数据" with no valid value
// and the "35%（80/100 台）" form with data, never 0 for missing data.
func TestServersHomeStatsText(t *testing.T) {
	state := newTestState(t, testOptions(t))
	state.setLanguage(i18n.Chinese)
	if got := state.serversAvgStatText(nil, 0, 100); got != "暂无有效数据" {
		t.Fatalf("avg with no data = %q, want 暂无有效数据", got)
	}
	cpu := 35.0
	if got := state.serversAvgStatText(&cpu, 80, 100); got != "35%（80/100 台）" {
		t.Fatalf("avg with data = %q, want 35%%（80/100 台）", got)
	}
	disk := 62.0
	state.servers.stats = serveralerts.Stats{
		DiskPercent:    &disk,
		DiskTotalBytes: 100 * 1024 * 1024 * 1024,
		DiskUsedBytes:  62 * 1024 * 1024 * 1024,
	}
	if got := state.serversDiskStatText(); got != "62%（已用 62.0 GB / 共 100.0 GB）" {
		t.Fatalf("disk stat = %q, want 62%%（已用 62.0 GB / 共 100.0 GB）", got)
	}
	state.servers.stats = serveralerts.Stats{}
	if got := state.serversDiskStatText(); got != "暂无有效数据" {
		t.Fatalf("disk with no data = %q, want 暂无有效数据", got)
	}
}

// The overview and the alert center paint their quick actions and their
// sidebar rows.
func TestServersHomeAndAlertsPaint(t *testing.T) {
	state := newTestState(t, testOptions(t))
	state.openServersHome()
	size := gfx.Size{Width: 1400, Height: 900}
	canvas := &gfx.Canvas{}
	state.Paint(canvas, size)
	if _, ok := hitByID(canvas, hitServersHomeCollectAll); !ok {
		t.Fatal("the overview collect-all button is not painted")
	}
	if _, ok := hitByID(canvas, hitServersHomeBatchCmd); !ok {
		t.Fatal("the overview batch-command button is not painted")
	}
	if _, ok := hitByID(canvas, hitServersHome); !ok {
		t.Fatal("the Overview sidebar row is not painted")
	}
	state.openServerAlerts()
	canvas = &gfx.Canvas{}
	state.Paint(canvas, size)
	if _, ok := hitByID(canvas, hitServerAlertsMarkAllRead); !ok {
		t.Fatal("the alert center mark-all-read button is not painted")
	}
	if _, ok := hitByID(canvas, hitServerAlerts); !ok {
		t.Fatal("the Alerts sidebar row is not painted")
	}
}

// A new alert notifies the desktop with the server name and the kind in
// the title and the message in the body; with notifications off it stays
// silent, and a notification failure is only a status line.
func TestServersNotifyAlert(t *testing.T) {
	state := newTestState(t, testOptions(t))
	var titles, bodies []string
	state.options.Notify = func(note app.Notification) error {
		titles, bodies = append(titles, note.Title), append(bodies, note.Body)
		return nil
	}
	alert := serveralerts.Alert{
		ServerID: "srv-1", ServerName: "db", Kind: serveralerts.KindCPU,
		State: serveralerts.StateActive, Message: "CPU usage 90%",
	}
	state.notifyServerAlert(alert)
	if len(titles) != 1 || len(bodies) != 1 {
		t.Fatalf("notifications = %d, want 1", len(titles))
	}
	if titles[0] != "db · CPU" {
		t.Fatalf("title = %q, want db · CPU", titles[0])
	}
	if bodies[0] != "CPU usage 90%" {
		t.Fatalf("body = %q, want CPU usage 90%%", bodies[0])
	}
	state.notifyOff = true
	state.notifyServerAlert(alert)
	if len(titles) != 1 {
		t.Fatalf("notifications with notifyOff = %d, want 1", len(titles))
	}
	state.notifyOff = false
	state.options.Notify = func(app.Notification) error { return fmt.Errorf("notify failed") }
	state.notifyServerAlert(alert)
	if state.servers.status == "" {
		t.Fatal("a notification failure did not reach the status line")
	}
}

// The alert watcher starts lazily with the page, stops on closeServers,
// and starts again afterwards.
func TestServersAlertsWatcherLifecycle(t *testing.T) {
	state := newTestState(t, testOptions(t))
	if state.serversAlerts() == nil {
		t.Fatal("serversAlerts() = nil")
	}
	if state.servers.alertsWatcher == nil {
		t.Fatal("serversAlerts() did not keep the watcher")
	}
	state.closeServers()
	if state.servers.alertsWatcher != nil {
		t.Fatal("closeServers did not clear the watcher")
	}
	if state.serversAlerts() == nil {
		t.Fatal("serversAlerts() after close = nil")
	}
	state.closeServers()
}
