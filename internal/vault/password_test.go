package vault

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func mustSet(t *testing.T, vault *Vault, name, value string) {
	t.Helper()
	if err := vault.Set(name, value); err != nil {
		t.Fatal(err)
	}
}

func mustGet(t *testing.T, vault *Vault, name string) string {
	t.Helper()
	got, found, err := vault.Get(name)
	if err != nil || !found {
		t.Fatalf("Get(%q) = %q, %v, %v", name, got, found, err)
	}
	return got
}

func TestWrapUnlockAndSetKeepsProtection(t *testing.T) {
	keys := &memoryKeystore{}
	vault, path := openVault(t, keys)
	mustSet(t, vault, "a", "alpha")
	mustSet(t, vault, "b", "bee")
	if err := vault.WrapWithPassword("correct horse"); err != nil {
		t.Fatal(err)
	}
	if !vault.PasswordProtected() || !vault.Unlocked() {
		t.Fatal("after Wrap: protected and unlocked")
	}
	// The keystore must no longer hold a copy of the data key.
	if keys.has() {
		t.Fatal("the data key is still in the secret store after Wrap")
	}
	if _, err := keys.Load(KeyService, KeyAccount); !errors.Is(err, ErrKeystoreNotFound) {
		t.Fatalf("Load after Wrap = %v, want ErrKeystoreNotFound", err)
	}

	again, err := Open(path, keys)
	if err != nil {
		t.Fatal(err)
	}
	if !again.PasswordProtected() || again.Unlocked() {
		t.Fatal("a reopened protected vault must say so and start locked")
	}
	if _, _, err := again.Get("a"); !errors.Is(err, ErrNeedsConversion) {
		t.Fatalf("Get on a locked vault = %v, want ErrNeedsConversion", err)
	}
	if err := again.UnlockWithPassword("wrong horse"); !errors.Is(err, ErrWrongPassword) {
		t.Fatalf("Unlock with a wrong password = %v, want ErrWrongPassword", err)
	}
	if again.Unlocked() {
		t.Fatal("a wrong password unlocked the vault")
	}
	if err := again.UnlockWithPassword("correct horse"); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, again, "a"); got != "alpha" {
		t.Fatalf("after Unlock: a = %q", got)
	}
	// A write after unlocking must not strip the protection.
	mustSet(t, again, "c", "sea")
	if got := mustGet(t, again, "c"); got != "sea" {
		t.Fatalf("c = %q", got)
	}

	reopened, err := Open(path, keys)
	if err != nil {
		t.Fatal(err)
	}
	if !reopened.PasswordProtected() {
		t.Fatal("a Set after unlocking stripped the password protection")
	}
	if err := reopened.UnlockWithPassword("correct horse"); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, reopened, "c"); got != "sea" {
		t.Fatalf("after reopening: c = %q", got)
	}
	if got := mustGet(t, reopened, "b"); got != "bee" {
		t.Fatalf("after reopening: b = %q", got)
	}
}

func TestUnlockTouchesNeitherStoreNorFile(t *testing.T) {
	keys := &memoryKeystore{}
	vault, path := openVault(t, keys)
	mustSet(t, vault, "a", "alpha")
	if err := vault.WrapWithPassword("correct horse"); err != nil {
		t.Fatal(err)
	}
	again, err := Open(path, keys)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := again.UnlockWithPassword("correct horse"); err != nil {
		t.Fatal(err)
	}
	if keys.has() {
		t.Fatal("Unlock put the data key in the secret store")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("Unlock rewrote the file")
	}
}

