package workbench

import (
	"strings"
	"testing"
	"time"

	"github.com/vivaui/viva/gfx"
	"motty/internal/serversftp"
)

// The SFTP page opens on the currently selected server and reports a
// dial failure instead of hanging: the browser is opened on a background
// goroutine and the failure lands in browserErr.
func TestSFTPPageOpensAndReportsDialFailure(t *testing.T) {
	state := newTestState(t, testOptions(t))
	drain := installTestPost(t, state)
	state.openServersBatch()
	server := addBatchTestServer(t, state, "web", "", "web", "Ubuntu")
	state.servers.selectedID = server.ID
	state.openServersSFTP()
	view := &state.servers.sftp
	if view.serverID != server.ID {
		t.Fatalf("sftp serverID = %q, want the selected server %q", view.serverID, server.ID)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		drain()
		if !view.opening && view.browserErr != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	drain()
	if view.browserErr == "" {
		t.Fatal("opening an unreachable server reported no error")
	}
	// The failure reason is the dial error, shown on the page as
	// browserErr (errorText stays empty: the page itself is still usable
	// and the user can pick another server).
	if !strings.Contains(view.browserErr, "127.0.0.1") && !strings.Contains(view.browserErr, "refused") && !strings.Contains(view.browserErr, "dial") {
		t.Fatalf("sftp browserErr = %q, want a dial failure reason", view.browserErr)
	}
	// The page still paints with its toolbar and sidebar rows.
	size := gfx.Size{Width: 1400, Height: 900}
	canvas := &gfx.Canvas{}
	state.Paint(canvas, size)
	if _, ok := hitByID(canvas, hitServersSFTPOpen); !ok {
		t.Fatal("the SFTP open button is not painted")
	}
	if _, ok := hitByID(canvas, hitServersSFTPUpload); !ok {
		t.Fatal("the SFTP upload button is not painted")
	}
	if _, ok := hitByID(canvas, hitServersSFTP); !ok {
		t.Fatal("the SFTP sidebar row is not painted")
	}
}

// Without any server the page opens empty and paints its empty state.
func TestSFTPPageNoServers(t *testing.T) {
	state := newTestState(t, testOptions(t))
	state.openServersSFTP()
	view := &state.servers.sftp
	if view.serverID != "" {
		t.Fatalf("sftp serverID = %q, want empty with no servers", view.serverID)
	}
	if view.browser != nil || view.browserErr != "" {
		t.Fatal("an empty page opened a browser")
	}
	size := gfx.Size{Width: 1400, Height: 900}
	canvas := &gfx.Canvas{}
	state.Paint(canvas, size)
	if _, ok := hitByID(canvas, hitServersSFTPOpen); !ok {
		t.Fatal("the SFTP open button is not painted on an empty page")
	}
}

// Rename and delete dialogs are pure state on the click dispatcher: the
// rename dialog opens for the selected entry and Escape closes it, the
// delete confirmation clears on cancel, and confirming with no browser
// is a no-op rather than a crash.
func TestSFTPDialogStateMachine(t *testing.T) {
	state := newTestState(t, testOptions(t))
	state.openServersSFTP()
	view := &state.servers.sftp
	view.entries = []serversftp.Entry{
		{Name: "docs", IsDir: true},
		{Name: "notes.txt", Size: 42},
	}
	view.selected = "notes.txt"
	if entry, ok := state.sftpSelectedEntry(); !ok || entry.Name != "notes.txt" {
		t.Fatal("sftpSelectedEntry did not find the selected file")
	}
	// The delete confirmation opens for the selected entry and Escape
	// closes it.
	if !state.clickServersSFTP(hitServersSFTPDelete) {
		t.Fatal("delete click was not handled")
	}
	if view.deleteName != "notes.txt" {
		t.Fatalf("deleteName = %q, want notes.txt", view.deleteName)
	}
	if result, ok := state.serversKey(gfx.KeyEvent{Key: gfx.KeyEscape}); !ok || !result.Handled {
		t.Fatal("Escape did not close the delete confirmation")
	}
	if view.deleteName != "" {
		t.Fatal("delete confirmation is still open after Escape")
	}
	if !state.clickServersSFTP(hitServersSFTPDelete) {
		t.Fatal("delete click was not handled")
	}
	if !state.clickServersSFTP(hitServersSFTPDialogCancel) {
		t.Fatal("delete cancel click was not handled")
	}
	if view.deleteName != "" {
		t.Fatal("delete confirmation did not clear on cancel")
	}
	if !state.clickServersSFTP(hitServersSFTPDelete) {
		t.Fatal("delete click was not handled")
	}
	// No browser: confirming is a silent no-op.
	if !state.clickServersSFTP(hitServersSFTPDialogConfirm) {
		t.Fatal("delete confirm click was not handled")
	}
	if view.deleteName != "" {
		t.Fatal("delete confirmation did not clear after confirming")
	}
	// Enter and Up are no-ops without a browser, never a crash.
	view.selected = "docs"
	if !state.clickServersSFTP(hitServersSFTPEnter) {
		t.Fatal("enter click was not handled")
	}
	if !state.clickServersSFTP(hitServersSFTPUp) {
		t.Fatal("up click was not handled")
	}
	// Closing a nil browser is a no-op.
	state.closeServersSFTPBrowser()
	if view.browser != nil {
		t.Fatal("closeServersSFTPBrowser set a browser on a nil page")
	}
	// The mkdir and rename buttons do nothing without a browser.
	view.selected = ""
	if !state.clickServersSFTP(hitServersSFTPMkdir) {
		t.Fatal("mkdir click was not handled")
	}
	if view.mkdirDlg {
		t.Fatal("mkdir opened its dialog without a browser")
	}
	view.selected = "notes.txt"
	if !state.clickServersSFTP(hitServersSFTPRename) {
		t.Fatal("rename click was not handled")
	}
	if view.renameDlg {
		t.Fatal("rename opened its dialog without a browser")
	}
}
