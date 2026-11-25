package workbench

// Servers page (phase 2): the list declaration, in the known-hosts page's
// shape — header, search and actions, filter row, table, footer — so the
// page reads like the rest of the catalog sections.
//
// Columns: name, IP, OS, status (from LastCheck), CPU, memory and disk.
// The resource columns are placeholders ("—") until the collection phase
// fills them; the status column is the live one. Abnormal servers sort
// first (see sortManagedServers).

import (
	"strconv"
	"time"

	"github.com/vivaui/viva/components"
	"github.com/vivaui/viva/gfx"
	"github.com/vivaui/viva/ui"
)

const (
	serversColumns     = 8
	serversSidePad     = float32(18)
	serversSearchLeast = float32(140)
)

// serversSectionUI is what the servers page's declaration leaves for its
// paint (the empty state's icon slot).
type serversSectionUI struct {
	empty     bool
	emptyIcon uint8
	emptySlot ui.Box
	draw      func(*ui.DrawContext)
}

// declareServersWorkspace declares the servers page in the workbench page.
func (state *State) declareServersWorkspace(c ui.C) {
	state.syncServersVisible()
	view, pal := &state.servers, state.pal
	bounds := state.catalogSectionBounds()
	if view.rowDecl == nil {
		view.rowDecl = state.declareServersRow
		// The list shows one page of the filtered list: its keys and
		// labels map the page-relative row to the absolute one.
		view.list.Key = func(row int) int {
			start, _ := state.serversPageRange()
			if at := start + row; at >= 0 && at < len(view.visible) {
				return at
			}
			return row
		}
		view.list.Label = func(row int) string {
			start, _ := state.serversPageRange()
			if at := start + row; at >= 0 && at < len(view.visible) {
				return view.visible[at].Name
			}
			return ""
		}
	}
	const gap = float32(8)
	condensed := bounds.Width < 900
	newWidth, newIcon := toolbarButtonLayout(c.Canvas(), state.serversText("New", "新增"), 0, iconPlus, condensed)
	testWidth, testIcon := toolbarButtonLayout(c.Canvas(), state.serversText("Test all", "全部测试"), 0, iconRefresh, condensed)
	jumpWidth, jumpIcon := toolbarButtonLayout(c.Canvas(), state.serversText("Jump hosts", "跳板机"), 0, iconForward, condensed)
	importWidth, importIcon := toolbarButtonLayout(c.Canvas(), state.serversText("Import", "导入"), 0, iconUpload, condensed)

	empty := view.errorText != "" || len(view.rows) == 0 || len(view.visible) == 0
	emptyTitle, emptyDescription, emptyIconKind := "", "", uint8(iconServers)
	emptyDescriptionColor := pal.muted
	switch {
	case view.errorText != "":
		emptyTitle, emptyDescription, emptyIconKind = state.serversText("Servers could not be loaded", "服务器列表加载失败"), view.errorText, iconHelp
		emptyDescriptionColor = pal.danger
	case len(view.rows) == 0:
		emptyTitle, emptyDescription = state.serversText("No servers yet", "还没有服务器"), state.serversText("Add a server to manage it here, test its connection and open a terminal for it.", "新增一台服务器，在这里管理、测试连接并打开终端。")
	case len(view.visible) == 0:
		emptyTitle, emptyDescription, emptyIconKind = state.serversText("No matching servers", "没有匹配的服务器"), state.serversText("Change the filters or clear the search.", "调整筛选条件或清空搜索。"), iconSearch
	}
	layout := &state.sectionBodies.servers
	layout.empty, layout.emptyIcon = empty, emptyIconKind
	if layout.draw == nil {
		layout.draw = func(d *ui.DrawContext) { state.paintServersWorkspace(d.Canvas) }
	}

	body := ui.Column(c).Fill().PadSides(12, serversSidePad, 0, serversSidePad)
	body.Children(func() {
		ui.Column(c).MarginSides(4, 0, 12, 0).Gap(3).Children(func() {
			ui.Text(c, state.messages.NavServers).FontSize(18).Weight(gfx.WeightSemibold).Color(pal.text).Ellipsis()
			ui.Paragraph(c, state.serversText("Managed Linux servers: test connections, confirm host keys and open terminals.", "集中管理的 Linux 服务器：测试连接、确认主机指纹、打开终端。"), 2).FontSize(metrics.FontSizeS).Color(pal.muted)
		})
		ui.Row(c).Height(40).Gap(gap).Items(ui.Center).Children(func() {
			if !view.searchBound {
				view.searchField.Bind(&view.search)
				view.searchBound = true
			}
			ui.TextField(c, &view.searchField, state.messages.NavSearch, state.serversText("Search name, hostname or IP", "按名称、主机名或 IP 搜索"), ui.WithID(hitServersSearch)).
				Icon(lucideIcons[iconSearch]).Grow(1).MaxWidth(470).MinWidth(serversSearchLeast).Height(40)
			ui.Spacer(c)
			state.declareButtonSized(c, hitServersNew, state.serversText("New", "新增"), newIcon, true, false, true, toolbarFont, condensed).Size(newWidth, toolbarHeight)
			state.declareButtonSized(c, hitServersTestAll, state.serversText("Test all", "全部测试"), testIcon, false, false, len(view.visible) > 0 && !view.testingAll, toolbarFont, condensed).Size(testWidth, toolbarHeight)
			state.declareButtonSized(c, hitServersJumpHosts, state.serversText("Jump hosts", "跳板机"), jumpIcon, false, false, true, toolbarFont, condensed).Size(jumpWidth, toolbarHeight)
			state.declareButtonSized(c, hitServersImport, state.serversText("Import", "导入"), importIcon, false, false, true, toolbarFont, condensed).Size(importWidth, toolbarHeight)
		})
		state.declareServersFilters(c)
		if empty {
			layout.emptySlot = state.reserveEmptyPageWithColor(c, bounds, emptyTitle, emptyDescription, "", emptyDescriptionColor, gfx.ID{}, 0, false)
		} else {
			columns := [serversColumns]components.TableColumn{
				{Width: 200}, {Width: 150}, {Width: 120}, {Width: 80},
				{Width: 70}, {Width: 70}, {Width: 70}, {Width: 0},
			}
			headings := [serversColumns]string{
				state.serversText("Name", "名称"),
				state.serversText("IP", "IP"),
				state.serversText("OS", "操作系统"),
				state.serversText("Status", "状态"),
				"CPU", state.serversText("Memory", "内存"),
				state.serversText("Disk", "磁盘"),
				state.serversText("Flags", "异常标识"),
			}
			components.FitTableHeadings(c.Canvas(), state.Theme(), columns[:], headings[:])
			state.fitServersColumns(bounds.Width-2*serversSidePad, columns[:])
			theme := state.Theme()
			headerHeight := components.TableHeaderHeight(theme)
			headerPad := theme.Metrics.GapItem
			ui.Column(c).Grow(1).Shrink(1).MarginSides(14, 0, 0, 0).Children(func() {
				ui.Row(c).Height(headerHeight).Shrink(0).FillWidth().Gap(0).Items(ui.Center).Children(func() {
					for column := range serversColumns {
						width := columns[column].Width
						if width <= 0 {
							break
						}
						ui.Text(c, headings[column]).Weight(gfx.WeightMedium).PadX(headerPad).Width(width).Shrink(0).Ellipsis()
					}
					ui.Slot(c).Absolute().Left(0).Right(0).Bottom(0).Height(1).Background(theme.Border)
				})
				pageStart, pageEnd := state.serversPageRange()
				ui.List(c, &view.list, pageEnd-pageStart, view.rowDecl).Grow(1).Shrink(1).MinHeight(0).FillWidth().
					Scrollbars().Label(state.messages.NavServers)
			})
			// The selected row's actions sit under the table: test, terminal,
			// edit, delete or restore.
			state.declareServersRowActions(c)
		}
		ui.Divider(c).Background(pal.line)
		ui.Row(c).Height(49).Gap(10).Items(ui.Center).Children(func() {
			ui.Text(c, view.countText).FontSize(metrics.FontSizeS).Weight(gfx.WeightMedium).Color(pal.faint).Ellipsis()
			ui.Text(c, view.status).FontSize(metrics.FontSizeS).Color(pal.muted).Grow(1).Ellipsis()
			if view.errorText != "" {
				ui.Text(c, view.errorText).FontSize(metrics.FontSizeS).Color(pal.danger).Grow(1).Ellipsis()
			}
			if len(view.rows) > 0 {
				ui.Row(c).Height(30).Gap(6).Items(ui.Center).Shrink(0).Children(func() {
					ui.Text(c, state.serversText("Per page", "每页")).FontSize(metrics.FontSizeS).Color(pal.muted).Shrink(0)
					view.pageSizeChoice = state.serversPageSizeChoice()
					if ui.Choice(c, &view.pageSizeChoice, serversPageSizes[:], func(size int) string {
						if size == 0 {
							return state.serversText("All", "全部")
						}
						return strconv.Itoa(size)
					}).Height(30).Shrink(0).Changed() {
						state.setServersPageSize(view.pageSizeChoice)
					}
				})
			}
			if pageSize := state.serversPageSize(); len(view.visible) > pageSize {
				pages := state.serversPages()
				ui.Text(c, view.pageText).FontSize(metrics.FontSizeS).Color(pal.muted).Shrink(0)
				view.paginationItems = [3]ui.PaginationItem{
					{ID: hitServersPrevPage, Label: state.serversText("Previous", "上一页"), Kind: ui.PaginationPrevious, Disabled: view.page == 0},
					{Label: view.pageNumberText, Kind: ui.PaginationPage, Current: true},
					{ID: hitServersNextPage, Label: state.serversText("Next", "下一页"), Kind: ui.PaginationNext, Disabled: view.page+1 >= pages},
				}
				ui.Pagination(c, view.paginationItems[:]).Height(30).Shrink(0)
			}
		})
	})
	body.Clip().DrawOver(layout.draw)
}

