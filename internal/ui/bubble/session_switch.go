// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !windows

package bubble

import (
	"fmt"
	"os"
	"strings"
	"syscall"
)

// execIntoSession replaces the running process with the same binary,
// substituting `--session <id>` (or appending it). All other CLI args
// are preserved. Called after the tea.Program has shut down so the new
// process inherits a clean terminal. Never returns on success —
// syscall.Exec is a process image replacement.
func execIntoSession(sessionID string) error {
	args := buildSwitchArgs(os.Args, sessionID)
	if err := syscall.Exec(os.Args[0], args, os.Environ()); err != nil {
		return fmt.Errorf("exec %s: %w", os.Args[0], err)
	}
	return nil // unreachable
}

// buildSwitchArgs returns argv for the re-exec: original args with
// `--session ...` / `--session=...` / `--continue` / `--resume ...` /
// `--resume=...` removed, then `--session <id>` appended. Exposed for
// testing.
func buildSwitchArgs(orig []string, sessionID string) []string {
	out := make([]string, 0, len(orig)+2)
	out = append(out, orig[0])
	skipNext := false
	for _, a := range orig[1:] {
		if skipNext {
			skipNext = false
			continue
		}
		switch {
		case a == "--session", a == "--resume":
			skipNext = true
			continue
		case a == "--continue":
			continue
		case strings.HasPrefix(a, "--session="), strings.HasPrefix(a, "--resume="):
			continue
		}
		out = append(out, a)
	}
	out = append(out, "--session", sessionID)
	return out
}
