package workbench

// Servers overview page (phase 4): the home page for managed servers —
// server counts, resource summary, recent alerts and quick actions.
//
// The page paints only cached state: stats and health come from
// refreshServersHealth (run when the page opens and when the engine or
// watcher reports new data) and the alerts from the alert store's cache.
// It never derives health or statistics during Paint, and it never reads
// collection values through any path but the engine's latest values
// (serversLatest / engine.AllLatest), so an expired or missing value is
// shown as missing, never as 0.

import (
	"fmt"
	"sort"

	"github.com/vivaui/viva/gfx"
	"github.com/vivaui/viva/ui"
	"motty/internal/serveralerts"
)

// serversHomeSectionUI is what the overview page's declaration leaves for
// its paint (the empty state's icon slot, as on the servers page).
type serversHomeSectionUI struct {
	empty     bool
	emptyIcon uint8
	emptySlot ui.Box
	draw      func(*ui.DrawContext)
}

// declareServersHomeWorkspace declares the servers overview page in the
// workbench page.
func (state *State) declareServersHomeWorkspace(c ui.C) {
	view, pal := &state.servers, state.pal
	layout := &state.sectionBodies.serversHome
	layout.empty, layout.emptyIcon = false, uint8(iconServers)
	if layout.draw == nil {
		layout.draw = func(d *ui.DrawContext) { state.paintServersHomeWorkspace(d.Canvas) }
	}
	stats := view.stats

	body := ui.Column(c).Fill().PadSides(12, serversSidePad, 0, serversSidePad)
	body.Children(func() {
		ui.Column(c).MarginSides(4, 0, 12, 0).Gap(3).Children(func() {
			ui.Text(c, state.messages.NavServersHome).FontSize(18).Weight(gfx.WeightSemibold).Color(pal.text).Ellipsis()
			ui.Paragraph(c, state.serversText("Server counts, resource summary, recent alerts and quick actions.", "服务器数量、资源概况、近期异常与快捷操作。"), 2).FontSize(metrics.FontSizeS).Color(pal.muted)
		})
		// Four counts: total, SSH normal, offline, abnormal. Abnormal
		// follows the stats definition: a level other than ok, or an
		// active alert.
		ui.Row(c).Gap(8).MarginSides(0, 0, 12, 0).Children(func() {
			state.declareServersHomeStat(c, fmt.Sprintf("%d", stats.Total), state.serversText("Servers", "服务器总数"))
			state.declareServersHomeStat(c, fmt.Sprintf("%d", stats.Online), state.serversText("Online", "SSH 正常"))
			state.declareServersHomeStat(c, fmt.Sprintf("%d", stats.Offline), state.serversText("Offline", "离线"))
			state.declareServersHomeStat(c, fmt.Sprintf("%d", stats.Abnormal), state.serversText("Abnormal", "异常"))
		})
		ui.Divider(c).Background(pal.line)
		ui.Column(c).MarginSides(12, 0, 12, 0).Gap(6).Children(func() {
			ui.Text(c, state.serversText("Resources", "资源概况")).FontSize(16).Weight(gfx.WeightSemibold).Color(pal.text).Ellipsis()
			ui.Text(c, fmt.Sprintf(state.serversText("CPU average: %s", "CPU 平均使用率：%s"), state.serversAvgStatText(stats.CPUAvg, stats.CPUCount, stats.Total))).FontSize(metrics.FontSizeS).Color(pal.text).Ellipsis()
			ui.Text(c, fmt.Sprintf(state.serversText("Memory average: %s", "内存平均使用率：%s"), state.serversAvgStatText(stats.MemAvg, stats.MemCount, stats.Total))).FontSize(metrics.FontSizeS).Color(pal.text).Ellipsis()
			ui.Text(c, fmt.Sprintf(state.serversText("Disk overall: %s", "磁盘整体使用率：%s"), state.serversDiskStatText())).FontSize(metrics.FontSizeS).Color(pal.text).Ellipsis()
			if len(stats.HighDisks) > 0 {
				ui.Text(c, fmt.Sprintf(state.serversText("High-usage partitions (%d servers)", "高使用率分区（涉及 %d 台服务器）"), stats.HighDiskServers)).FontSize(metrics.FontSizeS).Weight(gfx.WeightMedium).Color(pal.text).MarginSides(6, 0, 0, 0).Ellipsis()
				for _, disk := range stats.HighDisks {
					ui.Text(c, fmt.Sprintf("%s · %s · %.0f%%", disk.Mount, disk.ServerName, disk.Percent)).FontSize(metrics.FontSizeS).Color(pal.muted).Ellipsis()
				}
			}
		})
		ui.Divider(c).Background(pal.line)
		ui.Column(c).MarginSides(12, 0, 12, 0).Gap(6).Children(func() {
			ui.Text(c, state.serversText("Recent alerts", "近期异常")).FontSize(16).Weight(gfx.WeightSemibold).Color(pal.text).Ellipsis()
			recent := state.recentServerAlerts()
			if len(recent) == 0 {
				ui.Text(c, state.serversText("No alerts", "暂无告警")).FontSize(metrics.FontSizeS).Color(pal.muted).Ellipsis()
			}
			for _, alert := range recent {
				ui.Text(c, state.serverAlertLine(alert)).FontSize(metrics.FontSizeS).Color(pal.text).Ellipsis()
			}
		})
		ui.Spacer(c)
		ui.Row(c).Height(40).Gap(8).Items(ui.Center).MarginSides(8, 0, 4, 0).Children(func() {
			state.declareButtonSized(c, hitServersHomeAdd, state.serversText("Add server", "添加服务器"), iconPlus, true, false, true, toolbarFont, false).Size(128, toolbarHeight)
			state.declareButtonSized(c, hitServersHomeCollectAll, state.serversText("Collect all", "批量采集"), iconRefresh, false, false, !view.collectingAll, toolbarFont, false).Size(128, toolbarHeight)
			state.declareButtonSized(c, hitServersHomeBatchCmd, state.serversText("Batch commands", "批量命令"), iconTerminal, false, false, true, toolbarFont, false).Size(192, toolbarHeight)
			state.declareButtonSized(c, hitServersHomeTerminal, state.serversText("Open SSH terminal", "打开 SSH 工作台"), iconTerminal, false, false, true, toolbarFont, false).Size(160, toolbarHeight)
		})
		ui.Divider(c).Background(pal.line)
		ui.Row(c).Height(49).Gap(10).Items(ui.Center).Children(func() {
			ui.Text(c, view.status).FontSize(metrics.FontSizeS).Color(pal.muted).Grow(1).Ellipsis()
			if view.errorText != "" {
				ui.Text(c, view.errorText).FontSize(metrics.FontSizeS).Color(pal.danger).Grow(1).Ellipsis()
			}
		})
	})
	body.Clip().DrawOver(layout.draw)
}

