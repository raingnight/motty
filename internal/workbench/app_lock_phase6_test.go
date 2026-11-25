package workbench

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/vivaui/viva/app/systemauth"
	"motty/internal/serveralerts"
	"motty/internal/serverassets"
)

// Phase 6 (workbench wiring): failure lockout, idle auto-lock, lock
// gates for batch/SFTP, tray lock entry and server-stats line, and the
// forgotten-password flow.

func TestAppLockFailureLockoutEscalatesAndSurvivesRestart(t *testing.T) {
	opts := testOptions(t)
	state := newTestState(t, opts)
	const password = "long fixture password"
	if err := finishAppLock(t, state, appLockEnable, "", password); err != nil {
		t.Fatal(err)
	}
	state.lockApplication()
	for i := 0; i < 5; i++ {
		if err := finishAppLock(t, state, appLockUnlock, "wrong password", ""); !errors.Is(err, errLockWrongPassword) {
			t.Fatalf("attempt %d: %v", i+1, err)
		}
	}
	record, err := readLockVerifier(state.appProtection.verifierPath)
	if err != nil {
		t.Fatal(err)
	}
	if record.FailedAttempts != 5 || record.LockCount != 1 {
		t.Fatalf("after 5 failures: attempts=%d locks=%d", record.FailedAttempts, record.LockCount)
	}
	if remaining := record.lockoutRemaining(time.Now()); remaining < 50*time.Second || remaining > 65*time.Second {
		t.Fatalf("first lockout remaining = %v, want about 1 minute", remaining)
	}
	lockedUntil := record.LockedUntilUnix
	if err := finishAppLock(t, state, appLockUnlock, password, ""); !errors.Is(err, errAppLockLockedOut) || !state.appLocked() {
		t.Fatalf("correct password during lockout: %v", err)
	}
	// A rejected attempt during the lockout extends nothing.
	record, err = readLockVerifier(state.appProtection.verifierPath)
	if err != nil {
		t.Fatal(err)
	}
	if record.LockedUntilUnix != lockedUntil {
		t.Fatal("an attempt during the lockout moved its end")
	}
	if text := state.appLockLockedOutText(); !strings.HasPrefix(text, "Too many incorrect attempts. Try again in ") || !strings.Contains(text, "sec") && !strings.Contains(text, "min") {
		t.Fatalf("lockout text = %q", text)
	}
	// Restart: a fresh protection object reading the same file still
	// refuses the correct password.
	restartOpts := testOptions(t)
	restartOpts.StatePath = opts.StatePath
	restarted := newTestState(t, restartOpts)
	if !restarted.appLocked() {
		t.Fatal("restarted window did not start locked")
	}
	if err := finishAppLock(t, restarted, appLockUnlock, password, ""); !errors.Is(err, errAppLockLockedOut) || !restarted.appLocked() {
		t.Fatalf("restarted unlock during lockout: %v", err)
	}
	// Expire the lockout, fail five more times: the second lockout is
	// five minutes.
	restarted.appProtection.record.LockedUntilUnix = time.Now().Add(-time.Second).Unix()
	for i := 0; i < 5; i++ {
		if err := finishAppLock(t, restarted, appLockUnlock, "wrong password", ""); !errors.Is(err, errLockWrongPassword) {
			t.Fatalf("second round attempt %d: %v", i+1, err)
		}
	}
	record, err = readLockVerifier(state.appProtection.verifierPath)
	if err != nil {
		t.Fatal(err)
	}
	if record.LockCount != 2 {
		t.Fatalf("lock count = %d, want 2", record.LockCount)
	}
	if remaining := record.lockoutRemaining(time.Now()); remaining < 290*time.Second || remaining > 310*time.Second {
		t.Fatalf("second lockout remaining = %v, want about 5 minutes", remaining)
	}
	// Expire again and unlock correctly: the failure history clears,
	// on disk as well as in memory.
	restarted.appProtection.record.LockedUntilUnix = time.Now().Add(-time.Second).Unix()
	if err := finishAppLock(t, restarted, appLockUnlock, password, ""); err != nil {
		t.Fatal(err)
	}
	record, err = readLockVerifier(state.appProtection.verifierPath)
	if err != nil {
		t.Fatal(err)
	}
	if record.FailedAttempts != 0 || record.LockCount != 0 || record.LockedUntilUnix != 0 {
		t.Fatalf("successful unlock left attempts=%d locks=%d until=%d", record.FailedAttempts, record.LockCount, record.LockedUntilUnix)
	}
}

