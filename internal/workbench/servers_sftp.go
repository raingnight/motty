package workbench

// SFTP file management page (phase 5): browse one managed server's files,
// upload, download, delete, rename and create directories, with a
// transfer list.
//
// The page is a thin UI over internal/serversftp: Open dials through
// serversDialConn (the serverconn.Dial wrapper — jump host, fingerprint
// check, vault credentials) and opens SFTP, falling back to SCP when the
// server refuses the subsystem. Browsing and transfers run on background
// goroutines and report back through serversPost; the window thread never
// waits on the network. Transfers run through the serversftp Browser's
// sftpclient.Worker, so resume, pause/cancel and bounded concurrency come
// from the existing transport, and a transfer never blocks collection or
// a terminal.
//
// Deletion is confirmed in a dialog on this page; the serversftp package
// itself performs without asking.

import (
	"context"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"strings"

	"github.com/vivaui/viva/app"
	"github.com/vivaui/viva/components"
	"github.com/vivaui/viva/gfx"
	"github.com/vivaui/viva/text"
	"github.com/vivaui/viva/ui"
	"motty/internal/serverassets"
	"motty/internal/serversftp"
)

// SFTP page hit identities, all under the servers hit feature.
var (
	hitServersSFTPOpen          = tree.ID(serversHits, "sftp-open")
	hitServersSFTPClose         = tree.ID(serversHits, "sftp-close")
	hitServersSFTPUp            = tree.ID(serversHits, "sftp-up")
	hitServersSFTPGo            = tree.ID(serversHits, "sftp-go")
	hitServersSFTPPath          = tree.ID(serversHits, "sftp-path")
	hitServersSFTPEnter         = tree.ID(serversHits, "sftp-enter")
	hitServersSFTPUpload        = tree.ID(serversHits, "sftp-upload")
	hitServersSFTPDownload      = tree.ID(serversHits, "sftp-download")
	hitServersSFTPMkdir         = tree.ID(serversHits, "sftp-mkdir")
	hitServersSFTPRename        = tree.ID(serversHits, "sftp-rename")
	hitServersSFTPDelete        = tree.ID(serversHits, "sftp-delete")
	hitServersSFTPRefresh       = tree.ID(serversHits, "sftp-refresh")
	hitServersSFTPFileList      = tree.Sub(serversHits, tree.Name("sftp-file"))
	hitServersSFTPTransferList  = tree.Sub(serversHits, tree.Name("sftp-transfer"))
	hitServersSFTPPause         = tree.ID(serversHits, "sftp-pause")
	hitServersSFTPResume        = tree.ID(serversHits, "sftp-resume")
	hitServersSFTPCancel        = tree.ID(serversHits, "sftp-cancel")
	hitServersSFTPDialogCancel  = tree.ID(serversHits, "sftp-dialog-cancel")
	hitServersSFTPDialogConfirm = tree.ID(serversHits, "sftp-dialog-confirm")
	hitServersSFTPDlgMkdirName  = tree.ID(serversHits, "sftp-dlg-mkdir-name")
	hitServersSFTPDlgRenameName = tree.ID(serversHits, "sftp-dlg-rename-name")
)

// serversSFTPSectionUI is what the SFTP page's declaration leaves for its
// paint (the empty state's icon slot, as on the servers page).
type serversSFTPSectionUI struct {
	empty     bool
	emptyIcon uint8
	emptySlot ui.Box
	draw      func(*ui.DrawContext)
}

// serversSFTPState is the SFTP page's sub-state on State.servers.
type serversSFTPState struct {
	serverID   string
	opening    bool
	loadingDir bool
	browser    *serversftp.Browser
	browserErr string

	cwd      string
	entries  []serversftp.Entry
	selected string // entry name

	pathEditor text.Editor
	pathField  ui.Field
	pathBound  bool

	// Dialogs: mkdir/rename carry an editor, delete carries a name.
	mkdirDlg    bool
	mkdirName   text.Editor
	mkdirField  ui.Field
	mkdirBound  bool
	renameDlg   bool
	renameName  text.Editor
	renameField ui.Field
	renameBound bool
	deleteName  string
	deleteIsDir bool

	// Transfers are read live from the browser each paint; only the
	// selected transfer id and the latest progress line are kept here.
	selectedTransfer uint64
	transferStatus   string

	status    string
	errorText string

	fileList        ui.ListState
	fileRowDecl     func(ui.ListRow)
	transferList    ui.ListState
	transferRowDecl func(ui.ListRow)
}

// openServersSFTP shows the SFTP page: the same load as the servers page,
// defaulting to the server selected on the servers page, and opens its
// browser when there is none yet.
func (state *State) openServersSFTP() {
	state.activeSection = sectionServersSFTP
	state.refreshServers(true)
	view := &state.servers.sftp
	if view.serverID == "" {
		if state.servers.selectedID != "" {
			view.serverID = state.servers.selectedID
		} else {
			for _, server := range state.servers.rows {
				if !server.Deleted {
					view.serverID = server.ID
					break
				}
			}
		}
	}
	if view.browser == nil && !view.opening && view.serverID != "" {
		if server, ok := state.serverByID(view.serverID); ok {
			state.openServersSFTPBrowser(server)
		}
	}
}

