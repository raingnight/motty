package workbench

// Alert center page (phase 4): the alert list, newest first, with read
// and kind filters, mark-all-read, and click-to-locate.
//
// The list paints only the cached alerts (see loadServerAlerts): a click
// marks the alert read in the store and opens the servers page with that
// server selected. Read state is written back to the serveralerts store,
// never kept in memory alone.

import (
	"errors"

	"github.com/vivaui/viva/components"
	"github.com/vivaui/viva/gfx"
	"github.com/vivaui/viva/ui"
	"motty/internal/serveralerts"
)

// serverAlertsSectionUI is what the alert center's declaration leaves for
// its paint (the empty state's icon slot, as on the servers page).
type serverAlertsSectionUI struct {
	empty     bool
	emptyIcon uint8
	emptySlot ui.Box
	draw      func(*ui.DrawContext)
}

// Alert row column widths; the message column takes the rest.
const (
	serverAlertNameWidth  = float32(150)
	serverAlertKindWidth  = float32(70)
	serverAlertValueWidth = float32(70)
	serverAlertTimeWidth  = float32(130)
	serverAlertStateWidth = float32(110)
	serverAlertReadWidth  = float32(50)
)

// declareServerAlertsWorkspace declares the alert center in the workbench page.
func (state *State) declareServerAlertsWorkspace(c ui.C) {
	view, pal := &state.servers, state.pal
	bounds := state.catalogSectionBounds()
	if view.alertsRowDecl == nil {
		view.alertsRowDecl = state.declareServerAlertRow
		view.alertsList.Key = func(row int) int {
			if row >= 0 && row < len(view.alertsVisible) {
				return row
			}
			return row
		}
		view.alertsList.Label = func(row int) string {
			if row >= 0 && row < len(view.alertsVisible) {
				return view.alertsVisible[row].ServerName
			}
			return ""
		}
	}
	const gap = float32(8)
	empty := len(view.alerts) == 0
	layout := &state.sectionBodies.serverAlerts
	layout.empty, layout.emptyIcon = empty, uint8(iconAlert)
	if layout.draw == nil {
		layout.draw = func(d *ui.DrawContext) { state.paintServerAlertsWorkspace(d.Canvas) }
	}
	unread := state.serverAlertsUnread()

	body := ui.Column(c).Fill().PadSides(12, serversSidePad, 0, serversSidePad)
	body.Children(func() {
		ui.Column(c).MarginSides(4, 0, 12, 0).Gap(3).Children(func() {
			ui.Text(c, state.messages.NavServerAlerts).FontSize(18).Weight(gfx.WeightSemibold).Color(pal.text).Ellipsis()
			ui.Paragraph(c, state.serversText("Resource, SSH and collection alerts, newest first. Click a row to open its server.", "资源、SSH 与采集告警，最新在前。点击一行打开对应服务器。"), 2).FontSize(metrics.FontSizeS).Color(pal.muted)
		})
		ui.Row(c).Height(40).Gap(gap).Items(ui.Center).Children(func() {
			ui.Text(c, view.alertsCountText).FontSize(metrics.FontSizeS).Weight(gfx.WeightMedium).Color(pal.faint).Ellipsis()
			ui.Spacer(c)
			state.declareButtonSized(c, hitServerAlertsMarkAllRead, state.serversText("Mark all read", "全部标为已读"), iconCheck, false, false, unread > 0, toolbarFont, false).Size(128, toolbarHeight)
		})
		ui.Row(c).Height(36).Gap(8).Items(ui.Center).MarginSides(8, 0, 4, 0).Children(func() {
			readOptions := []string{"", "unread", "read"}
			if ui.Choice(c, &view.alertsFilterRead, readOptions, func(read string) string {
				switch read {
				case "unread":
					return state.serversText("Unread", "未读")
				case "read":
					return state.serversText("Read", "已读")
				default:
					return state.serversText("All", "全部")
				}
			}).Height(30).Changed() {
				state.syncServerAlertsVisible()
			}
			kindOptions := []string{"", "cpu", "memory", "disk", "ssh", "collect"}
			if ui.Choice(c, &view.alertsFilterKind, kindOptions, func(kind string) string {
				if kind == "" {
					return state.serversText("All kinds", "全部类型")
				}
				return state.serverAlertKindLabel(serveralerts.AlertKind(kind))
			}).Height(30).Changed() {
				state.syncServerAlertsVisible()
			}
			ui.Spacer(c)
		})
		if empty {
			layout.emptySlot = state.reserveEmptyPageWithColor(c, bounds, state.serversText("No alerts", "暂无告警"), state.serversText("Resource, SSH and collection alerts will show here.", "资源超阈值、SSH 异常与采集失败会显示在这里。"), "", pal.muted, gfx.ID{}, 0, false)
		} else {
			theme := state.Theme()
			headerHeight := components.TableHeaderHeight(theme)
			headerPad := theme.Metrics.GapItem
			headings := []string{
				state.serversText("Server", "服务器"),
				state.serversText("Kind", "类型"),
				state.serversText("Message", "消息"),
				state.serversText("Value", "数值"),
				state.serversText("Time", "触发时间"),
				state.serversText("State", "状态"),
				state.serversText("Read", "已读"),
			}
			ui.Column(c).Grow(1).Shrink(1).MarginSides(14, 0, 0, 0).Children(func() {
				ui.Row(c).Height(headerHeight).Shrink(0).FillWidth().Gap(0).Items(ui.Center).Children(func() {
					state.declareServerAlertCells(c, headings, headerPad, true)
					ui.Slot(c).Absolute().Left(0).Right(0).Bottom(0).Height(1).Background(theme.Border)
				})
				ui.List(c, &view.alertsList, len(view.alertsVisible), view.alertsRowDecl).Grow(1).Shrink(1).MinHeight(0).FillWidth().
					Scrollbars().Label(state.messages.NavServerAlerts)
			})
		}
		ui.Divider(c).Background(pal.line)
		ui.Row(c).Height(49).Gap(10).Items(ui.Center).Children(func() {
			ui.Text(c, view.alertsCountText).FontSize(metrics.FontSizeS).Weight(gfx.WeightMedium).Color(pal.faint).Ellipsis()
			ui.Text(c, view.status).FontSize(metrics.FontSizeS).Color(pal.muted).Grow(1).Ellipsis()
			if view.errorText != "" {
				ui.Text(c, view.errorText).FontSize(metrics.FontSizeS).Color(pal.danger).Grow(1).Ellipsis()
			}
		})
	})
	body.Clip().DrawOver(layout.draw)
}

