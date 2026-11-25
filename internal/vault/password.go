// Application-password protection for the vault.
//
// The data key can be wrapped with a password the user types, instead of (or
// rather than) living in the system's secret store. The wrapping reuses the
// legacy file format — Argon2id parameters plus a WrappedKey sealed with
// AES-256-GCM under a key derived from the password — so a protected file is
// still just a file with a KDF, and NeedsConversion reports it the same way.
// The difference from the legacy master password is intent: this is the
// current, supported way to protect a vault, and unlocking never migrates the
// file or the key anywhere else. The wrapped key stays wrapped until the user
// removes the password or clears the vault.
//
// No password, derived key, or data key is ever written to a log or an error:
// errors name only what failed.
package vault

import (
	"crypto/rand"
	"errors"
	"fmt"
)

// The Argon2id costs for wrapping the data key with an application password.
// They match the app-lock check the workbench already uses, so one password
// costs the same to verify wherever it is checked.
const (
	passwordKDFTime      uint32 = 3
	passwordKDFMemoryKiB uint32 = 64 * 1024 // 64 MiB
	passwordKDFThreads   uint8  = 4
	passwordSaltSize     int    = 16
)

// Errors for application-password protection; test with errors.Is.
var (
	// ErrEmptyPassword means the application password was empty. An empty
	// password would wrap the key under a key anyone can derive, so Wrap and
	// Change refuse it outright instead of treating it as a wrong password.
	// Unlock and Remove do not use it: with no password to check against they
	// simply fail to unwrap, which is ErrWrongPassword.
	ErrEmptyPassword = errors.New("vault: the application password must not be empty")
	// ErrNotPasswordProtected means ChangePassword or RemovePassword was asked
	// of a vault that has no application password. There is no old password to
	// verify, so neither operation has anything to do.
	ErrNotPasswordProtected = errors.New("vault: the vault is not protected by an application password")
)

// PasswordProtected reports whether the file carries a KDF: the data key is
// wrapped with an application password. It says nothing about whether the key
// is in memory; a protected vault still needs UnlockWithPassword after a
// restart.
func (vault *Vault) PasswordProtected() bool {
	vault.mu.Lock()
	defer vault.mu.Unlock()
	return vault.file.KDF != nil
}

// Unlocked reports whether the data key is in memory, so the vault can be
// read and written without asking for anything.
func (vault *Vault) Unlocked() bool {
	vault.mu.Lock()
	defer vault.mu.Unlock()
	return vault.key != nil
}

// wrapKey seals key under a key derived from password, with fresh Argon2id
// parameters. The format is the one unwrapKey reads: a random nonce prefixed
// to the sealed key, bound to wrapAssociated.
func wrapKey(key []byte, password string) (*kdfParams, []byte, error) {
	salt := make([]byte, passwordSaltSize)
	if _, err := rand.Read(salt); err != nil {
		return nil, nil, err
	}
	params := &kdfParams{Name: "argon2id", Time: passwordKDFTime, MemoryKiB: passwordKDFMemoryKiB, Threads: passwordKDFThreads, Salt: salt}
	kek := deriveKey(password, params)
	defer clear(kek)
	gcm, err := newGCM(kek)
	if err != nil {
		return nil, nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, err
	}
	return params, gcm.Seal(nonce, nonce, key, []byte(wrapAssociated)), nil
}

// WrapWithPassword wraps the data key in memory with password and writes the
// file in its protected form. The records are untouched.
//
// A vault with no key in memory and no records gets a fresh key first, so an
// empty vault can be protected before anything is saved. A vault with records
// whose key is not in memory takes it from the secret store instead; if that
// fails the vault is left alone. A locked protected vault cannot be wrapped
// again without its password: it reports ErrNeedsConversion, so call
// UnlockWithPassword first.
//
// Order matters, and it is the reverse of ConvertLegacy: the protected file
// is written first, and only once it is on disk is the copy of the data key
// in the secret store deleted — otherwise the password would be a decoration
// over a key anyone with store access can read. If deleting that copy fails,
// the file is restored to the protection it had before (unprotected, for a
// first wrap) and the error is returned, so the vault is never left half
// protected. A fresh key made for an empty vault is forgotten again on any
// failure, so a failed wrap leaves the vault exactly as it was.
func (vault *Vault) WrapWithPassword(password string) error {
	vault.mu.Lock()
	defer vault.mu.Unlock()
	if password == "" {
		return ErrEmptyPassword
	}
	if vault.file.KDF != nil && vault.key == nil {
		return ErrNeedsConversion
	}
	hadKey := vault.key != nil
	if !hadKey {
		if len(vault.file.Records) > 0 {
			if err := vault.prepare(false); err != nil {
				return err
			}
			if vault.key == nil {
				return ErrKeyLost
			}
		} else {
			// An empty vault: reuse a key the store already holds, if any.
			// A store that answers nothing — no store, no key, a refusal —
			// is not a failure here: the password is about to become the
			// only place the new key is kept under.
			_ = vault.prepare(false)
			if vault.key == nil {
				key := make([]byte, keySize)
				if _, err := rand.Read(key); err != nil {
					return err
				}
				vault.key = key
			}
		}
	}
	params, wrapped, err := wrapKey(vault.key, password)
	if err != nil {
		if !hadKey {
			clear(vault.key)
			vault.key = nil
		}
		return err
	}
	oldKDF, oldWrapped := vault.file.KDF, vault.file.WrappedKey
	records := vault.file.Records
	if err := vault.writeWithProtection(records, params, wrapped); err != nil {
		if !hadKey {
			clear(vault.key)
			vault.key = nil
		}
		return err
	}
	if vault.keys != nil {
		if err := vault.keys.Delete(KeyService, KeyAccount); err != nil {
			if restoreErr := vault.writeWithProtection(records, oldKDF, oldWrapped); restoreErr != nil {
				return errors.Join(err, fmt.Errorf("%w: %v", ErrRollbackFailed, restoreErr))
			}
			if !hadKey {
				clear(vault.key)
				vault.key = nil
			}
			return err
		}
	}
	return nil
}

