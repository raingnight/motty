package workbench

// Servers page (phase 2): dialogs.
//
// The editor dialog carries every server field the task lists: name,
// hostname, IP, port, user, auth method (password or key), jump host and
// group dropdowns, tags, notes, sudo mode, and the stored fingerprint
// read-only. A password typed here is saved to the vault under
// "server-credential:<asset UUID>" on save and never written to the
// config store; a key passphrase goes to "server-passphrase:<asset UUID>".
// The fingerprint is shown, never edited: a new or changed fingerprint
// reaches the store only through the confirm dialog in servers_state.go.

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/vivaui/viva/components"
	"github.com/vivaui/viva/gfx"
	"github.com/vivaui/viva/text"
	"github.com/vivaui/viva/ui"
	"motty/internal/serverassets"
)

// Dialog field identities (declared here; serversEditorFor in
// servers_state.go routes typing to them).
var (
	hitServersDlgName     = tree.ID(serversHits, "dialog-name")
	hitServersDlgHostname = tree.ID(serversHits, "dialog-hostname")
	hitServersDlgAddress  = tree.ID(serversHits, "dialog-address")
	hitServersDlgPort     = tree.ID(serversHits, "dialog-port")
	hitServersDlgUser     = tree.ID(serversHits, "dialog-user")
	hitServersDlgIdentity = tree.ID(serversHits, "dialog-identity")
	hitServersDlgPassword = tree.ID(serversHits, "dialog-password")
	hitServersDlgTags     = tree.ID(serversHits, "dialog-tags")
	hitServersDlgNotes    = tree.ID(serversHits, "dialog-notes")
	hitServersJumpName    = tree.ID(serversHits, "jump-name")
	hitServersJumpAddress = tree.ID(serversHits, "jump-address")
	hitServersJumpPort    = tree.ID(serversHits, "jump-port")
	hitServersJumpUser    = tree.ID(serversHits, "jump-user")
	hitServersImportPath  = tree.ID(serversHits, "import-path")
)

// serversEditorDialog is the add/edit server dialog's draft.
type serversEditorDialog struct {
	open         bool
	editingID    string
	name         text.Editor
	hostname     text.Editor
	address      text.Editor
	port         text.Editor
	user         text.Editor
	identityFile text.Editor
	password     text.Editor
	tags         text.Editor
	notes        text.Editor
	nameField, hostnameField, addressField, portField, userField,
	identityField, passwordField, tagsField, notesField ui.Field
	bound bool
	// authMethod is "password" or "key"; sudoMode is "none" or "nopasswd".
	authMethod, groupID, jumpHostID, sudoMode string
	err                                       string
}

// serversJumpDialog edits one jump host at a time; the choice above the
// fields picks which one, or a blank one for a new entry.
type serversJumpDialog struct {
	open                                          bool
	selectedID                                    string
	loadedID                                      string // the jump the fields currently show; "" is a new draft
	name                                          text.Editor
	address                                       text.Editor
	port                                          text.Editor
	user                                          text.Editor
	nameField, addressField, portField, userField ui.Field
	bound                                         bool
	authMethod                                    string
	err                                           string
}

// serversImportDialog holds the import file path and its preview.
type serversImportDialog struct {
	open       bool
	path       text.Editor
	pathField  ui.Field
	bound      bool
	preview    *serverassets.ImportPreview
	previewErr string
	summary    string
}

func setEditor(editor *text.Editor, value string) {
	*editor = text.Editor{}
	editor.Insert(value)
}

