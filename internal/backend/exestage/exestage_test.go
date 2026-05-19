// SPDX-License-Identifier: AGPL-3.0-or-later

package exestage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeExe(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o755); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

func TestStage_ContentAddressedImmutable(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	srcDir := t.TempDir()

	src := writeExe(t, srcDir, "enso", "BINARY-V1")
	p1, err := Stage(src)
	if err != nil {
		t.Fatalf("Stage v1: %v", err)
	}
	if b, _ := os.ReadFile(p1); string(b) != "BINARY-V1" {
		t.Fatalf("staged content = %q, want BINARY-V1", b)
	}
	if fi, _ := os.Stat(p1); fi == nil || fi.Mode().Perm()&0o100 == 0 {
		t.Fatalf("staged binary must be executable")
	}

	// Same content again → same path, copied at most once.
	p1b, err := Stage(src)
	if err != nil {
		t.Fatalf("Stage v1 again: %v", err)
	}
	if p1b != p1 {
		t.Fatalf("identical content must map to the same path: %q vs %q", p1b, p1)
	}

	// Rebuild the host binary IN PLACE (the corruption trigger). The
	// new content must get a BRAND-NEW path, and the old snapshot must
	// still exist with the OLD bytes — that immutability is what keeps
	// an in-flight guest worker from being corrupted.
	if err := os.WriteFile(src, []byte("BINARY-V2-REBUILT"), 0o755); err != nil {
		t.Fatalf("rebuild src: %v", err)
	}
	p2, err := Stage(src)
	if err != nil {
		t.Fatalf("Stage v2: %v", err)
	}
	if p2 == p1 {
		t.Fatalf("rebuilt (different) binary must get a new path, got the same: %q", p2)
	}
	if b, _ := os.ReadFile(p1); string(b) != "BINARY-V1" {
		t.Fatalf("old snapshot mutated to %q — immutability violated", b)
	}
	if b, _ := os.ReadFile(p2); string(b) != "BINARY-V2-REBUILT" {
		t.Fatalf("new snapshot content = %q, want BINARY-V2-REBUILT", b)
	}
	if !strings.Contains(p1, "/exe/") || filepath.Base(p1) != "enso" {
		t.Fatalf("unexpected staged layout: %q", p1)
	}
}

func TestSweep(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	srcDir := t.TempDir()

	a, _ := Stage(writeExe(t, srcDir, "a", "AAA"))
	b, _ := Stage(writeExe(t, srcDir, "b", "BBB"))
	if a == "" || b == "" || a == b {
		t.Fatalf("setup: distinct snapshots expected (%q, %q)", a, b)
	}

	// olderThan in the future-ish → both are "recent" → kept.
	if n, err := Sweep(time.Hour); err != nil || n != 0 {
		t.Fatalf("Sweep(1h) removed %d (err %v), want 0 (both recent)", n, err)
	}
	if _, err := os.Stat(a); err != nil {
		t.Fatalf("recent snapshot wrongly swept: %v", err)
	}

	// 0 → remove everything (the unconditional prune backstop).
	n, err := Sweep(0)
	if err != nil || n != 2 {
		t.Fatalf("Sweep(0) removed %d (err %v), want 2", n, err)
	}
	if _, err := os.Stat(a); !os.IsNotExist(err) {
		t.Fatalf("snapshot survived Sweep(0): %v", err)
	}
}
