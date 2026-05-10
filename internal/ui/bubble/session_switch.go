// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !windows

package bubble

import (
	"fmt"
	"os"
	"os/exec"
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
	bin, err := resolveSelfPath(os.Args[0])
	if err != nil {
		return fmt.Errorf("resolve self path: %w", err)
	}
	if err := syscall.Exec(bin, args, os.Environ()); err != nil {
		return fmt.Errorf("exec %s: %w", bin, err)
	}
	return nil // unreachable
}

// resolveSelfPath returns an absolute path to the currently-running
// binary suitable for syscall.Exec, which (unlike the shell) does not
// consult PATH. os.Executable is preferred because it reflects the
// actual binary even if argv[0] was rewritten; we fall back to
// exec.LookPath(argv[0]) and finally argv[0] itself.
func resolveSelfPath(argv0 string) (string, error) {
	if p, err := os.Executable(); err == nil && p != "" {
		return p, nil
	}
	if p, err := exec.LookPath(argv0); err == nil && p != "" {
		return p, nil
	}
	if argv0 == "" {
		return "", fmt.Errorf("empty argv[0] and os.Executable unavailable")
	}
	return argv0, nil
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