// openServersEditor opens the editor for a new server (id "") or an
// existing one.
func (state *State) openServersEditor(id string) {
	dlg := &state.servers.editor
	dlg.open = true
	dlg.editingID = id
	dlg.err = ""
	dlg.authMethod, dlg.sudoMode = "password", "none"
	dlg.groupID, dlg.jumpHostID = "", ""
	setEditor(&dlg.name, "")
	setEditor(&dlg.hostname, "")
	setEditor(&dlg.address, "")
	setEditor(&dlg.port, "22")
	setEditor(&dlg.user, "")
	setEditor(&dlg.identityFile, "")
	setEditor(&dlg.password, "")
	setEditor(&dlg.tags, "")
	setEditor(&dlg.notes, "")
	if server, ok := state.serverByID(id); ok && id != "" {
		setEditor(&dlg.name, server.Name)
		setEditor(&dlg.hostname, server.Hostname)
		setEditor(&dlg.address, server.Address)
		setEditor(&dlg.port, strconv.Itoa(serverPortOrDefault(server.Port)))
		setEditor(&dlg.user, server.User)
		setEditor(&dlg.identityFile, server.IdentityFile)
		setEditor(&dlg.tags, strings.Join(server.Tags, ", "))
		setEditor(&dlg.notes, server.Notes)
		dlg.authMethod = server.AuthMethod
		if dlg.authMethod == "" {
			dlg.authMethod = "password"
		}
		dlg.groupID, dlg.jumpHostID = server.GroupID, server.JumpHostID
		dlg.sudoMode = server.SudoMode
		if dlg.sudoMode == "" {
			dlg.sudoMode = "none"
		}
	}
}

// saveServersEditor validates the draft and writes it through the store.
// A typed password is saved to the vault, never to the store row.
func (state *State) saveServersEditor() {
	dlg := &state.servers.editor
	name := strings.TrimSpace(dlg.name.String())
	address := strings.TrimSpace(dlg.address.String())
	if name == "" || address == "" {
		dlg.err = state.serversText("Name and IP are required.", "名称和 IP 必填。")
		return
	}
	port, err := strconv.Atoi(strings.TrimSpace(dlg.port.String()))
	if err != nil || port <= 0 || port > 65535 {
		dlg.err = state.serversText("Port must be 1–65535.", "端口须为 1–65535。")
		return
	}
	server := serverassets.Server{
		Name: name, Hostname: strings.TrimSpace(dlg.hostname.String()),
		Address: address, Port: port, User: strings.TrimSpace(dlg.user.String()),
		AuthMethod: dlg.authMethod, IdentityFile: strings.TrimSpace(dlg.identityFile.String()),
		GroupID: dlg.groupID, JumpHostID: dlg.jumpHostID,
		Notes: strings.TrimSpace(dlg.notes.String()), SudoMode: dlg.sudoMode,
	}
	for _, tag := range strings.Split(dlg.tags.String(), ",") {
		if tag = strings.TrimSpace(tag); tag != "" {
			server.Tags = append(server.Tags, tag)
		}
	}
	if existing, ok := state.serverByID(dlg.editingID); ok && dlg.editingID != "" {
		server.ID = existing.ID
		server.CredentialRef = existing.CredentialRef
		server.HostKeyFingerprint = existing.HostKeyFingerprint
		server.LastCheck = existing.LastCheck
		server.Deleted = existing.Deleted
		server.OS = existing.OS
	}
	store := state.serversStore()
	if store == nil {
		dlg.err = state.servers.storeErr
		return
	}
	var saved serverassets.Server
	if server.ID == "" {
		saved, err = store.CreateServer(server)
	} else {
		saved, err = store.UpdateServer(server)
	}
	if err != nil {
		dlg.err = err.Error()
		return
	}
	if dlg.authMethod == "password" {
		if password := dlg.password.String(); password != "" {
			if err := state.saveServerSecret("credential", saved.ID, password); err != nil {
				dlg.err = state.serversText("Server saved, but the password could not be stored in the vault.", "服务器已保存，但密码未能存入 vault。")
				return
			}
		}
	} else if password := dlg.password.String(); password != "" {
		// A key passphrase typed alongside key auth.
		if err := state.saveServerSecret("passphrase", saved.ID, password); err != nil {
			dlg.err = state.serversText("Server saved, but the key passphrase could not be stored in the vault.", "服务器已保存，但密钥口令未能存入 vault。")
			return
		}
	}
	dlg.open = false
	state.servers.selectedID = saved.ID
	state.servers.status = fmt.Sprintf(state.serversText("Saved %s", "已保存 %s"), saved.Name)
	state.refreshServers(false)
}

// openServersJumpDialog opens the jump-host manager.
func (state *State) openServersJumpDialog() {
	dlg := &state.servers.jumpsDlg
	dlg.open = true
	dlg.err = ""
	dlg.selectedID = ""
	dlg.authMethod = "password"
	setEditor(&dlg.name, "")
	setEditor(&dlg.address, "")
	setEditor(&dlg.port, "22")
	setEditor(&dlg.user, "")
}

