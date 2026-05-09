// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package bubble

import "errors"

// execIntoSession on Windows refuses cleanly — syscall.Exec doesn't
// exist there. Falling back to "spawn child + parent exit" would
// re-implement a Unix idiom for marginal benefit, so the session-
// switch hotkey isn't available on Windows.
func execIntoSession(sessionID string) error {
	return errors.New("session switch not supported on Windows; relaunch with --session " + sessionID)
}