func TestAppLockIdleLockUsesLastActivity(t *testing.T) {
	state := newTestState(t, testOptions(t))
	const password = "long fixture password"
	if err := finishAppLock(t, state, appLockEnable, "", password); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	state.appLockIdleMinutes = 15
	state.noteActivityAt(now)
	if state.checkIdleLock(now.Add(14 * time.Minute)) {
		t.Fatal("locked one minute before the timeout")
	}
	if !state.checkIdleLock(now.Add(16*time.Minute)) || !state.appLocked() {
		t.Fatal("did not lock after the timeout")
	}
	if err := finishAppLock(t, state, appLockUnlock, password, ""); err != nil {
		t.Fatal(err)
	}
	// "Never" never locks, however stale the activity is.
	state.appLockIdleMinutes = 0
	state.noteActivityAt(now.Add(-24 * time.Hour))
	if state.checkIdleLock(now) || state.appLocked() {
		t.Fatal("idle lock fired with the timeout set to never")
	}
}

func TestAppLockIdleWakeArmsAndFires(t *testing.T) {
	state := newTestState(t, testOptions(t))
	const password = "long fixture password"
	if err := finishAppLock(t, state, appLockEnable, "", password); err != nil {
		t.Fatal(err)
	}
	var captured func()
	var delay time.Duration
	state.options.PostAfter = func(d time.Duration, fn func()) func() {
		delay, captured = d, fn
		return func() {}
	}
	state.appLockIdleMinutes = 5
	state.noteActivityAt(time.Now().Add(-20 * time.Minute))
	state.armIdleLockWake()
	if captured == nil || delay != time.Minute {
		t.Fatalf("idle wake not armed: delay=%v armed=%v", delay, captured != nil)
	}
	captured()
	if !state.appLocked() {
		t.Fatal("the idle wake did not lock")
	}
}

func TestAppLockForgotPasswordClearsVaultAndLock(t *testing.T) {
	opts := testOptions(t)
	state := newTestState(t, opts)
	const password = "long fixture password"
	if err := finishAppLock(t, state, appLockEnable, "", password); err != nil {
		t.Fatal(err)
	}
	if err := state.secrets.vault.Set("forgot-test", "test credential"); err != nil {
		t.Fatal(err)
	}
	state.lockApplication()
	if !state.clickAppLock(hitAppLockForgot) || !state.lockUI.forgetConfirm {
		t.Fatal("forgot-password entry did not open its confirmation")
	}
	if !state.clickAppLock(hitAppLockForgotConfirm) {
		t.Fatal("forgot-password confirmation was not handled")
	}
	if state.appLocked() || state.appLockEnabled() {
		t.Fatal("forgot-password flow left the lock in place")
	}
	if _, err := os.Stat(state.appProtection.verifierPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("verifier after forgetting: %v", err)
	}
	if state.secrets.vault.Len() != 0 {
		t.Fatal("forgot-password flow left saved credentials behind")
	}
}

