package workbench

// Servers page (phase 2): click dispatch for the page and its dialogs.

import (
	"github.com/vivaui/viva/gfx"
	"github.com/vivaui/viva/ui"
)

// clickServers handles a click on the servers page or one of its dialogs.
// It returns false for identities it does not own. The overview and alert
// center pages share the servers hit feature, so they dispatch first.
func (state *State) clickServers(id gfx.ID) bool {
	view := &state.servers
	switch state.activeSection {
	case sectionServersHome:
		return state.clickServersHome(id)
	case sectionServerAlerts:
		return state.clickServerAlerts(id)
	case sectionServersBatch:
		return state.clickServersBatch(id)
	case sectionServersSFTP:
		return state.clickServersSFTP(id)
	case sectionServers:
	default:
		return false
	}
	// Dialog buttons win over the page behind them.
	if view.pendingFP != nil {
		switch id {
		case hitServersFingerprintOK:
			state.confirmServerFingerprint()
			return true
		case hitServersDialogDismiss, hitServersDialogClose, hitServersDialogCancel:
			view.pendingFP = nil
			return true
		}
		return false
	}
	if view.deleteID != "" {
		switch id {
		case hitServersDialogConfirm:
			deleted := view.deleteID
			view.deleteID = ""
			if store := state.serversStore(); store == nil {
				view.errorText = view.storeErr
				return true
			} else if err := store.DeleteServer(deleted); err != nil {
				view.errorText = err.Error()
				return true
			}
			view.status = state.serversText("Server deleted (soft delete).", "服务器已删除（软删除）。")
			state.refreshServers(false)
			return true
		case hitServersDialogDismiss, hitServersDialogClose, hitServersDialogCancel:
			view.deleteID = ""
			return true
		}
		return false
	}
	if view.importDlg.open {
		switch id {
		case hitServersImportPreview:
			state.previewServersImport()
			return true
		case hitServersImportApply:
			state.applyServersImport()
			return true
		case hitServersDialogDismiss, hitServersDialogClose, hitServersDialogCancel:
			view.importDlg.open = false
			view.importDlg.preview = nil
			return true
		}
	}
	if view.jumpsDlg.open {
		switch id {
		case hitServersJumpNew:
			state.openServersJumpDialog()
			return true
		case hitServersJumpDelete:
			state.deleteServersJump()
			return true
		case hitServersDialogConfirm:
			state.saveServersJump()
			return true
		case hitServersDialogDismiss, hitServersDialogClose, hitServersDialogCancel:
			view.jumpsDlg.open = false
			return true
		}
	}
	if view.editor.open {
		switch id {
		case hitServersDialogConfirm:
			state.saveServersEditor()
			return true
		case hitServersDialogDismiss, hitServersDialogClose, hitServersDialogCancel:
			view.editor.open = false
			return true
		}
	}
	switch id {
	case hitServersSearch:
		return true
	case hitServersRefresh:
		state.refreshServers(true)
		return true
	case hitServersNew:
		state.openServersEditor("")
		return true
	case hitServersTestAll:
		state.runServerTestsAll()
		return true
	case hitServersJumpHosts:
		state.openServersJumpDialog()
		return true
	case hitServersImport:
		view.importDlg.open = true
		view.importDlg.preview = nil
		view.importDlg.previewErr = ""
		view.importDlg.summary = ""
		return true
	case hitServersShowDeleted:
		// The checkbox writes the filter itself; the click only asks for a
		// repaint so the list re-syncs at once.
		return true
	case hitServersTest:
		if view.selectedID != "" {
			state.runServerTest(view.selectedID)
		}
		return true
	case hitServersOpenTerminal:
		if server, ok := state.serverByID(view.selectedID); ok {
			state.openServerTerminal(server)
		}
		return true
	case hitServersEdit:
		if view.selectedID != "" {
			state.openServersEditor(view.selectedID)
		}
		return true
	case hitServersDelete:
		if view.selectedID != "" {
			view.deleteID = view.selectedID
		}
		return true
	case hitServersRestore:
		if view.selectedID != "" {
			state.restoreServer(view.selectedID)
		}
		return true
	case hitServersPrevPage:
		if view.page > 0 {
			view.page--
			view.list.Select(-1)
			view.list.ScrollTo(0, ui.ListStart)
			state.syncServersVisible()
		}
		return true
	case hitServersNextPage:
		if view.page+1 < state.serversPages() {
			view.page++
			view.list.Select(-1)
			view.list.ScrollTo(0, ui.ListStart)
			state.syncServersVisible()
		}
		return true
	}
	if id.Scope() == hitServersRowList {
		index := state.serversIndexForHit(id)
		if index >= 0 && index < len(view.visible) {
			server := view.visible[index]
			if view.selectedID == server.ID {
				view.selectedID = ""
			} else {
				view.selectedID = server.ID
			}
			return true
		}
	}
	return false
}