// declareServersHomeStat declares one of the overview's four counts: a
// large number over its label.
func (state *State) declareServersHomeStat(c ui.C, value, label string) {
	pal := state.pal
	ui.Column(c).Grow(1).Gap(2).Children(func() {
		ui.Text(c, value).FontSize(22).Weight(gfx.WeightSemibold).Color(pal.text).Ellipsis()
		ui.Text(c, label).FontSize(metrics.FontSizeS).Color(pal.muted).Ellipsis()
	})
}

// serversAvgStatText formats one resource average the spec §8.2 way:
// "35%（80/100 台）" — the average over the servers with a valid value,
// with how many took part out of the total. With no valid value it says
// so instead of showing 0.
func (state *State) serversAvgStatText(avg *float64, count, total int) string {
	if avg == nil {
		return state.serversText("No valid data", "暂无有效数据")
	}
	return fmt.Sprintf(state.serversText("%.0f%% (%d/%d servers)", "%.0f%%（%d/%d 台）"), *avg, count, total)
}

// serversDiskStatText formats the capacity-weighted overall disk usage
// from the cached stats, with the totals behind it. With no valid value
// it says so instead of showing 0.
func (state *State) serversDiskStatText() string {
	stats := state.servers.stats
	if stats.DiskPercent == nil {
		return state.serversText("No valid data", "暂无有效数据")
	}
	return fmt.Sprintf(state.serversText("%.0f%% (used %s of %s)", "%.0f%%（已用 %s / 共 %s）"),
		*stats.DiskPercent, formatBytes(int64(stats.DiskUsedBytes)), formatBytes(int64(stats.DiskTotalBytes)))
}