// loadServersJump fills the jump dialog from the selected jump host; "" clears
// the draft for a new one.
func (state *State) loadServersJump(id string) {
	dlg := &state.servers.jumpsDlg
	dlg.selectedID = id
	dlg.loadedID = id
	dlg.err = ""
	setEditor(&dlg.name, "")
	setEditor(&dlg.address, "")
	setEditor(&dlg.port, "22")
	setEditor(&dlg.user, "")
	dlg.authMethod = "password"
	for _, jump := range state.servers.jumps {
		if jump.ID == id {
			setEditor(&dlg.name, jump.Name)
			setEditor(&dlg.address, jump.Address)
			setEditor(&dlg.port, strconv.Itoa(serverPortOrDefault(jump.Port)))
			setEditor(&dlg.user, jump.User)
			dlg.authMethod = jump.AuthMethod
			if dlg.authMethod == "" {
				dlg.authMethod = "password"
			}
			return
		}
	}
}

// saveServersJump writes the jump dialog's draft.
func (state *State) saveServersJump() {
	dlg := &state.servers.jumpsDlg
	name := strings.TrimSpace(dlg.name.String())
	address := strings.TrimSpace(dlg.address.String())
	if name == "" || address == "" {
		dlg.err = state.serversText("Name and address are required.", "名称和地址必填。")
		return
	}
	port, err := strconv.Atoi(strings.TrimSpace(dlg.port.String()))
	if err != nil || port <= 0 || port > 65535 {
		dlg.err = state.serversText("Port must be 1–65535.", "端口须为 1–65535。")
		return
	}
	jump := serverassets.JumpHost{
		ID: dlg.selectedID, Name: name, Address: address, Port: port,
		User: strings.TrimSpace(dlg.user.String()), AuthMethod: dlg.authMethod,
	}
	store := state.serversStore()
	if store == nil {
		dlg.err = state.servers.storeErr
		return
	}
	var saved serverassets.JumpHost
	if jump.ID == "" {
		saved, err = store.CreateJumpHost(jump)
	} else {
		saved, err = store.UpdateJumpHost(jump)
	}
	if err != nil {
		dlg.err = err.Error()
		return
	}
	dlg.selectedID = saved.ID
	state.refreshServers(false)
}

// deleteServersJump removes the selected jump host.
func (state *State) deleteServersJump() {
	dlg := &state.servers.jumpsDlg
	if dlg.selectedID == "" {
		return
	}
	store := state.serversStore()
	if store == nil {
		dlg.err = state.servers.storeErr
		return
	}
	if err := store.DeleteJumpHost(dlg.selectedID); err != nil {
		dlg.err = err.Error()
		return
	}
	state.openServersJumpDialog()
	state.refreshServers(false)
}

// declareServersDialogs declares whichever servers dialog is on top. Only
// one is declared per paint: fingerprint confirmation wins over delete,
// which wins over the editor, jump manager and import. The batch and SFTP
// pages' dialogs are checked first: their states are only set while their
// page is open, so they never shadow a servers-page dialog.
func (state *State) declareServersDialogs(c ui.C) {
	view := &state.servers
	if view.batch.confirm.stage != 0 || view.batch.templateDlg.open || view.batch.templateDeleteID != "" {
		state.declareServersBatchDialogs(c)
		return
	}
	if view.sftp.mkdirDlg || view.sftp.renameDlg || view.sftp.deleteName != "" {
		state.declareServersSFTPDialogs(c)
		return
	}
	switch {
	case view.pendingFP != nil:
		state.declareServersFingerprintDialog(c)
	case view.deleteID != "":
		state.declareServersDeleteDialog(c)
	case view.editor.open:
		state.declareServersEditorDialog(c)
	case view.jumpsDlg.open:
		state.declareServersJumpDialog(c)
	case view.importDlg.open:
		state.declareServersImportDialog(c)
	}
}

func (state *State) serversDialogField(c ui.C, label string, field *ui.Field, editor *text.Editor, id gfx.ID, placeholder string) {
	ui.Text(c, label).FontSize(metrics.FontSizeS).Weight(gfx.WeightMedium).Color(state.pal.muted)
	ui.TextField(c, field, label, placeholder, ui.WithID(id)).Height(36)
}

