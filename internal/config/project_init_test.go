// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

// TestDetectLang_MarkerFiles verifies the auto-detect heuristic for
// each supported language and the generic fallback. Empty workdir =>
// generic, since detection has nothing to read.
func TestDetectLang_MarkerFiles(t *testing.T) {
	cases := []struct {
		name     string
		markers  []string // files to create
		wantSlug string
	}{
		{"go", []string{"go.mod"}, "go"},
		{"node", []string{"package.json"}, "node"},
		{"python_pyproject", []string{"pyproject.toml"}, "python"},
		{"python_requirements", []string{"requirements.txt"}, "python"},
		{"rust", []string{"Cargo.toml"}, "rust"},
		{"generic_empty", nil, "generic"},
		// go takes precedence over node when both markers exist —
		// matches the order of langPresets.
		{"go_wins_over_node", []string{"go.mod", "package.json"}, "go"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, m := range tc.markers {
				mustWrite(t, filepath.Join(dir, m), "")
			}
			if got := detectLang(dir); got != tc.wantSlug {
				t.Errorf("detectLang(%q) = %q, want %q", dir, got, tc.wantSlug)
			}
		})
	}

	// Empty workdir short-circuits to generic — no filesystem touch.
	if got := detectLang(""); got != "generic" {
		t.Errorf("detectLang(\"\") = %q, want \"generic\"", got)
	}
}

// TestRunProjectInit_NonInteractive_PerLanguage walks every preset
// and checks that the rendered TOML reflects the language's apk
// package set and egress allowlist. Locks in the per-language table.
func TestRunProjectInit_NonInteractive_PerLanguage(t *testing.T) {
	for _, preset := range langPresets {
		t.Run(preset.Slug, func(t *testing.T) {
			var out bytes.Buffer
			res, body, err := RunProjectInit(nil, &out, ProjectInitOptions{
				Lang:        preset.Slug,
				Backend:     "podman",
				Interactive: false,
			})
			if err != nil {
				t.Fatalf("RunProjectInit: %v", err)
			}
			if res.Lang != preset.Slug {
				t.Errorf("Lang = %q, want %q", res.Lang, preset.Slug)
			}
			if res.Backend != "podman" {
				t.Errorf("Backend = %q, want podman", res.Backend)
			}
			// apk line must contain every package in the preset.
			for _, pkg := range preset.AlpinePackages {
				if !strings.Contains(body, pkg) {
					t.Errorf("body missing apk package %q\n%s", pkg, body)
				}
			}
			// Egress allowlist must contain every host.
			for _, host := range preset.EgressAllow {
				if !strings.Contains(body, host) {
					t.Errorf("body missing egress host %q\n%s", host, body)
				}
			}
			// podman active, lima commented out (one-line prefix).
			if !strings.Contains(body, "[backend.podman]") {
				t.Errorf("body missing uncommented [backend.podman]")
			}
			if !strings.Contains(body, "# [backend.lima]") {
				t.Errorf("body missing commented [backend.lima] hint")
			}
		})
	}
}

// TestRunProjectInit_BackendLima inverts the comment/uncomment: lima
// becomes active, podman becomes the hint block.
func TestRunProjectInit_BackendLima(t *testing.T) {
	var out bytes.Buffer
	res, body, _ := RunProjectInit(nil, &out, ProjectInitOptions{
		Lang:        "go",
		Backend:     "lima",
		Interactive: false,
	})
	if res.Backend != "lima" {
		t.Fatalf("Backend = %q, want lima", res.Backend)
	}
	if !strings.Contains(body, "[backend.lima]") {
		t.Errorf("body missing uncommented [backend.lima]")
	}
	if !strings.Contains(body, "# [backend.podman]") {
		t.Errorf("body missing commented [backend.podman] hint")
	}
}

// TestRunProjectInit_DetectFromWorkdir confirms that an unset Lang
// falls back to filesystem detection.
func TestRunProjectInit_DetectFromWorkdir(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "Cargo.toml"), "")
	var out bytes.Buffer
	res, body, err := RunProjectInit(nil, &out, ProjectInitOptions{
		Workdir:     dir,
		Interactive: false,
	})
	if err != nil {
		t.Fatalf("RunProjectInit: %v", err)
	}
	if res.Lang != "rust" {
		t.Errorf("Lang = %q, want rust (detected from Cargo.toml)", res.Lang)
	}
	if !strings.Contains(body, "cargo") {
		t.Errorf("body missing cargo apk package")
	}
}