// declareServerAlertCells declares one alert row's cells (header or data)
// at the alert table's column widths.
func (state *State) declareServerAlertCells(c ui.C, cells []string, pad float32, header bool) {
	widths := []float32{
		serverAlertNameWidth, serverAlertKindWidth, 0,
		serverAlertValueWidth, serverAlertTimeWidth, serverAlertStateWidth, serverAlertReadWidth,
	}
	for column, cell := range cells {
		text := ui.Text(c, cell).PadX(pad)
		if header {
			text = text.Weight(gfx.WeightMedium)
		}
		if widths[column] > 0 {
			text = text.Width(widths[column]).Shrink(0)
		} else {
			text = text.Grow(1).Basis(0)
		}
		text.Ellipsis()
	}
}

// declareServerAlertRow declares one row of the alert center's table.
func (state *State) declareServerAlertRow(row ui.ListRow) {
	view := &state.servers
	if row.Index < 0 || row.Index >= len(view.alertsVisible) {
		return
	}
	alert := view.alertsVisible[row.Index]
	theme := state.Theme()
	height := components.TableRowHeight(theme)
	pad := theme.Metrics.GapItem
	name := alert.ServerName
	if name == "" {
		name = alert.ServerID
	}
	read := state.serversText("Read", "已读")
	if !alert.Read {
		read = state.serversText("Unread", "未读")
	}
	cells := []string{
		name,
		state.serverAlertKindLabel(alert.Kind),
		alert.Message,
		state.serverAlertValueText(alert),
		serverAlertTimeText(alert),
		state.serverAlertStateText(alert),
		read,
	}
	c := row.C
	element := row.Element.Height(height).Shrink(0).FillWidth().Gap(0).Items(ui.Center)
	if alert.Read {
		element = element.Hover(ui.Look{}.Background(theme.Hover)).
			Pressed(ui.Look{}.Background(theme.Muted))
	} else {
		// An unread alert keeps its row marked until it is opened.
		element = element.Background(theme.Muted)
	}
	element.Children(func() {
		state.declareServerAlertCells(c, cells, pad, false)
		ui.Slot(c).Absolute().Left(0).Right(0).Bottom(0).Height(1).Background(theme.Border)
	})
	label := name + " · " + state.serverAlertKindLabel(alert.Kind) + " · " + alert.Message
	element.Hit(hitServerAlertsRowList.At(row.Index), label)
}

// serverAlertsUnread is how many cached alerts are still unread.
func (state *State) serverAlertsUnread() int {
	unread := 0
	for _, alert := range state.servers.alerts {
		if !alert.Read {
			unread++
		}
	}
	return unread
}

// markServerAlertRead marks one alert read in the store and in the cache.
func (state *State) markServerAlertRead(id string) error {
	store := state.serversAlertsStore()
	if store == nil {
		return errors.New(state.serversText("alert store is unavailable", "告警存储不可用"))
	}
	if err := store.MarkRead(id); err != nil {
		return err
	}
	for index := range state.servers.alerts {
		if state.servers.alerts[index].ID == id {
			state.servers.alerts[index].Read = true
		}
	}
	state.syncServerAlertsVisible()
	return nil
}

// clickServerAlerts handles a click on the alert center page.
func (state *State) clickServerAlerts(id gfx.ID) bool {
	view := &state.servers
	switch id {
	case hitServerAlertsMarkAllRead:
		store := state.serversAlertsStore()
		if store == nil {
			view.status = state.serversText("Alert store is unavailable", "告警存储不可用")
			return true
		}
		if err := store.MarkAllRead(); err != nil {
			view.status = err.Error()
			return true
		}
		state.refreshServerAlerts()
		view.status = state.serversText("All alerts marked read", "全部告警已标为已读")
		return true
	}
	if id.Scope() == hitServerAlertsRowList {
		index := item(id)
		if index >= 0 && index < len(view.alertsVisible) {
			alert := view.alertsVisible[index]
			if err := state.markServerAlertRead(alert.ID); err != nil {
				view.status = err.Error()
				return true
			}
			if !state.locateServer(alert.ServerID) {
				view.status = state.serversText("The alert's server is gone", "该告警对应的服务器不存在或已删除")
				return true
			}
			return true
		}
	}
	return false
}

// paintServerAlertsWorkspace paints the alert center's empty state into
// the slot its declaration left.
func (state *State) paintServerAlertsWorkspace(canvas *gfx.Canvas) {
	layout := &state.sectionBodies.serverAlerts
	if layout.empty {
		state.drawReservedEmptyPage(canvas, layout.emptySlot, layout.emptyIcon)
	}
}