func (state *State) declareServersEditorDialog(c ui.C) {
	dlg := &state.servers.editor
	if !dlg.bound {
		dlg.nameField.Bind(&dlg.name)
		dlg.hostnameField.Bind(&dlg.hostname)
		dlg.addressField.Bind(&dlg.address)
		dlg.portField.Bind(&dlg.port)
		dlg.userField.Bind(&dlg.user)
		dlg.identityField.Bind(&dlg.identityFile)
		dlg.passwordField.Bind(&dlg.password)
		dlg.tagsField.Bind(&dlg.tags)
		dlg.notesField.Bind(&dlg.notes)
		dlg.bound = true
	}
	title := state.serversText("New server", "新增服务器")
	if dlg.editingID != "" {
		title = state.serversText("Edit server", "编辑服务器")
	}
	description := state.serversText("Connection settings for one managed server.", "一台受管服务器的连接信息。")
	if server, ok := state.serverByID(dlg.editingID); ok && dlg.editingID != "" && server.HostKeyFingerprint != "" {
		description = fmt.Sprintf(state.serversText("Stored host key fingerprint: %s", "已保存的主机指纹：%s"), server.HostKeyFingerprint)
	}
	viewport := dialogViewport(c)
	height := dialogContentHeight(c.Canvas(), *c.Theme(), viewport, 620, description, 560)
	dialog := ui.Dialog(c, ui.DialogSpec{
		Title: title, Description: description,
		Width: 620, Height: height, Layer: dialogLayer,
		Scope: dialogScope, DismissID: hitServersDialogDismiss, CloseID: hitServersDialogClose,
		InitialFocusID: hitServersDialogCancel,
	})
	dialog.Body(func() {
		ui.Column(c).Fill().Gap(6).Children(func() {
			state.serversDialogField(c, state.serversText("Name", "名称"), &dlg.nameField, &dlg.name, hitServersDlgName, "")
			state.serversDialogField(c, state.serversText("Hostname", "主机名"), &dlg.hostnameField, &dlg.hostname, hitServersDlgHostname, "")
			state.serversDialogField(c, state.serversText("IP address", "IP 地址"), &dlg.addressField, &dlg.address, hitServersDlgAddress, "")
			ui.Row(c).Gap(8).Children(func() {
				ui.Column(c).Grow(1).Children(func() {
					state.serversDialogField(c, state.serversText("Port", "端口"), &dlg.portField, &dlg.port, hitServersDlgPort, "22")
				})
				ui.Column(c).Grow(1).Children(func() {
					state.serversDialogField(c, state.serversText("Login user", "登录用户"), &dlg.userField, &dlg.user, hitServersDlgUser, "")
				})
			})
			ui.Text(c, state.serversText("Authentication", "认证方式")).FontSize(metrics.FontSizeS).Weight(gfx.WeightMedium).Color(state.pal.muted)
			ui.Choice(c, &dlg.authMethod, []string{"password", "key"}, func(method string) string {
				if method == "key" {
					return state.serversText("Key", "密钥")
				}
				return state.serversText("Password", "密码")
			}).Height(36)
			if dlg.authMethod == "key" {
				state.serversDialogField(c, state.serversText("Identity file", "密钥文件"), &dlg.identityField, &dlg.identityFile, hitServersDlgIdentity, "~/.ssh/id_ed25519")
				state.serversDialogField(c, state.serversText("Key passphrase (saved to vault)", "密钥口令（存入 vault）"), &dlg.passwordField, &dlg.password, hitServersDlgPassword, "")
			} else {
				state.serversDialogField(c, state.serversText("Password (saved to vault)", "密码（存入 vault）"), &dlg.passwordField, &dlg.password, hitServersDlgPassword, "")
			}
			ui.Text(c, state.serversText("Jump host", "跳板机")).FontSize(metrics.FontSizeS).Weight(gfx.WeightMedium).Color(state.pal.muted)
			jumpOptions := []string{""}
			for _, jump := range state.servers.jumps {
				jumpOptions = append(jumpOptions, jump.ID)
			}
			ui.Choice(c, &dlg.jumpHostID, jumpOptions, func(id string) string {
				if id == "" {
					return state.serversText("Direct connection", "直连")
				}
				for _, jump := range state.servers.jumps {
					if jump.ID == id {
						return jump.Name
					}
				}
				return id
			}).Height(36)
			ui.Text(c, state.serversText("Group", "分组")).FontSize(metrics.FontSizeS).Weight(gfx.WeightMedium).Color(state.pal.muted)
			groupOptions := []string{""}
			for _, group := range state.servers.groups {
				groupOptions = append(groupOptions, group.ID)
			}
			ui.Choice(c, &dlg.groupID, groupOptions, func(id string) string {
				if id == "" {
					return state.serversText("Ungrouped", "未分组")
				}
				return state.serversGroupName(id)
			}).Height(36)
			state.serversDialogField(c, state.serversText("Tags (comma separated)", "标签（逗号分隔）"), &dlg.tagsField, &dlg.tags, hitServersDlgTags, "")
			state.serversDialogField(c, state.serversText("Notes", "备注"), &dlg.notesField, &dlg.notes, hitServersDlgNotes, "")
			ui.Text(c, state.serversText("Sudo mode (reserved)", "Sudo 方式（预留）")).FontSize(metrics.FontSizeS).Weight(gfx.WeightMedium).Color(state.pal.muted)
			ui.Choice(c, &dlg.sudoMode, []string{"none", "nopasswd"}, func(mode string) string {
				if mode == "nopasswd" {
					return "nopasswd"
				}
				return "none"
			}).Height(36)
			if dlg.err != "" {
				ui.Text(c, dlg.err).FontSize(metrics.FontSizeS).Weight(gfx.WeightMedium).Color(state.pal.danger).LineHeight(24)
			}
		})
	})
	dialog.Footer(func() {
		state.dialogActionFooter(c,
			components.ButtonAction{ID: hitServersDialogCancel, Label: state.messages.Cancel, Variant: components.ButtonSecondary},
			components.ButtonAction{ID: hitServersDialogConfirm, Label: state.serversText("Save", "保存")},
		)
	})
}

