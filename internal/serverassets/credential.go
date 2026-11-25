package serverassets

// Vault naming for server-asset credentials. This package only ever stores
// these reference strings; the secret values live in the vault under the
// same names and must never be written to config.db, a log or an error.
//
// The name is derived from the asset's own UUID, so a server and its
// jump host each have distinct names even when they share a password:
//   - SSH password:            server-credential:<asset UUID>
//   - private-key passphrase:  server-passphrase:<asset UUID>
//
// This mirrors the existing vault conventions (credential:<id>,
// passphrase:<identity>) with a distinct prefix so server-asset secrets
// cannot collide with the workbench host credentials.

// PasswordCredentialName is the vault name for a server/jump host's SSH password.
func PasswordCredentialName(assetID string) string {
	return "server-credential:" + assetID
}

// KeyPassphraseName is the vault name for a server/jump host's key passphrase.
func KeyPassphraseName(assetID string) string {
	return "server-passphrase:" + assetID
}

// CredentialNameFor returns the vault name matching a server's auth method:
// the passphrase name for key auth, the password name otherwise.
func CredentialNameFor(srv Server) string {
	if srv.AuthMethod == AuthKey {
		return KeyPassphraseName(srv.ID)
	}
	return PasswordCredentialName(srv.ID)
}

// JumpHostCredentialNameFor returns the vault name matching a jump host's
// auth method.
func JumpHostCredentialNameFor(jump JumpHost) string {
	if jump.AuthMethod == AuthKey {
		return KeyPassphraseName(jump.ID)
	}
	return PasswordCredentialName(jump.ID)
}
