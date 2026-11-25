package i18n

// AppLockMessages holds application lock labels and errors.
type AppLockMessages struct {
	Protection, ChangeAction, SystemBeforePassword                             string
	WrongPassword, Corrupt, DevicePassword                                     string
	Lock, Title                                                                string
	KeepRunning, RestoreAfterUnlock                                            string
	UnlockToReview                                                             string
	Password, PasswordPlaceholder, UnlockPassword, UnlockSystem, Divider       string
	Working, Required, Mismatch, NewPassword, ConfirmPassword, CurrentPassword string
	SettingsTitle, SettingsDescription, DisabledDescription, ChangePassword    string
	SystemTitle, SystemDescription, SystemDisabled, SystemUnavailable          string
	SystemForeground, SystemPrompt, PasswordTooLong                            string
	EnableTitle, ChangeTitle, DisableTitle                                     string
	EnableDescription, ChangeDescription, DisableDescription                   string
	EnrollTitle, EnrollDescription                                             string
	Save, Cancel, Error, Minimum                                               string
	// LockedOutFmt takes the remaining lockout time, already formatted with
	// LockedMinutesFmt or LockedSecondsFmt.
	LockedOutFmt, LockedMinutesFmt, LockedSecondsFmt string
	// LockedNotice is the refusal shown when a sensitive action is attempted
	// while the application is locked.
	LockedNotice string
	// ForgotPassword is the lock screen's entry to the forgotten-password
	// flow; the rest is its inline confirmation.
	ForgotPassword, ForgotTitle, ForgotDescription, ForgotConfirm, ForgotDone, ForgotFailed string
	// IdleTitle/IdleDescription label the idle auto-lock choice row; a choice
	// of 0 minutes is IdleNever, other choices use IdleMinutesFmt.
	IdleTitle, IdleDescription, IdleNever, IdleMinutesFmt string
	// SystemVaultProtected explains why a system-unlock enrollment was
	// refused: the vault is protected with a password other than the
	// application lock password, so system unlock could never open it.
	SystemVaultProtected string
	// VaultSealedSystem is the notice after a system unlock left the vault
	// sealed: only a password unlock can open it.
	VaultSealedSystem string
	// VaultUnavailable is the notice when an app-lock action could not open
	// or re-wrap the vault: the application stays unlocked, the vault stays
	// sealed, and the user is pointed at Settings.
	VaultUnavailable string
	// VaultConvertPrompt is the notice when enabling the lock left a legacy
	// vault untouched: it still needs its old master password, once, in
	// Settings.
	VaultConvertPrompt string
	// VaultMigrated confirms the one-time migration that wrapped an
	// unprotected vault with the application lock password.
	VaultMigrated string
}