func (state *State) declareServersJumpDialog(c ui.C) {
	dlg := &state.servers.jumpsDlg
	if !dlg.bound {
		dlg.nameField.Bind(&dlg.name)
		dlg.addressField.Bind(&dlg.address)
		dlg.portField.Bind(&dlg.port)
		dlg.userField.Bind(&dlg.user)
		dlg.bound = true
	}
	viewport := dialogViewport(c)
	height := dialogContentHeight(c.Canvas(), *c.Theme(), viewport, 560, state.serversText("Jump hosts carry connections to servers that are not directly reachable.", "跳板机用于连接无法直达的服务器。"), 320)
	dialog := ui.Dialog(c, ui.DialogSpec{
		Title: state.serversText("Jump hosts", "跳板机管理"), Description: state.serversText("Jump hosts carry connections to servers that are not directly reachable.", "跳板机用于连接无法直达的服务器。"),
		Width: 560, Height: height, Layer: dialogLayer,
		Scope: dialogScope, DismissID: hitServersDialogDismiss, CloseID: hitServersDialogClose,
		InitialFocusID: hitServersDialogCancel,
	})
	dialog.Body(func() {
		ui.Column(c).Fill().Gap(6).Children(func() {
			jumpOptions := []string{""}
			for _, jump := range state.servers.jumps {
				jumpOptions = append(jumpOptions, jump.ID)
			}
			ui.Choice(c, &dlg.selectedID, jumpOptions, func(id string) string {
				if id == "" {
					return state.serversText("New jump host", "新建跳板机")
				}
				for _, jump := range state.servers.jumps {
					if jump.ID == id {
						return jump.Name
					}
				}
				return id
			}).Height(36)
			// Picking an entry loads it into the fields below; picking the
			// blank entry clears the draft for a new one. The choice writes
			// during declaration, so the load runs here, before the fields
			// are declared.
			if dlg.selectedID != dlg.loadedID {
				state.loadServersJump(dlg.selectedID)
			}
			state.serversDialogField(c, state.serversText("Name", "名称"), &dlg.nameField, &dlg.name, hitServersJumpName, "")
			state.serversDialogField(c, state.serversText("Address", "地址"), &dlg.addressField, &dlg.address, hitServersJumpAddress, "")
			ui.Row(c).Gap(8).Children(func() {
				ui.Column(c).Grow(1).Children(func() {
					state.serversDialogField(c, state.serversText("Port", "端口"), &dlg.portField, &dlg.port, hitServersJumpPort, "22")
				})
				ui.Column(c).Grow(1).Children(func() {
					state.serversDialogField(c, state.serversText("User", "用户"), &dlg.userField, &dlg.user, hitServersJumpUser, "")
				})
			})
			ui.Text(c, state.serversText("Authentication", "认证方式")).FontSize(metrics.FontSizeS).Weight(gfx.WeightMedium).Color(state.pal.muted)
			ui.Choice(c, &dlg.authMethod, []string{"password", "key"}, func(method string) string {
				if method == "key" {
					return state.serversText("Key", "密钥")
				}
				return state.serversText("Password", "密码")
			}).Height(36)
			ui.Row(c).Gap(8).Children(func() {
				state.declareButtonSized(c, hitServersJumpNew, state.serversText("New", "新建"), iconPlus, false, false, true, toolbarFont, false).Size(96, toolbarHeight)
				state.declareButtonSized(c, hitServersJumpDelete, state.serversText("Delete", "删除"), iconTrash, false, false, dlg.selectedID != "", toolbarFont, false).Size(96, toolbarHeight)
			})
			if dlg.err != "" {
				ui.Text(c, dlg.err).FontSize(metrics.FontSizeS).Weight(gfx.WeightMedium).Color(state.pal.danger).LineHeight(24)
			}
		})
	})
	dialog.Footer(func() {
		state.dialogActionFooter(c,
			components.ButtonAction{ID: hitServersDialogCancel, Label: state.messages.Cancel, Variant: components.ButtonSecondary},
			components.ButtonAction{ID: hitServersDialogConfirm, Label: state.serversText("Save", "保存")},
		)
	})
}