// openServersSFTPBrowser dials one server in the background and opens
// its file browser. A failure reports its reason and leaves the page
// usable: the user can pick another server and try again.
func (state *State) openServersSFTPBrowser(server serverassets.Server) {
	view := &state.servers.sftp
	if state.appAccessBlocked() {
		view.errorText = state.messages.AppLock.LockedNotice
		return
	}
	if view.opening {
		return
	}
	state.closeServersSFTPBrowser()
	view.opening = true
	view.browserErr = ""
	view.errorText = ""
	view.entries = nil
	view.status = fmt.Sprintf(state.serversText("Connecting to %s…", "正在连接 %s…"), server.Name)
	go func() {
		browser, err := serversftp.Open(context.Background(), server, state.serversDialConn)
		state.serversPost(func() {
			view.opening = false
			if err != nil {
				view.browserErr = err.Error()
				view.status = ""
				state.serversInvalidate()
				return
			}
			view.browser = browser
			view.cwd = browser.WorkingDir()
			view.status = fmt.Sprintf(state.serversText("Connected to %s", "已连接 %s"), server.Name)
			if browser.IsSCP() {
				view.status += state.serversText(" (SCP mode: listings are approximate)", "（SCP 模式：列表为近似值）")
			}
			setEditor(&view.pathEditor, view.cwd)
			state.loadServersSFTPDir(view.cwd)
			state.serversInvalidate()
		})
	}()
}

// closeServersSFTPBrowser closes the open browser, if any. The shared
// config store is untouched; closeServers calls this on window close.
func (state *State) closeServersSFTPBrowser() {
	view := &state.servers.sftp
	if view.browser != nil {
		_ = view.browser.Close()
		view.browser = nil
	}
	view.entries = nil
	view.selected = ""
	view.selectedTransfer = 0
	view.transferStatus = ""
}

// loadServersSFTPDir lists one remote directory on a background
// goroutine: the listing is a network round trip and must never run on
// the window thread. A result that arrives after the user switched
// servers is dropped.
func (state *State) loadServersSFTPDir(dir string) {
	view := &state.servers.sftp
	if state.appAccessBlocked() {
		view.errorText = state.messages.AppLock.LockedNotice
		return
	}
	browser := view.browser
	if browser == nil {
		return
	}
	cleaned, err := serversftp.CleanPath(dir)
	if err != nil {
		view.errorText = err.Error()
		return
	}
	if view.loadingDir {
		return
	}
	view.loadingDir = true
	go func() {
		entries, err := browser.ListDir(cleaned)
		state.serversPost(func() {
			view.loadingDir = false
			if view.browser != browser {
				return
			}
			if err != nil {
				view.errorText = err.Error()
				state.serversInvalidate()
				return
			}
			view.cwd = cleaned
			view.entries = entries
			view.selected = ""
			view.errorText = ""
			setEditor(&view.pathEditor, cleaned)
			state.serversInvalidate()
		})
	}()
}

// sftpSelectedEntry returns the selected directory entry, if any.
func (state *State) sftpSelectedEntry() (serversftp.Entry, bool) {
	view := &state.servers.sftp
	for _, entry := range view.entries {
		if entry.Name == view.selected {
			return entry, true
		}
	}
	return serversftp.Entry{}, false
}

// startServersSFTPUpload uploads one local file into the current remote
// directory, on a background goroutine. Progress posts a status line;
// completion reloads the directory.
func (state *State) startServersSFTPUpload(localPath string) {
	view := &state.servers.sftp
	if state.appAccessBlocked() {
		view.errorText = state.messages.AppLock.LockedNotice
		return
	}
	browser := view.browser
	if browser == nil {
		return
	}
	base := filepath.Base(localPath)
	remotePath, err := serversftp.JoinDir(view.cwd, base)
	if err != nil {
		view.errorText = err.Error()
		return
	}
	go func() {
		err := browser.Upload(localPath, remotePath, func(sent, total, rate int64) {
			state.serversPost(func() {
				view.transferStatus = fmt.Sprintf(state.serversText("Uploading %s: %s / %s · %s/s", "正在上传 %s：%s / %s · %s/s"),
					base, formatBytes(sent), formatBytes(total), formatBytes(rate))
				state.serversInvalidate()
			})
		})
		state.serversPost(func() {
			if err != nil {
				view.errorText = err.Error()
			} else {
				view.transferStatus = ""
				view.status = fmt.Sprintf(state.serversText("Uploaded %s", "已上传 %s"), base)
				state.loadServersSFTPDir(view.cwd)
			}
			state.serversInvalidate()
		})
	}()
}