func TestChangePassword(t *testing.T) {
	keys := &memoryKeystore{}
	vault, path := openVault(t, keys)
	mustSet(t, vault, "a", "alpha")
	mustSet(t, vault, "b", "bee")
	if err := vault.WrapWithPassword("old horse"); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := vault.ChangePassword("wrong horse", "new horse"); !errors.Is(err, ErrWrongPassword) {
		t.Fatalf("ChangePassword with a wrong old password = %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("a failed ChangePassword changed the file")
	}
	if err := vault.ChangePassword("old horse", "new horse"); err != nil {
		t.Fatal(err)
	}
	if !vault.PasswordProtected() || !vault.Unlocked() {
		t.Fatal("after ChangePassword: protected and unlocked")
	}
	if keys.has() {
		t.Fatal("ChangePassword left a data key in the secret store")
	}

	again, err := Open(path, keys)
	if err != nil {
		t.Fatal(err)
	}
	if err := again.UnlockWithPassword("old horse"); !errors.Is(err, ErrWrongPassword) {
		t.Fatalf("the old password still opens the vault: %v", err)
	}
	if err := again.UnlockWithPassword("new horse"); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, again, "a"); got != "alpha" {
		t.Fatalf("after ChangePassword: a = %q", got)
	}
	if got := mustGet(t, again, "b"); got != "bee" {
		t.Fatalf("after ChangePassword: b = %q", got)
	}
}

func TestRemovePassword(t *testing.T) {
	keys := &memoryKeystore{}
	vault, path := openVault(t, keys)
	mustSet(t, vault, "a", "alpha")
	if err := vault.WrapWithPassword("correct horse"); err != nil {
		t.Fatal(err)
	}
	if keys.has() {
		t.Fatal("the data key is still in the secret store after Wrap")
	}
	if err := vault.RemovePassword("wrong horse"); !errors.Is(err, ErrWrongPassword) {
		t.Fatalf("RemovePassword with a wrong password = %v", err)
	}
	if !vault.PasswordProtected() {
		t.Fatal("a failed RemovePassword stripped the protection")
	}
	if err := vault.RemovePassword("correct horse"); err != nil {
		t.Fatal(err)
	}
	if vault.PasswordProtected() {
		t.Fatal("RemovePassword left the vault protected")
	}
	if !keys.has() {
		t.Fatal("RemovePassword did not put the data key back in the secret store")
	}

	again, err := Open(path, keys)
	if err != nil {
		t.Fatal(err)
	}
	if again.PasswordProtected() {
		t.Fatal("the reopened vault is still protected")
	}
	// No password needed: Prepare through the store is enough.
	if got := mustGet(t, again, "a"); got != "alpha" {
		t.Fatalf("after RemovePassword: a = %q", got)
	}
	mustSet(t, again, "b", "bee")
	if got := mustGet(t, again, "b"); got != "bee" {
		t.Fatalf("b = %q", got)
	}
}

func TestSealRequiresUnlockAgain(t *testing.T) {
	keys := &memoryKeystore{}
	vault, path := openVault(t, keys)
	mustSet(t, vault, "a", "alpha")
	if err := vault.WrapWithPassword("correct horse"); err != nil {
		t.Fatal(err)
	}
	vault.Seal()
	if vault.Unlocked() || !vault.PasswordProtected() {
		t.Fatal("after Seal: locked but still protected")
	}
	if _, _, err := vault.Get("a"); !errors.Is(err, ErrNeedsConversion) {
		t.Fatalf("Get after Seal = %v, want ErrNeedsConversion", err)
	}
	if err := vault.UnlockWithPassword("correct horse"); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, vault, "a"); got != "alpha" {
		t.Fatalf("after re-unlocking: a = %q", got)
	}
	// The file itself is unchanged by sealing, so a fresh open still needs
	// the password.
	again, err := Open(path, keys)
	if err != nil {
		t.Fatal(err)
	}
	if !again.PasswordProtected() || again.Unlocked() {
		t.Fatal("a reopened vault must still be protected and locked")
	}
}

func TestWrapAnEmptyVault(t *testing.T) {
	keys := &memoryKeystore{}
	vault, path := openVault(t, keys)
	if err := vault.WrapWithPassword("correct horse"); err != nil {
		t.Fatal(err)
	}
	if !vault.PasswordProtected() || !vault.Unlocked() {
		t.Fatal("after Wrap of an empty vault: protected and unlocked")
	}
	mustSet(t, vault, "a", "alpha")
	if keys.has() {
		t.Fatal("the data key is in the secret store after wrapping an empty vault")
	}

	again, err := Open(path, keys)
	if err != nil {
		t.Fatal(err)
	}
	if err := again.UnlockWithPassword("correct horse"); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, again, "a"); got != "alpha" {
		t.Fatalf("a = %q", got)
	}
	mustSet(t, again, "b", "bee")
	if got := mustGet(t, again, "b"); got != "bee" {
		t.Fatalf("b = %q", got)
	}
}