func (state *State) declareServersImportDialog(c ui.C) {
	dlg := &state.servers.importDlg
	if !dlg.bound {
		dlg.pathField.Bind(&dlg.path)
		dlg.bound = true
	}
	viewport := dialogViewport(c)
	height := dialogContentHeight(c.Canvas(), *c.Theme(), viewport, 620, state.serversText("Import servers from a CSV or .xlsx file (name, hostname, address, port, user, auth, group, tags, os, notes).", "从 CSV 或 .xlsx 文件导入服务器（列：名称、主机名、地址、端口、用户、认证、分组、标签、系统、备注）。"), 300)
	dialog := ui.Dialog(c, ui.DialogSpec{
		Title: state.serversText("Import servers", "导入服务器"), Description: state.serversText("Import servers from a CSV or .xlsx file (name, hostname, address, port, user, auth, group, tags, os, notes).", "从 CSV 或 .xlsx 文件导入服务器（列：名称、主机名、地址、端口、用户、认证、分组、标签、系统、备注）。"),
		Width: 620, Height: height, Layer: dialogLayer,
		Scope: dialogScope, DismissID: hitServersDialogDismiss, CloseID: hitServersDialogClose,
		InitialFocusID: hitServersDialogCancel,
	})
	dialog.Body(func() {
		ui.Column(c).Fill().Gap(6).Children(func() {
			state.serversDialogField(c, state.serversText("CSV or .xlsx file path", "CSV 或 .xlsx 文件路径"), &dlg.pathField, &dlg.path, hitServersImportPath, "")
			ui.Row(c).Gap(8).Children(func() {
				state.declareButtonSized(c, hitServersImportPreview, state.serversText("Preview", "预览"), iconSearch, false, false, true, toolbarFont, false).Size(110, toolbarHeight)
			})
			if dlg.previewErr != "" {
				ui.Text(c, dlg.previewErr).FontSize(metrics.FontSizeS).Weight(gfx.WeightMedium).Color(state.pal.danger).LineHeight(24)
			}
			if dlg.preview != nil {
				ui.Text(c, fmt.Sprintf(state.serversText("%d new, %d skipped, %d failed", "新增 %d，跳过 %d，失败 %d"), len(dlg.preview.ToAdd), len(dlg.preview.Skipped), len(dlg.preview.Failed))).FontSize(metrics.FontSizeS).Weight(gfx.WeightMedium).Color(state.pal.text)
				shown := 0
				for _, row := range dlg.preview.ToAdd {
					if shown >= 10 {
						break
					}
					ui.Text(c, row.Name+" · "+row.Address).FontSize(metrics.FontSizeS).Color(state.pal.muted).Ellipsis()
					shown++
				}
				for _, skipped := range dlg.preview.Skipped {
					if shown >= 10 {
						break
					}
					ui.Text(c, skipped.Name+" — "+state.serversText("skip", "跳过")+": "+skipped.Reason).FontSize(metrics.FontSizeS).Color(state.pal.muted).Ellipsis()
					shown++
				}
				for _, failed := range dlg.preview.Failed {
					if shown >= 10 {
						break
					}
					ui.Text(c, failed.Name+" — "+failed.Reason).FontSize(metrics.FontSizeS).Color(state.pal.danger).Ellipsis()
					shown++
				}
			}
			if dlg.summary != "" {
				ui.Text(c, dlg.summary).FontSize(metrics.FontSizeS).Weight(gfx.WeightMedium).Color(state.pal.text).LineHeight(24)
			}
		})
	})
	dialog.Footer(func() {
		apply := components.ButtonAction{ID: hitServersImportApply, Label: state.serversText("Import", "确认导入")}
		if dlg.preview == nil {
			apply.Variant = components.ButtonSecondary
		}
		state.dialogActionFooter(c,
			components.ButtonAction{ID: hitServersDialogCancel, Label: state.messages.Cancel, Variant: components.ButtonSecondary},
			apply,
		)
	})
}

