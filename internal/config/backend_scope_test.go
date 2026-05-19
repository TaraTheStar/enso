// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"path/filepath"
	"testing"
)

// TestBackendEnv_ProjectScopedOnly verifies the scoping rule: [backend]
// type/workspace are honored from the user config, but the per-backend
// environment sub-tables ([backend.podman|lima|egress]) set in the user
// config are stripped, while the same tables in the repo's
// .enso/config.toml are honored.
func TestBackendEnv_ProjectScopedOnly(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "xdg"))
	t.Setenv("HOME", filepath.Join(tmp, "home"))

	xdg := filepath.Join(tmp, "xdg", "enso")
	mustMkdir(t, xdg)
	// USER config: a legitimate selection (type/workspace) PLUS env
	// tables it must not be allowed to set globally.
	mustWrite(t, filepath.Join(xdg, "config.toml"), `
[providers.local]
endpoint = "http://x:1/v1"
model = "stub"

[backend]
type = "lima"
workspace = "overlay"

[backend.podman]
image = "USER-SHOULD-NOT-WIN"

[backend.lima]
init = ["user-init-should-be-ignored"]

[backend.egress]
allow = ["user.example.com"]
`)

	// PROJECT config (the repo's committed .enso/config.toml): the only
	// legitimate place for backend environment.
	proj := filepath.Join(tmp, ".enso")
	mustMkdir(t, proj)
	mustWrite(t, filepath.Join(proj, "config.toml"), `
[backend.podman]
image = "PROJECT-WINS"

[backend.lima]
init = ["apt-get install -y git"]
`)

	cfg, err := Load(tmp, "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Selection knobs from the user config are honored.
	if cfg.Backend.Type != "lima" {
		t.Errorf("Backend.Type = %q, want %q (user-level selection is allowed)", cfg.Backend.Type, "lima")
	}
	if cfg.Backend.Workspace != "overlay" {
		t.Errorf("Backend.Workspace = %q, want %q (user-level selection is allowed)", cfg.Backend.Workspace, "overlay")
	}

	// User-scoped backend env is stripped; project-scoped wins.
	if cfg.Backend.Podman.Image != "PROJECT-WINS" {
		t.Errorf("Backend.Podman.Image = %q, want %q (user env must be stripped, project honored)",
			cfg.Backend.Podman.Image, "PROJECT-WINS")
	}
	if got := cfg.Backend.Lima.Init; len(got) != 1 || got[0] != "apt-get install -y git" {
		t.Errorf("Backend.Lima.Init = %v, want [apt-get install -y git] (user init must not leak in)", got)
	}
	if len(cfg.Backend.Egress.Allow) != 0 {
		t.Errorf("Backend.Egress.Allow = %v, want empty (egress set only in user config must be stripped)",
			cfg.Backend.Egress.Allow)
	}
}

// TestStripBackendEnv_Unit covers the helper directly: env tables go,
// selection knobs stay, nil-safe when [backend] is absent.
func TestStripBackendEnv_Unit(t *testing.T) {
	layer := map[string]any{
		"backend": map[string]any{
			"type":      "podman",
			"workspace": "overlay",
			"podman":    map[string]any{"image": "x"},
			"lima":      map[string]any{"init": []any{"y"}},
			"egress":    map[string]any{"allow": []any{"z"}},
		},
	}
	stripped := stripBackendEnv(layer)
	if len(stripped) != 3 {
		t.Fatalf("stripped = %v, want 3 entries", stripped)
	}
	b := layer["backend"].(map[string]any)
	if b["type"] != "podman" || b["workspace"] != "overlay" {
		t.Errorf("selection knobs must survive: %v", b)
	}
	for _, k := range []string{"podman", "lima", "egress"} {
		if _, ok := b[k]; ok {
			t.Errorf("backend.%s should have been stripped", k)
		}
	}
	if got := stripBackendEnv(map[string]any{"providers": map[string]any{}}); got != nil {
		t.Errorf("no [backend] table → nil, got %v", got)
	}
}