// UnlockWithPassword derives the data key from password and keeps it only in
// memory. It never writes the secret store and never rewrites the file, so
// the vault stays password-protected for the next start. A wrong password
// returns ErrWrongPassword and leaves the vault locked.
//
// A vault that is not password-protected returns nil without doing anything:
// its key, if any, is still taken from the secret store by the usual Prepare
// path. An already unlocked vault also returns nil.
func (vault *Vault) UnlockWithPassword(password string) error {
	vault.mu.Lock()
	defer vault.mu.Unlock()
	if vault.file.KDF == nil || vault.key != nil {
		return nil
	}
	key, err := unwrapKey(vault.file, password)
	if err != nil {
		return err
	}
	vault.key = key
	return nil
}

// ChangePassword re-wraps the data key under newPassword, with a fresh salt.
// It first verifies oldPassword against the file: a wrong old password
// returns ErrWrongPassword and the file is not touched. The records are kept
// as they are, and the vault stays unlocked afterwards.
//
// A stale copy of the data key in the secret store, if one is somehow still
// there, is deleted after the new file is written, with the same rollback as
// WrapWithPassword: if the deletion fails the file goes back to the old
// wrapping.
func (vault *Vault) ChangePassword(oldPassword, newPassword string) error {
	vault.mu.Lock()
	defer vault.mu.Unlock()
	if vault.file.KDF == nil {
		return ErrNotPasswordProtected
	}
	if newPassword == "" {
		return ErrEmptyPassword
	}
	key, err := unwrapKey(vault.file, oldPassword)
	if err != nil {
		return err
	}
	// The key the records are sealed under is the one already in memory, if
	// there is one; the freshly unwrapped copy is then a duplicate to discard.
	// Otherwise the unwrapped key is the one to keep.
	useKey := key
	if vault.key != nil {
		clear(key)
		useKey = vault.key
	}
	params, wrapped, err := wrapKey(useKey, newPassword)
	if err != nil {
		if vault.key == nil {
			clear(key)
		}
		return err
	}
	oldKDF, oldWrapped := vault.file.KDF, vault.file.WrappedKey
	records := vault.file.Records
	if err := vault.writeWithProtection(records, params, wrapped); err != nil {
		if vault.key == nil {
			clear(key)
		}
		return err
	}
	if vault.keys != nil {
		if err := vault.keys.Delete(KeyService, KeyAccount); err != nil {
			if restoreErr := vault.writeWithProtection(records, oldKDF, oldWrapped); restoreErr != nil {
				if vault.key == nil {
					clear(key)
				}
				return errors.Join(err, fmt.Errorf("%w: %v", ErrRollbackFailed, restoreErr))
			}
			if vault.key == nil {
				clear(key)
			}
			return err
		}
	}
	if vault.key == nil {
		vault.key = key
	}
	return nil
}

// RemovePassword verifies password, stores the data key back in the secret
// store, and rewrites the file without its password protection. It follows
// the ConvertLegacy order — store first, rewrite second — so a failure
// leaves the protected file as it was: a rewrite that fails also removes the
// key just stored, and a store that fails never reaches the rewrite. The
// vault stays unlocked afterwards, and the next start needs no password.
func (vault *Vault) RemovePassword(password string) error {
	vault.mu.Lock()
	defer vault.mu.Unlock()
	if vault.file.KDF == nil {
		return ErrNotPasswordProtected
	}
	if vault.keys == nil {
		return ErrUnavailable
	}
	key, err := unwrapKey(vault.file, password)
	if err != nil {
		return err
	}
	if err := vault.keys.Store(KeyService, KeyAccount, key); err != nil {
		clear(key)
		if errors.Is(err, ErrKeystoreUnavailable) {
			return ErrUnavailable
		}
		return err
	}
	if err := vault.writeWithProtection(vault.file.Records, nil, nil); err != nil {
		_ = vault.keys.Delete(KeyService, KeyAccount)
		if vault.key == nil {
			clear(key)
		}
		return err
	}
	if vault.key == nil {
		vault.key = key
	} else {
		clear(key)
	}
	return nil
}

// Seal forgets the data key in memory, as a restart does. It touches neither
// the file nor the secret store: a protected vault still needs its password
// afterwards, and an unprotected one still reads its key from the store.
func (vault *Vault) Seal() {
	vault.mu.Lock()
	defer vault.mu.Unlock()
	clear(vault.key)
	vault.key = nil
}