// startServersSFTPDownload downloads one remote file to a local path, on
// a background goroutine.
func (state *State) startServersSFTPDownload(remotePath, localPath string) {
	view := &state.servers.sftp
	if state.appAccessBlocked() {
		view.errorText = state.messages.AppLock.LockedNotice
		return
	}
	browser := view.browser
	if browser == nil {
		return
	}
	base := filepath.Base(localPath)
	go func() {
		err := browser.Download(remotePath, localPath, func(sent, total, rate int64) {
			state.serversPost(func() {
				view.transferStatus = fmt.Sprintf(state.serversText("Downloading %s: %s / %s · %s/s", "正在下载 %s：%s / %s · %s/s"),
					base, formatBytes(sent), formatBytes(total), formatBytes(rate))
				state.serversInvalidate()
			})
		})
		state.serversPost(func() {
			if err != nil {
				view.errorText = err.Error()
			} else {
				view.transferStatus = ""
				view.status = fmt.Sprintf(state.serversText("Downloaded %s", "已下载 %s"), base)
			}
			state.serversInvalidate()
		})
	}()
}

// --- page declaration ---

// declareServersSFTPWorkspace declares the SFTP file management page.
func (state *State) declareServersSFTPWorkspace(c ui.C) {
	view, pal := &state.servers.sftp, state.pal
	layout := &state.sectionBodies.serversSFTP
	layout.empty, layout.emptyIcon = false, uint8(iconFolderOpen)
	if layout.draw == nil {
		layout.draw = func(d *ui.DrawContext) { state.paintServersSFTPWorkspace(d.Canvas) }
	}
	if view.fileRowDecl == nil {
		view.fileRowDecl = state.declareServersSFTPFileRow
		view.fileList.Key = func(row int) int { return row }
		view.fileList.Label = func(row int) string {
			if row >= 0 && row < len(view.entries) {
				return view.entries[row].Name
			}
			return ""
		}
	}
	if view.transferRowDecl == nil {
		view.transferRowDecl = state.declareServersSFTPTransferRow
		view.transferList.Key = func(row int) int { return row }
		view.transferList.Label = func(row int) string {
			if view.browser != nil {
				if transfers := view.browser.Transfers(); row >= 0 && row < len(transfers) {
					return transfers[row].Name
				}
			}
			return ""
		}
	}
	if !view.pathBound {
		view.pathField.Bind(&view.pathEditor)
		view.pathBound = true
	}
	body := ui.Column(c).Fill().PadSides(12, serversSidePad, 0, serversSidePad)
	body.Children(func() {
		ui.Column(c).MarginSides(4, 0, 12, 0).Gap(3).Children(func() {
			ui.Text(c, state.messages.NavServersSFTP).FontSize(18).Weight(gfx.WeightSemibold).Color(pal.text).Ellipsis()
			ui.Paragraph(c, state.serversText("Browse and manage one server's files over SFTP (SCP when the server has no SFTP subsystem).", "通过 SFTP 浏览和管理一台服务器的文件（无 SFTP 子系统时回退到 SCP）。"), 2).FontSize(metrics.FontSizeS).Color(pal.muted)
		})
		state.declareServersSFTPServerRow(c)
		state.declareServersSFTPPathRow(c)
		state.declareServersSFTPToolbar(c)
		state.declareServersSFTPFileList(c)
		state.declareServersSFTPTransfers(c)
		ui.Divider(c).Background(pal.line)
		ui.Row(c).Height(49).Gap(10).Items(ui.Center).Children(func() {
			if view.transferStatus != "" {
				ui.Text(c, view.transferStatus).FontSize(metrics.FontSizeS).Color(pal.muted).Grow(1).Ellipsis()
			} else {
				ui.Text(c, view.status).FontSize(metrics.FontSizeS).Color(pal.muted).Grow(1).Ellipsis()
			}
			if view.browserErr != "" {
				ui.Text(c, view.browserErr).FontSize(metrics.FontSizeS).Color(pal.danger).Grow(1).Ellipsis()
			}
			if view.errorText != "" {
				ui.Text(c, view.errorText).FontSize(metrics.FontSizeS).Color(pal.danger).Grow(1).Ellipsis()
			}
		})
	})
	body.Clip().DrawOver(layout.draw)
}