func TestWrapAnEmptyVaultWithoutAStore(t *testing.T) {
	// With no secret store at all, the password is the only place the key is
	// kept under, so wrapping an empty vault must still work.
	path := filepath.Join(t.TempDir(), "vault.json")
	vault, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := vault.WrapWithPassword("correct horse"); err != nil {
		t.Fatal(err)
	}
	mustSet(t, vault, "a", "alpha")

	again, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := again.UnlockWithPassword("correct horse"); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, again, "a"); got != "alpha" {
		t.Fatalf("a = %q", got)
	}
}

func TestPasswordErrors(t *testing.T) {
	keys := &memoryKeystore{}
	vault, _ := openVault(t, keys)
	mustSet(t, vault, "a", "alpha")

	if err := vault.WrapWithPassword(""); !errors.Is(err, ErrEmptyPassword) {
		t.Fatalf("Wrap with an empty password = %v, want ErrEmptyPassword", err)
	}
	if vault.PasswordProtected() {
		t.Fatal("an empty password protected the vault")
	}
	// Unlock on an unprotected vault is a no-op.
	if err := vault.UnlockWithPassword("anything"); err != nil {
		t.Fatalf("Unlock on an unprotected vault = %v", err)
	}
	if err := vault.ChangePassword("old", "new"); !errors.Is(err, ErrNotPasswordProtected) {
		t.Fatalf("ChangePassword on an unprotected vault = %v", err)
	}
	if err := vault.RemovePassword("old"); !errors.Is(err, ErrNotPasswordProtected) {
		t.Fatalf("RemovePassword on an unprotected vault = %v", err)
	}

	// A locked protected vault cannot be wrapped again without its password.
	if err := vault.WrapWithPassword("correct horse"); err != nil {
		t.Fatal(err)
	}
	vault.Seal()
	if err := vault.WrapWithPassword("another horse"); !errors.Is(err, ErrNeedsConversion) {
		t.Fatalf("Wrap on a locked vault = %v, want ErrNeedsConversion", err)
	}
	if err := vault.UnlockWithPassword("correct horse"); err != nil {
		t.Fatal(err)
	}
	// Unlocking an already unlocked vault is a no-op, even with a wrong password.
	if err := vault.UnlockWithPassword("wrong horse"); err != nil {
		t.Fatalf("Unlock on an unlocked vault = %v", err)
	}
	// An empty new password is refused, and the old one still works.
	if err := vault.ChangePassword("correct horse", ""); !errors.Is(err, ErrEmptyPassword) {
		t.Fatalf("ChangePassword to an empty password = %v", err)
	}
	vault.Seal()
	if err := vault.UnlockWithPassword("correct horse"); err != nil {
		t.Fatalf("the old password stopped working after a refused change: %v", err)
	}
}

func TestClearOnAProtectedVaultStartsOver(t *testing.T) {
	keys := &memoryKeystore{}
	vault, _ := openVault(t, keys)
	mustSet(t, vault, "a", "alpha")
	if err := vault.WrapWithPassword("correct horse"); err != nil {
		t.Fatal(err)
	}
	if err := vault.Clear(); err != nil {
		t.Fatal(err)
	}
	if vault.PasswordProtected() || vault.Unlocked() {
		t.Fatal("after Clear: still protected or unlocked")
	}
	if keys.has() {
		t.Fatal("Clear left a data key in the secret store")
	}
	// A forgotten password is escaped by Clear: the vault rebuilds itself
	// through the secret store, with no password.
	mustSet(t, vault, "b", "bee")
	if got := mustGet(t, vault, "b"); got != "bee" {
		t.Fatalf("b = %q", got)
	}
	if _, found, _ := vault.Get("a"); found {
		t.Fatal("a secret survived Clear")
	}
}