func TestAppLockLockedBatchAndSFTPRefused(t *testing.T) {
	state := newTestState(t, testOptions(t))
	const password = "long fixture password"
	if err := finishAppLock(t, state, appLockEnable, "", password); err != nil {
		t.Fatal(err)
	}
	state.lockApplication()
	state.requestServersBatchRun("echo hi", "", nil)
	if state.servers.batch.running || state.servers.batch.errorText != state.messages.AppLock.LockedNotice {
		t.Fatalf("locked batch run: running=%v error=%q", state.servers.batch.running, state.servers.batch.errorText)
	}
	state.openServersSFTPBrowser(serverassets.Server{ID: "s1", Name: "s1"})
	if state.servers.sftp.browser != nil || state.servers.sftp.opening || state.servers.sftp.errorText != state.messages.AppLock.LockedNotice {
		t.Fatalf("locked SFTP open: browser=%v opening=%v error=%q", state.servers.sftp.browser != nil, state.servers.sftp.opening, state.servers.sftp.errorText)
	}
	state.startServersSFTPUpload("/tmp/never-uploaded")
	if state.servers.sftp.errorText != state.messages.AppLock.LockedNotice {
		t.Fatalf("locked SFTP upload: error=%q", state.servers.sftp.errorText)
	}
	state.runServersSFTPMkdir("never-created")
	if state.servers.sftp.errorText != state.messages.AppLock.LockedNotice {
		t.Fatalf("locked SFTP mkdir: error=%q", state.servers.sftp.errorText)
	}
	// Unlocked, the same entries get past the gate: the batch run now
	// complains about targets, not about the lock.
	if err := finishAppLock(t, state, appLockUnlock, password, ""); err != nil {
		t.Fatal(err)
	}
	state.requestServersBatchRun("echo hi", "", nil)
	if state.servers.batch.errorText == state.messages.AppLock.LockedNotice {
		t.Fatal("unlocked batch run still reported the lock")
	}
}

func TestTrayMenuAppLockAndServerStats(t *testing.T) {
	state := newTestState(t, testOptions(t))
	tray := NewTray(func() {})
	window := &fakeTrayWindow{}
	tray.Add(state, window)
	menu := tray.Menu()
	for _, item := range menu {
		if item.ID == "lock-app" || item.ID == "server-stats" {
			t.Fatalf("menu showed %q before the lock existed or stats were known", item.ID)
		}
	}
	const password = "long fixture password"
	if err := finishAppLock(t, state, appLockEnable, "", password); err != nil {
		t.Fatal(err)
	}
	state.servers.stats = serveralerts.Stats{Total: 3, Online: 2, Offline: 1, Abnormal: 0}
	state.servers.healthAt = time.Now()
	menu = tray.Menu()
	var lockSelect func()
	statsFound, lockFound := false, false
	for _, item := range menu {
		switch item.ID {
		case "lock-app":
			lockFound = true
			lockSelect = item.OnSelect
			if item.Title != state.messages.TrayLockApp {
				t.Fatalf("lock entry title = %q", item.Title)
			}
		case "server-stats":
			statsFound = true
			if !item.Disabled {
				t.Fatal("server-stats line is selectable")
			}
			if want := "Servers: 2 online · 1 offline · 0 abnormal"; item.Title != want {
				t.Fatalf("server-stats line = %q, want %q", item.Title, want)
			}
		}
	}
	if !lockFound || !statsFound {
		t.Fatalf("menu lacked lock=%v stats=%v", lockFound, statsFound)
	}
	if lockSelect == nil {
		t.Fatal("lock entry has no action")
	}
	lockSelect()
	if !state.appLocked() {
		t.Fatal("tray lock entry did not lock the application")
	}
	menu = tray.Menu()
	if len(menu) != 2 {
		t.Fatalf("locked tray menu has %d entries, want show-window and quit only", len(menu))
	}
	for _, item := range menu {
		if item.ID == "lock-app" || item.ID == "server-stats" {
			t.Fatalf("locked tray menu showed %q", item.ID)
		}
	}
}

func TestAppLockEnrollVaultGate(t *testing.T) {
	state := newTestState(t, testOptions(t))
	const password = "long fixture password"
	if err := finishAppLock(t, state, appLockEnable, "", password); err != nil {
		t.Fatal(err)
	}
	// Enabling linked the vault to the application lock password, and
	// the linkage survived to the file.
	record, err := readLockVerifier(state.appProtection.verifierPath)
	if err != nil {
		t.Fatal(err)
	}
	if !record.VaultLinked {
		t.Fatal("enabling did not record the vault linkage")
	}
	vaultFile := state.secrets.vault
	// Sealed again, the vault still opens with the application lock
	// password, so enrollment is allowed.
	vaultFile.Seal()
	if err := state.checkVaultForEnroll(password); err != nil {
		t.Fatalf("enroll with the vault password: %v", err)
	}
	// A wrong application lock password reports as a wrong password,
	// not as a vault problem.
	vaultFile.Seal()
	if err := state.checkVaultForEnroll("wrong password"); !errors.Is(err, errLockWrongPassword) {
		t.Fatalf("enroll with a wrong password: %v", err)
	}
	// Re-wrap the vault with a different password: enrollment is now
	// refused, because system unlock could never open it.
	if err := vaultFile.UnlockWithPassword(password); err != nil {
		t.Fatal(err)
	}
	if err := vaultFile.WrapWithPassword("another password entirely"); err != nil {
		t.Fatal(err)
	}
	vaultFile.Seal()
	if err := state.checkVaultForEnroll(password); !errors.Is(err, errAppLockVaultProtected) {
		t.Fatalf("enroll with a differently-protected vault: %v", err)
	}
	if text := state.appLockErrorText(errAppLockVaultProtected); text != state.messages.AppLock.SystemVaultProtected {
		t.Fatalf("enroll refusal text = %q", text)
	}
}

