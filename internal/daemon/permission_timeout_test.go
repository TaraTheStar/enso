// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !windows

package daemon

import (
	"testing"
	"time"

	"github.com/TaraTheStar/enso/internal/config"
)

func TestServer_PermissionTimeoutDefault(t *testing.T) {
	// Empty/zero config means the daemon falls back to
	// DefaultPermissionTimeout. A regression here would mean every
	// prompt uses the wrong budget — most painful on installs that
	// never opted in to a custom value.
	s := &Server{cfg: &config.Config{}}
	got := s.permissionTimeout()
	want := time.Duration(config.DefaultPermissionTimeout) * time.Second
	if got != want {
		t.Errorf("default permissionTimeout=%v, want %v", got, want)
	}
}

func TestServer_PermissionTimeoutCustom(t *testing.T) {
	s := &Server{cfg: &config.Config{
		Daemon: config.DaemonConfig{PermissionTimeout: 90},
	}}
	got := s.permissionTimeout()
	if got != 90*time.Second {
		t.Errorf("permissionTimeout=%v, want 90s", got)
	}
}

func TestServer_PermissionTimeoutNegativeFallsBackToDefault(t *testing.T) {
	// Defensive: a negative value in config (bad TOML edit) shouldn't
	// produce a never-deny side effect.
	s := &Server{cfg: &config.Config{
		Daemon: config.DaemonConfig{PermissionTimeout: -1},
	}}
	got := s.permissionTimeout()
	want := time.Duration(config.DefaultPermissionTimeout) * time.Second
	if got != want {
		t.Errorf("negative permissionTimeout=%v, want default %v", got, want)
	}
}

func TestServer_PermissionTimeoutNilCfg(t *testing.T) {
	// Defensive: zero-value Server (e.g. used in narrow unit tests)
	// must not panic dereferencing s.cfg.
	s := &Server{}
	got := s.permissionTimeout()
	want := time.Duration(config.DefaultPermissionTimeout) * time.Second
	if got != want {
		t.Errorf("nil-cfg permissionTimeout=%v, want default %v", got, want)
	}
}
