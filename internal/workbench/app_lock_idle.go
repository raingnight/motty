package workbench

import (
	"fmt"
	"time"

	"motty/internal/i18n"
)

// Idle auto-lock (phase 6): the application locks itself after a period
// without keyboard or pointer activity, when an application lock is set.
// Activity is one atomic timestamp, written at the State.Pointer and
// State.Key entries and read by checkIdleLock; the window thread is the
// only place that ever locks. A delayed wake (options.PostAfter, the same
// mechanism scheduleWake uses) re-checks once a minute, so no goroutine
// is owned here and there is nothing for State.Close to leak: stopping
// the pending wake is enough. Headless tests have no PostAfter, so they
// drive checkIdleLock directly with a fake clock.

// defaultAppLockIdleMinutes is the idle timeout a fresh install gets;
// 0 means never lock automatically.
const defaultAppLockIdleMinutes = 15

// appLockIdleChoices are the timeouts the settings row offers, in minutes;
// 0 is "never".
var appLockIdleChoices = [...]int{5, 15, 30, 60, 0}

// normalizeAppLockIdleMinutes makes a saved timeout a usable one: one of
// the offered values, or the default for anything else (including a file
// that predates the setting, which carries 0 without meaning "never").
func normalizeAppLockIdleMinutes(value int) int {
	switch value {
	case 0, 5, 15, 30, 60:
		return value
	default:
		return defaultAppLockIdleMinutes
	}
}

// restoreAppLockIdleMinutes reads the saved timeout: "never" is stored
// apart (AppLockIdleOff), because a bare 0 also means "never saved".
func restoreAppLockIdleMinutes(saved savedState) int {
	if saved.AppLockIdleOff {
		return 0
	}
	if saved.AppLockIdleMinutes == 0 {
		return defaultAppLockIdleMinutes
	}
	return normalizeAppLockIdleMinutes(saved.AppLockIdleMinutes)
}

// noteActivity records user activity now; noteActivityAt records it at a
// given time, for tests.
func (state *State) noteActivity() { state.noteActivityAt(time.Now()) }

func (state *State) noteActivityAt(at time.Time) {
	state.lastActivity.Store(at.UnixNano())
}

// idleCheckDelay is how often the delayed wake re-checks: a minute, or
// the injected interval a test sets.
func (state *State) idleCheckDelay() time.Duration {
	if state.idleCheckInterval > 0 {
		return state.idleCheckInterval
	}
	return time.Minute
}

// checkIdleLock locks the application when it has been idle longer than
// the configured timeout. It reports whether it locked. Only the window
// thread (Wake, the delayed wake, or a test driving it directly) calls
// it; lockApplication itself is the same sealProfile path a manual lock
// takes. A first call with no activity recorded yet starts the clock
// instead of locking at once.
func (state *State) checkIdleLock(now time.Time) bool {
	if !state.appLockEnabled() || state.appLocked() {
		return false
	}
	minutes := state.appLockIdleMinutes
	if minutes <= 0 {
		return false
	}
	last := state.lastActivity.Load()
	if last == 0 {
		state.noteActivityAt(now)
		return false
	}
	if now.Sub(time.Unix(0, last)) < time.Duration(minutes)*time.Minute {
		return false
	}
	state.lockApplication()
	return true
}

// armIdleLockWake asks the window's event loop to re-check the idle lock
// after idleCheckDelay. It arms at most one wake at a time and only while
// an idle lock could fire; the wake's callback re-arms itself until the
// lock fires or the setting stops applying. With no PostAfter (a headless
// test) nothing is armed and checkIdleLock is the only path.
func (state *State) armIdleLockWake() {
	if state.idleWakeStop != nil || state.options.PostAfter == nil || state.closed || state.callbacksClosed.Load() {
		return
	}
	if !state.appLockEnabled() || state.appLocked() || state.appLockIdleMinutes <= 0 {
		return
	}
	state.idleWakeStop = state.options.PostAfter(state.idleCheckDelay(), func() {
		state.idleWakeStop = nil
		if state.closed {
			return
		}
		state.checkIdleLock(time.Now())
		state.armIdleLockWake()
		if state.options.Invalidate != nil {
			state.options.Invalidate()
		}
	})
}

// stopIdleLockWake cancels a pending idle re-check. State.Close calls it.
func (state *State) stopIdleLockWake() {
	if state.idleWakeStop != nil {
		state.idleWakeStop()
		state.idleWakeStop = nil
	}
}

// formatLockoutRemaining renders a lockout remainder for LockedOutFmt:
// whole minutes, rounded up, or seconds below a minute.
func formatLockoutRemaining(m i18n.AppLockMessages, remaining time.Duration) string {
	if remaining >= time.Minute {
		minutes := int(remaining.Minutes())
		if remaining > time.Duration(minutes)*time.Minute {
			minutes++
		}
		return fmt.Sprintf(m.LockedMinutesFmt, minutes)
	}
	seconds := int(remaining.Seconds())
	if seconds < 1 {
		seconds = 1
	}
	return fmt.Sprintf(m.LockedSecondsFmt, seconds)
}
