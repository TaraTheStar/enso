// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"path/filepath"
	"testing"
)

func TestLoad_DaemonPermissionTimeout(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "xdg"))
	t.Setenv("HOME", filepath.Join(tmp, "home"))

	xdg := filepath.Join(tmp, "xdg", "enso")
	mustMkdir(t, xdg)
	mustWrite(t, filepath.Join(xdg, "config.toml"), `
[providers.local]
endpoint = "http://x:1/v1"
model = "stub"

[daemon]
permission_timeout = 120
`)

	cfg, err := Load(tmp, "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Daemon.PermissionTimeout != 120 {
		t.Errorf("PermissionTimeout=%d, want 120", cfg.Daemon.PermissionTimeout)
	}
}

func TestLoad_DaemonPermissionTimeoutZeroByDefault(t *testing.T) {
	// Unset means 0 — the daemon side substitutes DefaultPermissionTimeout.
	// Tested here to lock in the convention so a future TOML change to
	// "default to 60 directly" doesn't silently bypass the daemon's
	// fallback path.
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "xdg"))
	t.Setenv("HOME", filepath.Join(tmp, "home"))

	xdg := filepath.Join(tmp, "xdg", "enso")
	mustMkdir(t, xdg)
	mustWrite(t, filepath.Join(xdg, "config.toml"), `
[providers.local]
endpoint = "http://x:1/v1"
model = "stub"
`)

	cfg, err := Load(tmp, "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Daemon.PermissionTimeout != 0 {
		t.Errorf("PermissionTimeout=%d, want 0 (unset)", cfg.Daemon.PermissionTimeout)
	}
	if DefaultPermissionTimeout != 60 {
		t.Errorf("DefaultPermissionTimeout=%d, want 60", DefaultPermissionTimeout)
	}
}
