// SPDX-License-Identifier: AGPL-3.0-or-later

package instructions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolveForPath_DeepENSO covers the headline case: a file in
// foo/bar/baz/x.go pulls in foo/bar/ENSO.md, but NOT foo/ENSO.md (out
// of subdir scope: caller decides) nor the root cwd's ENSO.md (the
// system prompt already has that).
func TestResolveForPath_DeepENSO(t *testing.T) {
	tmp := t.TempDir()

	// Root cwd has its own ENSO.md — must NOT be returned.
	if err := os.WriteFile(filepath.Join(tmp, "ENSO.md"), []byte("root rules"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A deep subdir's ENSO.md — MUST be returned.
	deep := filepath.Join(tmp, "foo", "bar")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	deepFile := filepath.Join(deep, "ENSO.md")
	if err := os.WriteFile(deepFile, []byte("deep rules"), 0o644); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(deep, "baz", "x.go")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("// file"), 0o644); err != nil {
		t.Fatal(err)
	}

	layers, err := ResolveForPath(target, tmp)
	if err != nil {
		t.Fatalf("ResolveForPath: %v", err)
	}
	if len(layers) != 1 {
		t.Fatalf("got %d layers, want 1: %+v", len(layers), layers)
	}
	if layers[0].Name != deepFile {
		t.Errorf("layer Name: got %q, want %q", layers[0].Name, deepFile)
	}
	if !strings.Contains(layers[0].Body, "deep rules") {
		t.Errorf("body missing expected content: %q", layers[0].Body)
	}
}

// TestResolveForPath_MixedENSO_AND_AGENTS confirms both file types
// are collected and ordered top-down (root-adjacent first, deepest
// last).
func TestResolveForPath_MixedENSO_AND_AGENTS(t *testing.T) {
	tmp := t.TempDir()
	mid := filepath.Join(tmp, "src")
	deep := filepath.Join(mid, "api")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	// src/ENSO.md and src/api/AGENTS.md
	if err := os.WriteFile(filepath.Join(mid, "ENSO.md"), []byte("src rules"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deep, "AGENTS.md"), []byte("api rules"), 0o644); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(deep, "handlers.go")
	if err := os.WriteFile(target, []byte("//"), 0o644); err != nil {
		t.Fatal(err)
	}

	layers, err := ResolveForPath(target, tmp)
	if err != nil {
		t.Fatal(err)
	}
	if len(layers) != 2 {
		t.Fatalf("got %d layers, want 2: %+v", len(layers), layers)
	}
	// Top-down order: src/ENSO.md before src/api/AGENTS.md
	if !strings.HasSuffix(layers[0].Name, filepath.Join("src", "ENSO.md")) {
		t.Errorf("layers[0] name: %q (want src/ENSO.md)", layers[0].Name)
	}
	if !strings.HasSuffix(layers[1].Name, filepath.Join("api", "AGENTS.md")) {
		t.Errorf("layers[1] name: %q (want api/AGENTS.md)", layers[1].Name)
	}
}

// TestResolveForPath_RootFilesSkipped pins the no-double-inject
// contract: ENSO.md / AGENTS.md at the root cwd are already in the
// static system prompt, so they must NOT appear in ResolveForPath's
// output for ANY target.
func TestResolveForPath_RootFilesSkipped(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "ENSO.md"), []byte("root rules"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "AGENTS.md"), []byte("root agents"), 0o644); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(tmp, "x.go")
	if err := os.WriteFile(target, []byte("//"), 0o644); err != nil {
		t.Fatal(err)
	}

	layers, err := ResolveForPath(target, tmp)
	if err != nil {
		t.Fatal(err)
	}
	if len(layers) != 0 {
		t.Errorf("root files leaked into contextual injection: %+v", layers)
	}
}

// TestResolveForPath_OutsideRootIgnored confirms a target outside the
// project root yields nothing — symlinks to /etc, ../ relative paths,
// and so on shouldn't trigger system reminders since the model already
// has no contract for instructions there.
func TestResolveForPath_OutsideRootIgnored(t *testing.T) {
	tmp := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "ENSO.md"), []byte("rogue"), 0o644); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(outside, "x.go")
	if err := os.WriteFile(target, []byte("//"), 0o644); err != nil {
		t.Fatal(err)
	}

	layers, err := ResolveForPath(target, tmp)
	if err != nil {
		t.Fatal(err)
	}
	if len(layers) != 0 {
		t.Errorf("layers from outside-root path: %+v", layers)
	}
}
