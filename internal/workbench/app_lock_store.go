package workbench

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

const (
	lockVerifierVersion  = 1
	lockSaltBytes        = 16
	lockHashBytes        = 32
	lockPasswordMinRunes = 6
	lockPasswordMaxBytes = 1024
)

var (
	errLockWrongPassword    = errors.New("incorrect application lock password")
	errLockCorrupt          = errors.New("invalid application lock settings")
	errLockPasswordTooShort = errors.New("application lock password is too short")
	errLockPasswordTooLong  = errors.New("application lock password is too long")
	// errAppLockLockedOut reports a password check refused while the
	// failure lockout is still running. It says nothing about whether
	// the password was right: the check never runs while locked out.
	errAppLockLockedOut = errors.New("application lock is temporarily locked out after too many incorrect passwords")
)

// lockoutThreshold is how many consecutive wrong passwords lock the
// application out; lockoutDurations is the lockout for the 1st, 2nd,
// 3rd and 4th lockout, with the last value capping every later one.
const lockoutThreshold = 5

var lockoutDurations = [...]time.Duration{
	time.Minute, 5 * time.Minute, 15 * time.Minute, 30 * time.Minute, time.Hour,
}

// lockoutDurationFor is the lockout of the nth lockout (1-based): the
// first is a minute, then 5, 15 and 30 minutes, then an hour at most.
func lockoutDurationFor(lockCount int) time.Duration {
	if lockCount <= 1 {
		return lockoutDurations[0]
	}
	if lockCount > len(lockoutDurations) {
		return lockoutDurations[len(lockoutDurations)-1]
	}
	return lockoutDurations[lockCount-1]
}

type lockVerifier struct {
	Version  int    `json:"version"`
	Salt     []byte `json:"salt"`
	Hash     []byte `json:"hash"`
	SystemID string `json:"system_id,omitempty"`
	// FailedAttempts counts consecutive wrong passwords since the last
	// success; LockCount counts lockouts since the last success;
	// LockedUntilUnix is the lockout's end as a Unix timestamp, 0 when
	// no lockout is running. All three are zero in a file written
	// before they existed, which reads as "never failed".
	FailedAttempts  int   `json:"failed_attempts,omitempty"`
	LockCount       int   `json:"lock_count,omitempty"`
	LockedUntilUnix int64 `json:"locked_until_unix,omitempty"`
	// VaultLinked says the vault is protected with this application
	// lock password (wrapped or unlocked by an app-lock action), as
	// opposed to a legacy file still kept by its old master password.
	// The vault API cannot tell the two apart — both carry a KDF — so
	// this flag is what keeps the legacy conversion prompt
	// (showNextPrompt) from firing for a vault the app lock already
	// owns. False in a file written before it existed: legacy until
	// proven otherwise.
	VaultLinked bool `json:"vault_linked,omitempty"`
}

// lockoutRemaining is how long the lockout still has to run, or 0 when
// none is running.
func (r lockVerifier) lockoutRemaining(now time.Time) time.Duration {
	if r.LockedUntilUnix == 0 {
		return 0
	}
	if remaining := time.Unix(r.LockedUntilUnix, 0).Sub(now); remaining > 0 {
		return remaining
	}
	return 0
}

func readLockVerifier(path string) (lockVerifier, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return lockVerifier{}, nil
	}
	if err != nil {
		return lockVerifier{}, err
	}
	defer file.Close()
	var record lockVerifier
	data, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil {
		return lockVerifier{}, err
	}
	if len(data) > 4096 || json.Unmarshal(data, &record) != nil || record.Version != lockVerifierVersion ||
		len(record.Salt) != lockSaltBytes || len(record.Hash) != lockHashBytes {
		return lockVerifier{}, errLockCorrupt
	}
	if record.SystemID != "" {
		raw, err := hex.DecodeString(record.SystemID)
		if err != nil || len(raw) != 16 || hex.EncodeToString(raw) != record.SystemID {
			return lockVerifier{}, errLockCorrupt
		}
	}
	return record, nil
}

func deriveLockHash(password string, salt []byte) []byte {
	passwordBytes := []byte(password)
	defer clear(passwordBytes)
	return argon2.IDKey(passwordBytes, salt, 3, 64*1024, 4, lockHashBytes)
}

func validateNewLockPassword(password string) error {
	if len(password) > lockPasswordMaxBytes {
		return errLockPasswordTooLong
	}
	if utf8.RuneCountInString(password) < lockPasswordMinRunes {
		return errLockPasswordTooShort
	}
	return nil
}

func newLockVerifier(password, systemID string) (lockVerifier, error) {
	if err := validateNewLockPassword(password); err != nil {
		return lockVerifier{}, err
	}
	salt := make([]byte, lockSaltBytes)
	if _, err := rand.Read(salt); err != nil {
		return lockVerifier{}, err
	}
	return lockVerifier{Version: lockVerifierVersion, Salt: salt, Hash: deriveLockHash(password, salt), SystemID: systemID}, nil
}

func (r lockVerifier) verify(password string) error {
	if len(password) > lockPasswordMaxBytes {
		return errLockPasswordTooLong
	}
	if r.Version != lockVerifierVersion {
		return errLockCorrupt
	}
	candidate := deriveLockHash(password, r.Salt)
	defer clear(candidate)
	if subtle.ConstantTimeCompare(candidate, r.Hash) != 1 {
		return errLockWrongPassword
	}
	return nil
}

func (r lockVerifier) save(path string) error {
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return writeFileAtomic(path, data)
}