func TestAppLockSystemUnlockClearsFailures(t *testing.T) {
	opts := testOptions(t)
	state := newTestState(t, opts)
	promptErr := error(nil)
	prompts := 0
	markers := map[string][]byte{}
	state.appProtection.system = appLockSystem{
		method: func() systemauth.VerificationMethod { return systemauth.VerificationWindowsHello },
		verify: func(context.Context, string) error { prompts++; return promptErr },
		load:   func(_, id string) ([]byte, error) { return markers[id], nil },
		store:  func(_, id string, data []byte) error { markers[id] = data; return nil },
		remove: func(_, id string) error { delete(markers, id); return nil },
	}
	const password = "long fixture password"
	if err := finishAppLock(t, state, appLockEnable, "", password); err != nil {
		t.Fatal(err)
	}
	if err := finishAppLock(t, state, appLockEnroll, password, ""); err != nil {
		t.Fatal(err)
	}
	state.lockApplication()
	for i := 0; i < 3; i++ {
		if err := finishAppLock(t, state, appLockUnlock, "wrong password", ""); !errors.Is(err, errLockWrongPassword) {
			t.Fatalf("attempt %d: %v", i+1, err)
		}
	}
	if err := finishAppLock(t, state, appLockSystemUnlock, "", ""); err != nil || state.appLocked() {
		t.Fatalf("system unlock: %v", err)
	}
	record, err := readLockVerifier(state.appProtection.verifierPath)
	if err != nil {
		t.Fatal(err)
	}
	if record.FailedAttempts != 0 || record.LockCount != 0 || record.LockedUntilUnix != 0 {
		t.Fatalf("system unlock left attempts=%d locks=%d until=%d", record.FailedAttempts, record.LockCount, record.LockedUntilUnix)
	}
	if prompts != 2 {
		t.Fatalf("system prompts = %d, want one for enrollment and one for the unlock", prompts)
	}
}

func TestRestoreAppLockIdleMinutes(t *testing.T) {
	if got := restoreAppLockIdleMinutes(savedState{}); got != 15 {
		t.Fatalf("unset timeout = %d, want 15", got)
	}
	if got := restoreAppLockIdleMinutes(savedState{AppLockIdleMinutes: 30}); got != 30 {
		t.Fatalf("saved 30 = %d", got)
	}
	if got := restoreAppLockIdleMinutes(savedState{AppLockIdleMinutes: 7}); got != 15 {
		t.Fatalf("saved 7 = %d, want the default 15", got)
	}
	if got := restoreAppLockIdleMinutes(savedState{AppLockIdleOff: true}); got != 0 {
		t.Fatalf("saved never = %d, want 0", got)
	}
	state := newTestState(t, testOptions(t))
	state.appLockIdleMinutes = 0
	if saved := state.snapshot(); !saved.AppLockIdleOff || saved.AppLockIdleMinutes != 0 {
		t.Fatalf("snapshot of never: off=%v minutes=%d", saved.AppLockIdleOff, saved.AppLockIdleMinutes)
	}
	state.appLockIdleMinutes = 60
	if saved := state.snapshot(); saved.AppLockIdleOff || saved.AppLockIdleMinutes != 60 {
		t.Fatalf("snapshot of 60: off=%v minutes=%d", saved.AppLockIdleOff, saved.AppLockIdleMinutes)
	}
}