// declareServersFilters declares the group / tag / OS / status filter row
// and the show-deleted switch. The choices write straight into the filter;
// a change re-syncs the visible list on the next paint.
func (state *State) declareServersFilters(c ui.C) {
	view := &state.servers
	groupOptions := []string{""}
	for _, group := range view.groups {
		groupOptions = append(groupOptions, group.ID)
	}
	tagOptions := []string{""}
	seenTags := map[string]bool{}
	for _, server := range view.rows {
		for _, tag := range server.Tags {
			if tag != "" && !seenTags[tag] {
				seenTags[tag] = true
				tagOptions = append(tagOptions, tag)
			}
		}
	}
	osOptions := []string{""}
	seenOS := map[string]bool{}
	for _, server := range view.rows {
		if server.OS != "" && !seenOS[server.OS] {
			seenOS[server.OS] = true
			osOptions = append(osOptions, server.OS)
		}
	}
	statusOptions := []string{"", "failed", "unknown", "ok"}
	healthOptions := []string{"", "offline", "auth", "collect", "alert", "ok"}
	alertOptions := []string{"", "has", "none"}
	ui.Row(c).Height(36).Gap(8).Items(ui.Center).MarginSides(8, 0, 4, 0).Children(func() {
		if ui.Choice(c, &view.filter.GroupID, groupOptions, func(id string) string {
			if id == "" {
				return state.serversText("All groups", "全部分组")
			}
			return state.serversGroupName(id)
		}).Height(30).Changed() {
			state.syncServersVisible()
		}
		if ui.Choice(c, &view.filter.Tag, tagOptions, func(tag string) string {
			if tag == "" {
				return state.serversText("All tags", "全部标签")
			}
			return tag
		}).Height(30).Changed() {
			state.syncServersVisible()
		}
		if ui.Choice(c, &view.filter.OS, osOptions, func(os string) string {
			if os == "" {
				return state.serversText("All systems", "全部系统")
			}
			return os
		}).Height(30).Changed() {
			state.syncServersVisible()
		}
		if ui.Choice(c, &view.filter.Status, statusOptions, func(status string) string {
			switch status {
			case "failed":
				return state.serversText("Offline", "离线")
			case "unknown":
				return state.serversText("Unknown", "未知")
			case "ok":
				return state.serversText("Online", "在线")
			default:
				return state.serversText("Any status", "全部状态")
			}
		}).Height(30).Changed() {
			state.syncServersVisible()
		}
		if ui.Choice(c, &view.healthFilter, healthOptions, func(health string) string {
			switch health {
			case "offline":
				return state.serversText("Offline", "离线")
			case "auth":
				return state.serversText("SSH error", "SSH 异常")
			case "collect":
				return state.serversText("Collection failed", "采集失败")
			case "alert":
				return state.serversText("Resource alert", "资源告警")
			case "ok":
				return state.serversText("Normal", "正常")
			default:
				return state.serversText("Any health", "全部健康状态")
			}
		}).Height(30).Changed() {
			state.syncServersVisible()
		}
		if ui.Choice(c, &view.alertFilter, alertOptions, func(alert string) string {
			switch alert {
			case "has":
				return state.serversText("Has alerts", "有告警")
			case "none":
				return state.serversText("No alerts", "无告警")
			default:
				return state.serversText("Any alerts", "全部告警")
			}
		}).Height(30).Changed() {
			state.syncServersVisible()
		}
		ui.Spacer(c)
		// The checkbox writes straight into the filter; the next paint's
		// sync picks the change up, as with the choices above.
		ui.Checkbox(c, &view.filter.IncludeDeleted, state.serversText("Show deleted", "显示已删除")).Hit(hitServersShowDeleted, state.serversText("Show deleted", "显示已删除"))
	})
}