// declareServersSFTPServerRow declares the server picker and the
// open/close buttons.
func (state *State) declareServersSFTPServerRow(c ui.C) {
	view, pal := &state.servers.sftp, state.pal
	serverOptions := []string{}
	for _, server := range state.servers.rows {
		if !server.Deleted {
			serverOptions = append(serverOptions, server.ID)
		}
	}
	ui.Row(c).Height(40).Gap(8).Items(ui.Center).MarginSides(8, 0, 4, 0).Children(func() {
		if ui.Choice(c, &view.serverID, serverOptions, func(id string) string {
			if server, ok := state.serverByID(id); ok {
				return server.Name
			}
			return id
		}).Height(36).Changed() {
			if server, ok := state.serverByID(view.serverID); ok {
				state.openServersSFTPBrowser(server)
			}
		}
		state.declareButtonSized(c, hitServersSFTPOpen, state.serversText("Open", "打开"), iconFolderOpen, false, false, view.serverID != "" && !view.opening, toolbarFont, false).Size(96, toolbarHeight)
		state.declareButtonSized(c, hitServersSFTPClose, state.serversText("Close", "关闭"), iconTrash, false, false, view.browser != nil, toolbarFont, false).Size(96, toolbarHeight)
		if view.opening {
			ui.Text(c, state.serversText("Connecting…", "连接中…")).FontSize(metrics.FontSizeS).Color(pal.muted)
		} else if view.browser != nil && view.browser.IsSCP() {
			ui.Text(c, state.serversText("SCP mode: listings are approximate", "SCP 模式：列表为近似值")).FontSize(metrics.FontSizeS).Color(pal.muted).Ellipsis()
		}
		ui.Spacer(c)
	})
}

// declareServersSFTPPathRow declares the breadcrumb path: up one level,
// the path field and go.
func (state *State) declareServersSFTPPathRow(c ui.C) {
	view := &state.servers.sftp
	ui.Row(c).Height(40).Gap(8).Items(ui.Center).MarginSides(8, 0, 4, 0).Children(func() {
		state.declareButtonSized(c, hitServersSFTPUp, state.serversText("Up", "上级目录"), iconFolderOpen, false, false, view.browser != nil && view.cwd != "/", toolbarFont, false).Size(128, toolbarHeight)
		ui.TextField(c, &view.pathField, state.serversText("Remote path", "远程路径"), "/", ui.WithID(hitServersSFTPPath)).
			Grow(1).MinWidth(serversSearchLeast).Height(40)
		state.declareButtonSized(c, hitServersSFTPGo, state.serversText("Go", "前往"), iconCheck, false, false, view.browser != nil, toolbarFont, false).Size(96, toolbarHeight)
	})
}

// declareServersSFTPToolbar declares the file actions. Enter opens the
// selected directory; upload and download use the native file dialogs.
func (state *State) declareServersSFTPToolbar(c ui.C) {
	view := &state.servers.sftp
	selected, ok := state.sftpSelectedEntry()
	ui.Row(c).Height(40).Gap(8).Items(ui.Center).MarginSides(8, 0, 4, 0).Children(func() {
		state.declareButtonSized(c, hitServersSFTPUpload, state.serversText("Upload", "上传"), iconUpload, false, false, view.browser != nil, toolbarFont, false).Size(96, toolbarHeight)
		state.declareButtonSized(c, hitServersSFTPDownload, state.serversText("Download", "下载"), iconDownload, false, false, ok && !selected.IsDir, toolbarFont, false).Size(96, toolbarHeight)
		state.declareButtonSized(c, hitServersSFTPEnter, state.serversText("Open", "进入"), iconFolderOpen, false, false, ok && selected.IsDir, toolbarFont, false).Size(96, toolbarHeight)
		state.declareButtonSized(c, hitServersSFTPMkdir, state.serversText("New folder", "新建目录"), iconPlus, false, false, view.browser != nil, toolbarFont, false).Size(128, toolbarHeight)
		state.declareButtonSized(c, hitServersSFTPRename, state.serversText("Rename", "重命名"), iconPencil, false, false, ok, toolbarFont, false).Size(112, toolbarHeight)
		state.declareButtonSized(c, hitServersSFTPDelete, state.serversText("Delete", "删除"), iconTrash, false, false, ok, toolbarFont, false).Size(96, toolbarHeight)
		state.declareButtonSized(c, hitServersSFTPRefresh, state.serversText("Refresh", "刷新"), iconRefresh, false, false, view.browser != nil && !view.loadingDir, toolbarFont, false).Size(96, toolbarHeight)
		ui.Spacer(c)
	})
}

// declareServersSFTPFileList declares the remote directory table.
func (state *State) declareServersSFTPFileList(c ui.C) {
	view := &state.servers.sftp
	theme := state.Theme()
	headerHeight := components.TableHeaderHeight(theme)
	headerPad := theme.Metrics.GapItem
	headings := []string{
		state.serversText("Name", "文件名"),
		state.serversText("Size", "大小"),
		state.serversText("Modified", "修改时间"),
		state.serversText("Type", "类型"),
	}
	widths := []float32{0, 110, 150, 90}
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
		ui.List(c, &view.fileList, len(view.entries), view.fileRowDecl).Grow(1).Shrink(1).MinHeight(0).FillWidth().
			Scrollbars().Label(state.serversText("Remote files", "远程文件"))
	})
}