// recentServerAlerts is the newest 8 alerts, active or recovered, newest
// first. It sorts a copy, so it holds whatever order the cache arrived in.
func (state *State) recentServerAlerts() []serveralerts.Alert {
	copied := append([]serveralerts.Alert(nil), state.servers.alerts...)
	sort.Slice(copied, func(i, j int) bool {
		if !copied[i].CreatedAt.Equal(copied[j].CreatedAt) {
			return copied[i].CreatedAt.After(copied[j].CreatedAt)
		}
		return copied[i].ID < copied[j].ID
	})
	if len(copied) > 8 {
		copied = copied[:8]
	}
	return copied
}

// serverAlertLine is one alert as a single line: server, kind, message,
// time and state.
func (state *State) serverAlertLine(alert serveralerts.Alert) string {
	name := alert.ServerName
	if name == "" {
		name = alert.ServerID
	}
	return fmt.Sprintf("%s · %s · %s · %s · %s",
		name, state.serverAlertKindLabel(alert.Kind), alert.Message,
		alert.CreatedAt.Format("2006-01-02 15:04"), state.serverAlertStateText(alert))
}

// serverAlertKindLabel names an alert kind.
func (state *State) serverAlertKindLabel(kind serveralerts.AlertKind) string {
	switch kind {
	case serveralerts.KindCPU:
		return "CPU"
	case serveralerts.KindMemory:
		return state.serversText("Memory", "内存")
	case serveralerts.KindDisk:
		return state.serversText("Disk", "磁盘")
	case serveralerts.KindSSH:
		return "SSH"
	case serveralerts.KindCollect:
		return state.serversText("Collection", "采集失败")
	default:
		return string(kind)
	}
}

// serverAlertValueText formats an alert's value: a percent for a resource
// kind, "—" for SSH and collection alerts, which carry no value.
func (state *State) serverAlertValueText(alert serveralerts.Alert) string {
	switch alert.Kind {
	case serveralerts.KindCPU, serveralerts.KindMemory, serveralerts.KindDisk:
		return fmt.Sprintf("%.0f%%", alert.Value)
	default:
		return "—"
	}
}

// serverAlertStateText names an alert's state; a recovered alert also
// shows when it recovered.
func (state *State) serverAlertStateText(alert serveralerts.Alert) string {
	if alert.State == serveralerts.StateRecovered {
		if alert.RecoveredAt != nil {
			return fmt.Sprintf(state.serversText("Recovered %s", "已恢复 %s"), alert.RecoveredAt.Format("2006-01-02 15:04"))
		}
		return state.serversText("Recovered", "已恢复")
	}
	return state.serversText("Active", "活跃")
}

// serverAlertTimeText formats an alert's trigger time.
func serverAlertTimeText(alert serveralerts.Alert) string {
	return alert.CreatedAt.Format("2006-01-02 15:04")
}

// clickServersHome handles a click on the overview page.
func (state *State) clickServersHome(id gfx.ID) bool {
	switch id {
	case hitServersHomeAdd:
		// Adding a server lives on the servers page: switch there and
		// open its editor.
		state.openServers()
		state.openServersEditor("")
		return true
	case hitServersHomeCollectAll:
		state.runServersCollectAll()
		return true
	case hitServersHomeBatchCmd:
		// Phase 5: the batch operations page.
		state.openServersBatch()
		return true
	case hitServersHomeTerminal:
		state.activeSection = sectionTerminal
		state.save()
		return true
	}
	return false
}

// paintServersHomeWorkspace paints the overview page's empty state into
// the slot its declaration left. The overview always has numbers to show,
// so it is normally a no-op.
func (state *State) paintServersHomeWorkspace(canvas *gfx.Canvas) {
	layout := &state.sectionBodies.serversHome
	if layout.empty {
		state.drawReservedEmptyPage(canvas, layout.emptySlot, layout.emptyIcon)
	}
}