// declareServersRowActions declares the selected server's actions.
func (state *State) declareServersRowActions(c ui.C) {
	view := &state.servers
	selected, ok := state.serverByID(view.selectedID)
	testing := ok && view.testing[selected.ID]
	ui.Row(c).Height(40).Gap(8).Items(ui.Center).MarginSides(8, 0, 4, 0).Children(func() {
		state.declareButtonSized(c, hitServersTest, state.serversText("Test", "测试"), iconRefresh, false, false, ok && !testing, toolbarFont, false).Size(96, toolbarHeight)
		state.declareButtonSized(c, hitServersOpenTerminal, state.serversText("Terminal", "打开终端"), iconTerminal, false, false, ok, toolbarFont, false).Size(128, toolbarHeight)
		state.declareButtonSized(c, hitServersEdit, state.serversText("Edit", "编辑"), iconPencil, false, false, ok, toolbarFont, false).Size(96, toolbarHeight)
		if ok && selected.Deleted {
			state.declareButtonSized(c, hitServersRestore, state.serversText("Restore", "恢复"), iconRefresh, false, false, true, toolbarFont, false).Size(96, toolbarHeight)
		} else {
			state.declareButtonSized(c, hitServersDelete, state.serversText("Delete", "删除"), iconTrash, false, false, ok, toolbarFont, false).Size(96, toolbarHeight)
		}
		if testing {
			ui.Text(c, state.serversText("Testing…", "测试中…")).FontSize(metrics.FontSizeS).Color(state.pal.muted)
		} else if ok {
			if result, seen := view.results[selected.ID]; seen && result.Error != nil {
				ui.Text(c, result.Error.Error()).FontSize(metrics.FontSizeS).Color(state.pal.danger).Ellipsis()
			}
		}
		ui.Spacer(c)
	})
}