// declareServersSFTPFileRow declares one remote file row.
func (state *State) declareServersSFTPFileRow(row ui.ListRow) {
	view := &state.servers.sftp
	if row.Index < 0 || row.Index >= len(view.entries) {
		return
	}
	entry := view.entries[row.Index]
	theme := state.Theme()
	height := components.TableRowHeight(theme)
	pad := theme.Metrics.GapItem
	size := "—"
	if !entry.IsDir {
		size = formatBytes(entry.Size)
	}
	cells := []string{
		entry.Name, size,
		entry.ModTime.Format("2006-01-02 15:04"), entry.Type,
	}
	widths := []float32{0, 110, 150, 90}
	c := row.C
	element := row.Element.Height(height).Shrink(0).FillWidth().Gap(0).Items(ui.Center)
	if entry.Name == view.selected {
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
			text.Ellipsis()
		}
		ui.Slot(c).Absolute().Left(0).Right(0).Bottom(0).Height(1).Background(theme.Border)
	})
	element.Hit(hitServersSFTPFileList.At(row.Index), entry.Name)
}

// declareServersSFTPTransfers declares the transfer list and its
// pause/resume/cancel buttons.
func (state *State) declareServersSFTPTransfers(c ui.C) {
	view, pal := &state.servers.sftp, state.pal
	var transfers []serversftp.TransferInfo
	if view.browser != nil {
		transfers = view.browser.Transfers()
	}
	selected := serversftp.TransferInfo{}
	selectedOK := false
	for _, transfer := range transfers {
		if transfer.ID == view.selectedTransfer {
			selected, selectedOK = transfer, true
			break
		}
	}
	ui.Column(c).MarginSides(8, 0, 4, 0).Gap(4).Children(func() {
		ui.Row(c).Height(30).Gap(8).Items(ui.Center).Children(func() {
			ui.Text(c, fmt.Sprintf(state.serversText("Transfers (%d)", "传输任务（%d）"), len(transfers))).FontSize(metrics.FontSizeS).Weight(gfx.WeightMedium).Color(pal.text).Ellipsis()
			ui.Spacer(c)
			canPause := selectedOK && (selected.State == "传输中" || selected.State == "排队中")
			canResume := selectedOK && selected.State == "已暂停"
			canCancel := selectedOK && (selected.State == "传输中" || selected.State == "排队中" || selected.State == "已暂停")
			state.declareButtonSized(c, hitServersSFTPPause, state.serversText("Pause", "暂停"), iconPause, false, false, canPause, toolbarFont, false).Size(96, toolbarHeight)
			state.declareButtonSized(c, hitServersSFTPResume, state.serversText("Resume", "继续"), iconPlay, false, false, canResume, toolbarFont, false).Size(96, toolbarHeight)
			state.declareButtonSized(c, hitServersSFTPCancel, state.serversText("Cancel", "取消"), iconTrash, false, false, canCancel, toolbarFont, false).Size(96, toolbarHeight)
		})
		ui.Column(c).Height(132).Shrink(0).Children(func() {
			ui.List(c, &view.transferList, len(transfers), view.transferRowDecl).Grow(1).Shrink(1).MinHeight(0).FillWidth().
				Scrollbars().Label(state.serversText("Transfers", "传输任务"))
		})
	})
}

// declareServersSFTPTransferRow declares one transfer row: name,
// progress, speed and state.
func (state *State) declareServersSFTPTransferRow(row ui.ListRow) {
	view := &state.servers.sftp
	if view.browser == nil {
		return
	}
	transfers := view.browser.Transfers()
	if row.Index < 0 || row.Index >= len(transfers) {
		return
	}
	transfer := transfers[row.Index]
	theme := state.Theme()
	height := components.TableRowHeight(theme)
	pad := theme.Metrics.GapItem
	progress := "—"
	if transfer.BytesTotal > 0 {
		progress = fmt.Sprintf("%d%% (%s / %s)", transfer.BytesDone*100/transfer.BytesTotal, formatBytes(transfer.BytesDone), formatBytes(transfer.BytesTotal))
	} else if transfer.BytesDone > 0 {
		progress = formatBytes(transfer.BytesDone)
	}
	speed := "—"
	if transfer.BytesPerSecond > 0 {
		speed = formatBytes(transfer.BytesPerSecond) + "/s"
	}
	label := transfer.Name
	if transfer.Operation == "upload" {
		label = state.serversText("↑ ", "↑ ") + label
	} else {
		label = state.serversText("↓ ", "↓ ") + label
	}
	cells := []string{label, progress, speed, transfer.State}
	if transfer.Error != "" {
		cells[3] += " · " + transfer.Error
	}
	widths := []float32{0, 220, 120, 0}
	c := row.C
	element := row.Element.Height(height).Shrink(0).FillWidth().Gap(0).Items(ui.Center)
	if transfer.ID == view.selectedTransfer {
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
			text.Ellipsis()
		}
		ui.Slot(c).Absolute().Left(0).Right(0).Bottom(0).Height(1).Background(theme.Border)
	})
	element.Hit(hitServersSFTPTransferList.At(row.Index), transfer.Name+" · "+transfer.State)
}