func (state *State) declareServersDeleteDialog(c ui.C) {
	view := &state.servers
	server, ok := state.serverByID(view.deleteID)
	name := view.deleteID
	if ok {
		name = server.Name
	}
	description := fmt.Sprintf(state.serversText("Delete %s? The server is soft-deleted and can be restored.", "删除 %s？删除为软删除，可以恢复。"), name)
	viewport := dialogViewport(c)
	height := dialogContentHeight(c.Canvas(), *c.Theme(), viewport, 520, description, 24)
	dialog := ui.Dialog(c, ui.DialogSpec{
		Title: state.serversText("Delete server", "删除服务器"), Description: description,
		Width: 520, Height: height, Layer: dialogLayer,
		Scope: dialogScope, DismissID: hitServersDialogDismiss, CloseID: hitServersDialogClose,
		InitialFocusID: hitServersDialogCancel,
	})
	dialog.Body(func() {
		ui.Column(c).Fill().Gap(8).Children(func() {
			ui.Text(c, state.serversText("The saved password stays in the vault; deleting the server does not delete its vault secret.", "已保存的密码仍保留在 vault 中；删除服务器不会删除其 vault 凭据。")).FontSize(metrics.FontSizeS).Color(state.pal.muted).LineHeight(24)
		})
	})
	dialog.Footer(func() {
		state.dialogActionFooter(c,
			components.ButtonAction{ID: hitServersDialogCancel, Label: state.messages.Cancel, Variant: components.ButtonSecondary},
			components.ButtonAction{ID: hitServersDialogConfirm, Label: state.serversText("Delete", "删除"), Variant: components.ButtonDestructive},
		)
	})
}

