// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package main

import "errors"

// spawnDetachedDaemon mirrors the POSIX version's signature so the
// shared daemonCmd compiles on Windows. The daemon itself isn't
// supported here; this returns the same error.
func spawnDetachedDaemon() error {
	return errors.New("enso daemon not supported on Windows; use enso tui or enso run (or run via WSL)")
}