// --- dialogs ---

// declareServersSFTPDialogs declares whichever SFTP dialog is on top:
// mkdir, rename or the delete confirmation. It is called from
// declareServersDialogs.
func (state *State) declareServersSFTPDialogs(c ui.C) {
	view := &state.servers.sftp
	switch {
	case view.mkdirDlg:
		state.declareServersSFTPMkdirDialog(c)
	case view.renameDlg:
		state.declareServersSFTPRenameDialog(c)
	case view.deleteName != "":
		state.declareServersSFTPDeleteDialog(c)
	}
}

func (state *State) declareServersSFTPMkdirDialog(c ui.C) {
	view := &state.servers.sftp
	if !view.mkdirBound {
		view.mkdirField.Bind(&view.mkdirName)
		view.mkdirBound = true
	}
	description := fmt.Sprintf(state.serversText("Create a directory under %s.", "在 %s 下创建目录。"), view.cwd)
	viewport := dialogViewport(c)
	height := dialogContentHeight(c.Canvas(), *c.Theme(), viewport, 520, description, 80)
	dialog := ui.Dialog(c, ui.DialogSpec{
		Title: state.serversText("New folder", "新建目录"), Description: description,
		Width: 520, Height: height, Layer: dialogLayer,
		Scope: dialogScope, DismissID: hitServersSFTPDialogCancel, CloseID: hitServersSFTPDialogCancel,
		InitialFocusID: hitServersSFTPDialogCancel,
	})
	dialog.Body(func() {
		ui.Column(c).Fill().Gap(6).Children(func() {
			state.serversDialogField(c, state.serversText("Name", "名称"), &view.mkdirField, &view.mkdirName, hitServersSFTPDlgMkdirName, "")
		})
	})
	dialog.Footer(func() {
		state.dialogActionFooter(c,
			components.ButtonAction{ID: hitServersSFTPDialogCancel, Label: state.messages.Cancel, Variant: components.ButtonSecondary},
			components.ButtonAction{ID: hitServersSFTPDialogConfirm, Label: state.serversText("Create", "创建")},
		)
	})
}

func (state *State) declareServersSFTPRenameDialog(c ui.C) {
	view := &state.servers.sftp
	if !view.renameBound {
		view.renameField.Bind(&view.renameName)
		view.renameBound = true
	}
	description := fmt.Sprintf(state.serversText("Rename %s.", "重命名 %s。"), view.selected)
	viewport := dialogViewport(c)
	height := dialogContentHeight(c.Canvas(), *c.Theme(), viewport, 520, description, 80)
	dialog := ui.Dialog(c, ui.DialogSpec{
		Title: state.serversText("Rename", "重命名"), Description: description,
		Width: 520, Height: height, Layer: dialogLayer,
		Scope: dialogScope, DismissID: hitServersSFTPDialogCancel, CloseID: hitServersSFTPDialogCancel,
		InitialFocusID: hitServersSFTPDialogCancel,
	})
	dialog.Body(func() {
		ui.Column(c).Fill().Gap(6).Children(func() {
			state.serversDialogField(c, state.serversText("New name", "新名称"), &view.renameField, &view.renameName, hitServersSFTPDlgRenameName, "")
		})
	})
	dialog.Footer(func() {
		state.dialogActionFooter(c,
			components.ButtonAction{ID: hitServersSFTPDialogCancel, Label: state.messages.Cancel, Variant: components.ButtonSecondary},
			components.ButtonAction{ID: hitServersSFTPDialogConfirm, Label: state.serversText("Rename", "重命名")},
		)
	})
}

func (state *State) declareServersSFTPDeleteDialog(c ui.C) {
	view := &state.servers.sftp
	kind := state.serversText("file", "文件")
	if view.deleteIsDir {
		kind = state.serversText("folder", "目录")
	}
	description := fmt.Sprintf(state.serversText("Delete this %s? %s", "删除该%s？%s"), kind, view.deleteName)
	viewport := dialogViewport(c)
	height := dialogContentHeight(c.Canvas(), *c.Theme(), viewport, 520, description, 48)
	dialog := ui.Dialog(c, ui.DialogSpec{
		Title: state.serversText("Delete", "删除"), Description: description,
		Width: 520, Height: height, Layer: dialogLayer,
		Scope: dialogScope, DismissID: hitServersSFTPDialogCancel, CloseID: hitServersSFTPDialogCancel,
		InitialFocusID: hitServersSFTPDialogCancel,
	})
	dialog.Body(func() {
		ui.Column(c).Fill().Gap(8).Children(func() {
			if view.deleteIsDir {
				ui.Text(c, state.serversText("Only an empty folder can be deleted.", "仅可删除空目录。")).FontSize(metrics.FontSizeS).Color(state.pal.muted).LineHeight(24)
			} else {
				ui.Text(c, state.serversText("The file is deleted on the server and cannot be recovered.", "文件将在服务器上删除，无法恢复。")).FontSize(metrics.FontSizeS).Color(state.pal.muted).LineHeight(24)
			}
		})
	})
	dialog.Footer(func() {
		state.dialogActionFooter(c,
			components.ButtonAction{ID: hitServersSFTPDialogCancel, Label: state.messages.Cancel, Variant: components.ButtonSecondary},
			components.ButtonAction{ID: hitServersSFTPDialogConfirm, Label: state.serversText("Delete", "删除"), Variant: components.ButtonDestructive},
		)
	})
}