// declareServersRow declares one row of the servers table. The row index
// is page-relative: the list shows the current page's slice of the
// filtered list. The resource cells show the collection engine's latest
// values (CPU/memory/disk percent); a server never collected successfully
// still shows "—", and an expired value is flagged in the flags column
// instead of being shown as current. The flags column lists every
// abnormal identifier the server carries, joined with " · ".
func (state *State) declareServersRow(row ui.ListRow) {
	view := &state.servers
	start, end := state.serversPageRange()
	at := start + row.Index
	if at < start || at >= end || at >= len(view.visible) {
		return
	}
	server := view.visible[at]
	theme := state.Theme()
	height := components.TableRowHeight(theme)
	pad := theme.Metrics.GapItem
	columns := state.serversColumns()
	address := server.Address
	if address == "" {
		address = server.Hostname
	}
	fingerprintChanged := state.servers.pendingFP != nil && state.servers.pendingFP.serverID == server.ID
	if result, seen := view.results[server.ID]; seen && result.FingerprintChanged {
		fingerprintChanged = true
	}
	latest, _ := state.serversLatest(server.ID)
	cpuCell, memCell, diskCell, expired := serversResourceCells(latest, time.Now())
	flags := state.serverFlagsText(server, state.serverHealth(server.ID), expired, fingerprintChanged)
	cells := [serversColumns]string{
		server.Name, address, server.OS, state.serversStatusLabel(server),
		cpuCell, memCell, diskCell, flags,
	}
	c := row.C
	element := row.Element.Height(height).Shrink(0).FillWidth().Gap(0).Items(ui.Center)
	if server.ID == view.selectedID {
		element = element.Background(theme.Muted)
	} else {
		element = element.
			Hover(ui.Look{}.Background(theme.Hover)).
			Pressed(ui.Look{}.Background(theme.Muted))
	}
	element.Children(func() {
		for column := range serversColumns {
			width := columns[column].Width
			if width <= 0 {
				break
			}
			cell := ui.Text(c, cells[column]).PadX(pad).Width(width).Shrink(0)
			if column == 3 && serverStatusOf(server) == "failed" {
				cell = cell.Color(state.pal.danger)
			}
			cell.Ellipsis()
		}
		ui.Slot(c).Absolute().Left(0).Right(0).Bottom(0).Height(1).Background(theme.Border)
	})
	element.Hit(hitServersRowList.At(row.Index), server.Name+" · "+address)
}

// serversColumns returns the table's current column widths, fitted to the
// page on the last declaration.
func (state *State) serversColumns() [serversColumns]components.TableColumn {
	columns := [serversColumns]components.TableColumn{
		{Width: 200}, {Width: 150}, {Width: 120}, {Width: 80},
		{Width: 70}, {Width: 70}, {Width: 70}, {Width: 0},
	}
	width := state.catalogSectionBounds().Width - 2*serversSidePad
	state.fitServersColumns(width, columns[:])
	return columns
}

func (state *State) fitServersColumns(width float32, columns []components.TableColumn) {
	remaining := max(width, 0)
	for index := range columns {
		columnWidth := columns[index].Width
		if columnWidth <= 0 || index == len(columns)-1 && columnWidth > remaining {
			columnWidth = remaining
		}
		columnWidth = min(columnWidth, remaining)
		columns[index].Width = columnWidth
		remaining -= columnWidth
	}
}

// paintServersWorkspace paints the servers page's empty state into the slot
// its declaration left.
func (state *State) paintServersWorkspace(canvas *gfx.Canvas) {
	layout := &state.sectionBodies.servers
	if layout.empty {
		state.drawReservedEmptyPage(canvas, layout.emptySlot, layout.emptyIcon)
	}
}
