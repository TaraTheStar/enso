// SPDX-License-Identifier: AGPL-3.0-or-later

// Package paths centralises enso's XDG Base Directory layout so call sites
// don't have to re-derive ~/.config/enso, ~/.local/share/enso, etc. Each
// helper honours the matching XDG_* env var first and falls back to the
// spec-defined default under $HOME.
//
// Layout:
//
//	ConfigDir  → $XDG_CONFIG_HOME/enso  (else ~/.config/enso)
//	  user-editable: config.toml, theme.toml, ENSO.md,
//	  agents/, workflows/, skills/
//	DataDir    → $XDG_DATA_HOME/enso    (else ~/.local/share/enso)
//	  app-managed persistent data: enso.db, memory/
//	StateDir   → $XDG_STATE_HOME/enso   (else ~/.local/state/enso)
//	  logs and other state that survives restarts but isn't portable:
//	  enso.log, debug.log, trust.json, worktrees/
//	RuntimeDir → $XDG_RUNTIME_DIR/enso  (else StateDir, per spec fallback)
//	  ephemeral runtime files: daemon.sock, daemon.pid
//
// Project-local `<cwd>/.enso/` (parallel to `.git/`) is NOT covered here —
// it has different semantics and is referenced directly by call sites.
package paths

import (
	"fmt"
	"os"
	"path/filepath"
)

// ConfigDir returns the directory for user-editable configuration.
func ConfigDir() (string, error) {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "enso"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("get home dir: %w", err)
	}
	return filepath.Join(home, ".config", "enso"), nil
}

// DataDir returns the directory for app-managed persistent data.
func DataDir() (string, error) {
	if dir := os.Getenv("XDG_DATA_HOME"); dir != "" {
		return filepath.Join(dir, "enso"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("get home dir: %w", err)
	}
	return filepath.Join(home, ".local", "share", "enso"), nil
}

// StateDir returns the directory for logs and similar persistent state.
func StateDir() (string, error) {
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return filepath.Join(dir, "enso"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("get home dir: %w", err)
	}
	return filepath.Join(home, ".local", "state", "enso"), nil
}

// RuntimeDir returns the directory for ephemeral runtime files (sockets,
// pidfiles). When XDG_RUNTIME_DIR is unset (e.g. systems without
// pam_systemd, or non-interactive sessions), the XDG spec recommends
// falling back to a private directory; we use StateDir for that.
func RuntimeDir() (string, error) {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "enso"), nil
	}
	return StateDir()
}