// --- click dispatch ---

// clickServersSFTP handles a click on the SFTP page or one of its
// dialogs. It returns false for identities it does not own.
func (state *State) clickServersSFTP(id gfx.ID) bool {
	view := &state.servers.sftp
	// Dialog buttons win over the page behind them.
	if view.mkdirDlg || view.renameDlg || view.deleteName != "" {
		switch id {
		case hitServersSFTPDialogCancel:
			view.mkdirDlg = false
			view.renameDlg = false
			view.deleteName = ""
			return true
		case hitServersSFTPDialogConfirm:
			if view.mkdirDlg {
				name := strings.TrimSpace(view.mkdirName.String())
				view.mkdirDlg = false
				if name == "" {
					view.errorText = state.serversText("Enter a folder name.", "请输入目录名。")
					return true
				}
				state.runServersSFTPMkdir(name)
				return true
			}
			if view.renameDlg {
				name := strings.TrimSpace(view.renameName.String())
				oldName := view.selected
				view.renameDlg = false
				if name == "" {
					view.errorText = state.serversText("Enter a new name.", "请输入新名称。")
					return true
				}
				state.runServersSFTPRename(oldName, name)
				return true
			}
			if view.deleteName != "" {
				name, isDir := view.deleteName, view.deleteIsDir
				view.deleteName = ""
				state.runServersSFTPDelete(name, isDir)
				return true
			}
			return true
		}
		return false
	}
	switch id {
	case hitServersSFTPOpen:
		if server, ok := state.serverByID(view.serverID); ok {
			state.openServersSFTPBrowser(server)
		}
		return true
	case hitServersSFTPClose:
		state.closeServersSFTPBrowser()
		view.status = state.serversText("Connection closed.", "连接已关闭。")
		return true
	case hitServersSFTPUp:
		if view.browser != nil && view.cwd != "/" {
			state.loadServersSFTPDir(path.Dir(view.cwd))
		}
		return true
	case hitServersSFTPGo:
		state.loadServersSFTPDir(view.pathEditor.String())
		return true
	case hitServersSFTPEnter:
		if entry, ok := state.sftpSelectedEntry(); ok && entry.IsDir {
			joined, err := serversftp.JoinDir(view.cwd, entry.Name)
			if err != nil {
				view.errorText = err.Error()
				return true
			}
			state.loadServersSFTPDir(joined)
		}
		return true
	case hitServersSFTPUpload:
		if view.browser == nil {
			view.errorText = state.serversText("Open a server first.", "请先打开一台服务器。")
			return true
		}
		paths, err := state.options.OpenFiles(app.OpenFileOptions{
			Title:         state.serversText("Choose files to upload", "选择要上传的文件"),
			AllowMultiple: true,
		})
		if errors.Is(err, app.ErrCanceled) || len(paths) == 0 {
			return true
		}
		if err != nil {
			view.errorText = err.Error()
			return true
		}
		for _, localPath := range paths {
			state.startServersSFTPUpload(localPath)
		}
		return true
	case hitServersSFTPDownload:
		entry, ok := state.sftpSelectedEntry()
		if !ok || entry.IsDir || view.browser == nil {
			return true
		}
		remotePath, err := serversftp.JoinDir(view.cwd, entry.Name)
		if err != nil {
			view.errorText = err.Error()
			return true
		}
		localPath, err := state.options.SaveFile(app.SaveFileOptions{
			Title:         state.serversText("Save file as", "保存文件"),
			SuggestedName: entry.Name,
		})
		if errors.Is(err, app.ErrCanceled) || err == nil && localPath == "" {
			return true
		}
		if err != nil {
			view.errorText = err.Error()
			return true
		}
		state.startServersSFTPDownload(remotePath, localPath)
		return true
	case hitServersSFTPMkdir:
		if view.browser == nil {
			return true
		}
		setEditor(&view.mkdirName, "")
		view.mkdirDlg = true
		return true
	case hitServersSFTPRename:
		if view.browser == nil {
			return true
		}
		if _, ok := state.sftpSelectedEntry(); !ok {
			return true
		}
		setEditor(&view.renameName, view.selected)
		view.renameDlg = true
		return true
	case hitServersSFTPDelete:
		if entry, ok := state.sftpSelectedEntry(); ok {
			view.deleteName = entry.Name
			view.deleteIsDir = entry.IsDir
		}
		return true
	case hitServersSFTPRefresh:
		state.loadServersSFTPDir(view.cwd)
		return true
	case hitServersSFTPPause:
		if view.browser != nil && view.selectedTransfer != 0 {
			view.browser.PauseTransfer(view.selectedTransfer)
		}
		return true
	case hitServersSFTPResume:
		if view.browser != nil && view.selectedTransfer != 0 {
			view.browser.ResumeTransfer(view.selectedTransfer)
		}
		return true
	case hitServersSFTPCancel:
		if view.browser != nil && view.selectedTransfer != 0 {
			view.browser.CancelTransfer(view.selectedTransfer)
		}
		return true
	}
	if id.Scope() == hitServersSFTPFileList {
		if index := item(id); index >= 0 && index < len(view.entries) {
			name := view.entries[index].Name
			if view.selected == name {
				view.selected = ""
			} else {
				view.selected = name
			}
			return true
		}
	}
	if id.Scope() == hitServersSFTPTransferList {
		if view.browser != nil {
			transfers := view.browser.Transfers()
			if index := item(id); index >= 0 && index < len(transfers) {
				transfer := transfers[index]
				if view.selectedTransfer == transfer.ID {
					view.selectedTransfer = 0
				} else {
					view.selectedTransfer = transfer.ID
				}
				return true
			}
		}
	}
	return false
}