func (state *State) declareServersFingerprintDialog(c ui.C) {
	pending := state.servers.pendingFP
	if pending == nil {
		return
	}
	description := fmt.Sprintf(state.serversText("The connection test for %s saw a different host key.", "%s 的连接测试发现了不同的主机密钥。"), pending.serverName)
	if pending.known == "" {
		description = fmt.Sprintf(state.serversText("The connection test for %s saw its host key for the first time.", "%s 的连接测试首次发现其主机密钥。"), pending.serverName)
	}
	viewport := dialogViewport(c)
	height := dialogContentHeight(c.Canvas(), *c.Theme(), viewport, 620, description, 96)
	dialog := ui.Dialog(c, ui.DialogSpec{
		Title: state.serversText("Confirm host key", "确认主机指纹"), Description: description,
		Width: 620, Height: height, Layer: dialogLayer,
		Scope: dialogScope, DismissID: hitServersDialogDismiss, CloseID: hitServersDialogClose,
		InitialFocusID: hitServersDialogCancel,
	})
	dialog.Body(func() {
		ui.Column(c).Fill().Gap(8).Children(func() {
			if pending.known != "" {
				ui.Text(c, fmt.Sprintf(state.serversText("Known fingerprint: %s", "已知指纹：%s"), pending.known)).FontSize(metrics.FontSizeS).Color(state.pal.muted).LineHeight(24)
			}
			ui.Text(c, fmt.Sprintf(state.serversText("Seen fingerprint: %s", "此次指纹：%s"), pending.fingerprint)).FontSize(metrics.FontSizeS).Weight(gfx.WeightMedium).Color(state.pal.text).LineHeight(24)
			ui.Text(c, state.serversText("Confirm only if you expect this key — for example after reinstalling the server. The fingerprint is saved only when you confirm.", "仅当你预期密钥会变化时才确认（例如重装服务器后）。只有确认后才会保存指纹。")).FontSize(metrics.FontSizeS).Color(state.pal.muted).LineHeight(24)
		})
	})
	dialog.Footer(func() {
		state.dialogActionFooter(c,
			components.ButtonAction{ID: hitServersDialogCancel, Label: state.serversText("Not now", "暂不确认"), Variant: components.ButtonSecondary},
			components.ButtonAction{ID: hitServersFingerprintOK, Label: state.serversText("Trust this key", "信任此密钥")},
		)
	})
}

// previewServersImport reads the import dialog's file and builds a
// preview. A .xlsx file has no separate preview step in serverassets: it
// imports directly and reports its summary.
func (state *State) previewServersImport() {
	dlg := &state.servers.importDlg
	path := strings.TrimSpace(dlg.path.String())
	if path == "" {
		dlg.previewErr = state.serversText("Enter a CSV or .xlsx file path.", "请输入 CSV 或 .xlsx 文件路径。")
		return
	}
	store := state.serversStore()
	if store == nil {
		dlg.previewErr = state.servers.storeErr
		return
	}
	if strings.HasSuffix(strings.ToLower(path), ".xlsx") {
		data, err := os.ReadFile(path)
		if err != nil {
			dlg.previewErr = err.Error()
			return
		}
		summary, err := store.ImportXLSX(data)
		if err != nil {
			dlg.previewErr = err.Error()
			return
		}
		dlg.preview, dlg.previewErr = nil, ""
		dlg.summary = fmt.Sprintf(state.serversText("Imported %d, skipped %d, failed %d.", "已导入 %d，跳过 %d，失败 %d。"), summary.Added, summary.Skipped, summary.Failed)
		state.refreshServers(false)
		return
	}
	file, err := os.Open(path)
	if err != nil {
		dlg.previewErr = err.Error()
		return
	}
	defer file.Close()
	rows, err := serverassets.ParseCSV(file)
	if err != nil {
		dlg.previewErr = err.Error()
		return
	}
	preview, err := store.PreviewImport(rows)
	if err != nil {
		dlg.previewErr = err.Error()
		return
	}
	dlg.preview, dlg.previewErr, dlg.summary = preview, "", ""
}

// applyServersImport applies the previewed import and shows the summary.
func (state *State) applyServersImport() {
	dlg := &state.servers.importDlg
	if dlg.preview == nil {
		return
	}
	store := state.serversStore()
	if store == nil {
		dlg.previewErr = state.servers.storeErr
		return
	}
	summary, err := store.ApplyImport(dlg.preview)
	if err != nil {
		dlg.previewErr = err.Error()
		return
	}
	dlg.summary = fmt.Sprintf(state.serversText("Imported %d, skipped %d, failed %d.", "已导入 %d，跳过 %d，失败 %d。"), summary.Added, summary.Skipped, summary.Failed)
	var reasons []string
	for _, result := range summary.Results {
		if result.Status == "failed" {
			reasons = append(reasons, result.Name+": "+result.Reason)
		}
	}
	if len(reasons) > 0 {
		dlg.summary += " " + strings.Join(reasons, "; ")
	}
	dlg.preview = nil
	state.refreshServers(false)
}
