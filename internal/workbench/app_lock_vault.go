package workbench

import (
	"errors"
	"fmt"
	"log"
	"os"

	"motty/internal/vault"
)

// Application lock and vault linkage (phase 6). The vault API is the
// frozen one from internal/vault/password.go; nothing here changes it.
// Every branch runs in runAppLockAction's finish callback, on the window
// thread, with the passwords still in hand — and no branch re-locks the
// application: a vault failure leaves the application unlocked, the vault
// sealed, and a notice pointing at Settings.
//
// One distinction the vault API cannot make matters throughout: a legacy
// file (old master password) and a new password-protected file look the
// same — PasswordProtected and NeedsConversion are both true for either.
// The branches below therefore never assume which one it is. Where the
// old password is needed and not in hand (enabling the lock over a legacy
// vault), the vault is left exactly as it was and the user is pointed at
// the existing conversion dialog in Settings, which asks for that
// password. Where the current application lock password is in hand, trying
// it against the vault is the test: it opens a vault wrapped with that
// same password and fails on anything else.

// vaultPasswordProtected reports whether this window has a vault whose
// file carries a KDF — legacy or application-password alike.
func (state *State) vaultPasswordProtected() bool {
	v := state.secrets.vault
	return v != nil && v.PasswordProtected()
}

// checkVaultForEnroll decides whether a system unlock may be enrolled
// with current as the application lock password. A vault the password
// opens — unprotected, already unlocked in this session, or wrapped with
// this same password — allows it; a vault another password protects
// refuses it with errAppLockVaultProtected. A wrong application lock
// password reports as a wrong password, not as a vault problem: the
// verifier is checked first when the vault is sealed and protected.
func (state *State) checkVaultForEnroll(current string) error {
	v := state.secrets.vault
	if v == nil || !v.PasswordProtected() || v.Unlocked() {
		return nil
	}
	if p := state.appProtection; p != nil {
		if err := p.record.verify(current); err != nil {
			return err
		}
	}
	if err := v.UnlockWithPassword(current); err != nil {
		return errAppLockVaultProtected
	}
	return nil
}

// vaultLinkedToAppLock reports whether the vault is protected with the
// application lock password, by the persisted flag on the lock record.
// A legacy file reads false until an app-lock action proves otherwise.
func (state *State) vaultLinkedToAppLock() bool {
	p := state.appProtection
	return p != nil && p.record.VaultLinked
}

// setVaultLinked records that the vault is (or is not) protected with
// the application lock password, persisting the flag in app-lock.json
// next to the counters. It is a no-op once the lock is gone: disabling
// removes the file, so there is nothing to persist the flag in.
func (state *State) setVaultLinked(linked bool) {
	p := state.appProtection
	if p == nil || p.verifierPath == "" || p.record.Version == 0 || p.record.VaultLinked == linked {
		return
	}
	p.record.VaultLinked = linked
	if err := p.record.save(p.verifierPath); err != nil {
		log.Printf("motty: save application lock vault linkage: %v", err)
	}
}

// syncVaultAfterAppLock links the vault to a successful application lock
// action. action is one of the runAppLockAction actions; current and next
// are its passwords. It only ever writes the vault or a status notice.
func (state *State) syncVaultAfterAppLock(action appLockAction, current, next string) {
	v := state.secrets.vault
	if v == nil {
		return
	}
	m := state.messages.AppLock
	switch action {
	case appLockEnable:
		if !v.PasswordProtected() {
			if err := v.WrapWithPassword(next); err != nil {
				state.status = state.messages.AppLock.VaultUnavailable
			} else {
				state.setVaultLinked(true)
			}
			return
		}
		// Protected already: try the new password against it. When it
		// opens, the vault is already protected with this password and
		// there is nothing to wrap; when it does not, this is a legacy
		// file (or a leftover wrapping) whose old password is not in
		// hand, so leave it untouched and point at Settings.
		if err := v.UnlockWithPassword(next); err != nil {
			state.status = m.VaultConvertPrompt
		} else {
			state.setVaultLinked(true)
		}
	case appLockUnlock:
		if v.PasswordProtected() {
			if v.Unlocked() {
				state.setVaultLinked(true)
				return
			}
			if err := v.UnlockWithPassword(current); err != nil {
				state.status = m.VaultUnavailable
			} else {
				state.setVaultLinked(true)
			}
			return
		}
		// A vault that predates the lock gets its one-time migration:
		// wrap it with the password just typed. A failure leaves the
		// vault as it was, sealed or not.
		if err := v.WrapWithPassword(current); err != nil {
			state.status = m.VaultUnavailable
		} else {
			state.status = m.VaultMigrated
			state.setVaultLinked(true)
		}
	case appLockChange:
		if v.PasswordProtected() {
			if err := v.ChangePassword(current, next); err != nil {
				state.status = m.VaultUnavailable
			} else {
				state.setVaultLinked(true)
			}
			return
		}
		if err := v.WrapWithPassword(next); err != nil {
			state.status = m.VaultUnavailable
		} else {
			state.setVaultLinked(true)
		}
	case appLockDisable:
		if !v.PasswordProtected() {
			return
		}
		if err := v.RemovePassword(current); err != nil && !errors.Is(err, vault.ErrNotPasswordProtected) {
			state.status = m.VaultUnavailable
		}
	case appLockSystemUnlock:
		// System unlock cannot supply a password, so a protected vault
		// stays sealed. Say so once, on the status line, and leave the
		// application unlocked: the user can still lock again and use
		// the password.
		if v.PasswordProtected() && !v.Unlocked() {
			state.status = m.VaultSealedSystem
		}
	}
}

// forgetAppLockPassword is the forgotten-password escape, from the lock
// screen's confirmation: there is no recovery and no recovery key, so it
// clears every encrypted credential and removes the application lock.
// Order matters: the vault first, then the lock file, then the unlocked
// state. Server assets, collected data, alerts and history are other
// files and are untouched; the OS keychain marker of a system unlock goes
// with the lock file. A vault that fails to clear aborts the whole step,
// so a half-forgotten state never strands the user without either.
func (state *State) forgetAppLockPassword() {
	p := state.appProtection
	if p == nil {
		return
	}
	if v := state.secrets.vault; v != nil {
		if err := v.Clear(); err != nil {
			state.lockUI.error = state.messages.AppLock.ForgotFailed
			state.invalidateWindow()
			return
		}
	}
	record := p.record
	if err := os.Remove(p.verifierPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		state.lockUI.error = state.messages.AppLock.ForgotFailed
		state.invalidateWindow()
		return
	}
	if record.SystemID != "" {
		_ = p.system.remove(keyService(appLockSystemService), record.SystemID)
	}
	p.record = lockVerifier{}
	p.locked.Store(false)
	state.lockUI = appLockUI{}
	state.status = state.messages.AppLock.ForgotDone
	state.noteActivity()
	state.resumeApplication(state.profileWindows())
	state.invalidateWindow()
}

// appLockLockedOutText is the lockout notice for the lock screen, with
// the remaining time, or "" when no lockout is running.
func (state *State) appLockLockedOutText() string {
	p := state.appProtection
	if p == nil {
		return ""
	}
	if remaining := p.record.lockoutRemaining(state.options.WallNow()); remaining > 0 {
		return fmt.Sprintf(state.messages.AppLock.LockedOutFmt, formatLockoutRemaining(state.messages.AppLock, remaining))
	}
	return ""
}