// runServersSFTPMkdir creates one directory under the current remote
// directory, on a background goroutine, then reloads the listing.
func (state *State) runServersSFTPMkdir(name string) {
	view := &state.servers.sftp
	if state.appAccessBlocked() {
		view.errorText = state.messages.AppLock.LockedNotice
		return
	}
	browser := view.browser
	if browser == nil {
		return
	}
	joined, err := serversftp.JoinDir(view.cwd, name)
	if err != nil {
		view.errorText = err.Error()
		return
	}
	go func() {
		err := browser.Mkdir(joined)
		state.serversPost(func() {
			if err != nil {
				view.errorText = err.Error()
			} else {
				view.status = fmt.Sprintf(state.serversText("Created folder %s", "已创建目录 %s"), name)
				state.loadServersSFTPDir(view.cwd)
			}
			state.serversInvalidate()
		})
	}()
}

// runServersSFTPRename renames one remote entry, on a background
// goroutine, then reloads the listing.
func (state *State) runServersSFTPRename(oldName, newName string) {
	view := &state.servers.sftp
	if state.appAccessBlocked() {
		view.errorText = state.messages.AppLock.LockedNotice
		return
	}
	browser := view.browser
	if browser == nil {
		return
	}
	oldPath, err := serversftp.JoinDir(view.cwd, oldName)
	if err != nil {
		view.errorText = err.Error()
		return
	}
	newPath, err := serversftp.JoinDir(view.cwd, newName)
	if err != nil {
		view.errorText = err.Error()
		return
	}
	go func() {
		err := browser.Rename(oldPath, newPath)
		state.serversPost(func() {
			if err != nil {
				view.errorText = err.Error()
			} else {
				view.status = fmt.Sprintf(state.serversText("Renamed %s to %s", "已将 %s 重命名为 %s"), oldName, newName)
				state.loadServersSFTPDir(view.cwd)
			}
			state.serversInvalidate()
		})
	}()
}

// runServersSFTPDelete deletes one remote entry (after the page's
// confirmation dialog), on a background goroutine, then reloads.
func (state *State) runServersSFTPDelete(name string, isDir bool) {
	view := &state.servers.sftp
	if state.appAccessBlocked() {
		view.errorText = state.messages.AppLock.LockedNotice
		return
	}
	browser := view.browser
	if browser == nil {
		return
	}
	joined, err := serversftp.JoinDir(view.cwd, name)
	if err != nil {
		view.errorText = err.Error()
		return
	}
	go func() {
		err := browser.Delete(joined, isDir)
		state.serversPost(func() {
			if err != nil {
				view.errorText = err.Error()
			} else {
				view.status = fmt.Sprintf(state.serversText("Deleted %s", "已删除 %s"), name)
				state.loadServersSFTPDir(view.cwd)
			}
			state.serversInvalidate()
		})
	}()
}

// paintServersSFTPWorkspace paints the SFTP page's empty state into the
// slot its declaration left. The page always has content, so it is
// normally a no-op.
func (state *State) paintServersSFTPWorkspace(canvas *gfx.Canvas) {
	layout := &state.sectionBodies.serversSFTP
	if layout.empty {
		state.drawReservedEmptyPage(canvas, layout.emptySlot, layout.emptyIcon)
	}
}