// TestRunProjectInit_LoadsClean is the load-bearing invariant test:
// the rendered output, when placed at <dir>/.enso/config.toml and
// loaded with Load(dir, ""), must not emit a scrub warning — i.e. it
// must contain ONLY project-scoped backend subtables and no
// [backend] type / workspace keys.
func TestRunProjectInit_LoadsClean(t *testing.T) {
	for _, preset := range langPresets {
		t.Run(preset.Slug, func(t *testing.T) {
			dir := t.TempDir()

			// Give Load a valid provider so it doesn't bail on
			// missing required fields downstream of the merge.
			mustMkdir(t, filepath.Join(dir, "xdg", "enso"))
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "xdg"))
			t.Setenv("HOME", filepath.Join(dir, "home"))
			mustWrite(t, filepath.Join(dir, "xdg", "enso", "config.toml"), `
[providers.stub]
endpoint = "http://x:1/v1"
model    = "stub"
`)

			_, body, err := RunProjectInit(nil, &bytes.Buffer{}, ProjectInitOptions{
				Lang:    preset.Slug,
				Backend: "podman",
			})
			if err != nil {
				t.Fatalf("RunProjectInit: %v", err)
			}
			projDir := filepath.Join(dir, ".enso")
			mustMkdir(t, projDir)
			mustWrite(t, filepath.Join(projDir, "config.toml"), body)

			// Capture slog warnings during Load. The scrub helper
			// fires a warn when [backend.podman|lima|egress] appear
			// in a non-project-scoped layer; the project file IS
			// project-scoped so no warn should fire from our output.
			warnings := captureWarnings(t, func() {
				if _, err := Load(dir, ""); err != nil {
					t.Fatalf("Load: %v", err)
				}
			})
			for _, w := range warnings {
				if strings.Contains(w, "backend environment is project-scoped") {
					t.Errorf("scrub warning fired for project-scoped file: %s", w)
				}
			}

			// Sanity-check the round-trip: parsed TOML carries the
			// preset's apk line into Backend.Podman.Init.
			cfg, err := Load(dir, "")
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if len(cfg.Backend.Podman.Init) != 1 {
				t.Fatalf("Backend.Podman.Init = %v, want one apk line", cfg.Backend.Podman.Init)
			}
			for _, pkg := range preset.AlpinePackages {
				if !strings.Contains(cfg.Backend.Podman.Init[0], pkg) {
					t.Errorf("parsed init missing %q: %q", pkg, cfg.Backend.Podman.Init[0])
				}
			}
		})
	}
}

// TestRunProjectInit_OutputParsesAsTOML guards against malformed
// output from any preset. A failed parse here means buildProjectTOML
// produced a syntactic mess.
func TestRunProjectInit_OutputParsesAsTOML(t *testing.T) {
	for _, preset := range langPresets {
		t.Run(preset.Slug, func(t *testing.T) {
			_, body, _ := RunProjectInit(nil, &bytes.Buffer{}, ProjectInitOptions{
				Lang:    preset.Slug,
				Backend: "podman",
			})
			var v map[string]any
			if err := toml.Unmarshal([]byte(body), &v); err != nil {
				t.Fatalf("rendered body doesn't parse as TOML: %v\n%s", err, body)
			}
		})
	}
}

// TestRunProjectInit_Interactive_ScriptedChoices feeds newline-
// delimited choices through the prompter and checks the result
// reflects them.
func TestRunProjectInit_Interactive_ScriptedChoices(t *testing.T) {
	// Choice 3 = Python; Choice 2 = lima.
	in := strings.NewReader("3\n2\n")
	var out bytes.Buffer
	res, body, err := RunProjectInit(in, &out, ProjectInitOptions{
		Interactive: true,
	})
	if err != nil {
		t.Fatalf("RunProjectInit: %v", err)
	}
	if res.Lang != "python" {
		t.Errorf("Lang = %q, want python", res.Lang)
	}
	if res.Backend != "lima" {
		t.Errorf("Backend = %q, want lima", res.Backend)
	}
	if !strings.Contains(body, "py3-pip") {
		t.Errorf("body missing python apk package")
	}
	if !strings.Contains(body, "pypi.org") {
		t.Errorf("body missing pypi egress host")
	}
}

// captureWarnings installs a slog handler that records WARN-level
// messages emitted during fn, restoring the prior default after.
func captureWarnings(t *testing.T, fn func()) []string {
	t.Helper()
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })

	var buf bytes.Buffer
	h := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})
	slog.SetDefault(slog.New(h))

	_ = context.Background() // keep import live if future changes need it
	fn()

	var out []string
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		out = append(out, line)
	}
	return out
}
