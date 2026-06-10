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
	p1, root1, err := Stage(src)
	if err != nil {
		t.Fatalf("Stage v1: %v", err)
	}
	// The mount root is the stable exe/ parent of the snapshot.
	if root1 != filepath.Dir(filepath.Dir(p1)) {
		t.Fatalf("root %q must be the parent of the <hash> dir of %q", root1, p1)
	}
	if b, _ := os.ReadFile(p1); string(b) != "BINARY-V1" {
		t.Fatalf("staged content = %q, want BINARY-V1", b)
	}
	if fi, _ := os.Stat(p1); fi == nil || fi.Mode().Perm()&0o100 == 0 {
		t.Fatalf("staged binary must be executable")
	}

	// Same content again → same path, copied at most once.
	p1b, _, err := Stage(src)
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
	p2, root2, err := Stage(src)
	if err != nil {
		t.Fatalf("Stage v2: %v", err)
	}
	if p2 == p1 {
		t.Fatalf("rebuilt (different) binary must get a new path, got the same: %q", p2)
	}
	// The mount root is INVARIANT across the rebuild — this is what
	// keeps the lima VM YAML from drifting and forcing a cold reboot.
	if root2 != root1 {
		t.Fatalf("mount root must be stable across rebuilds: %q vs %q", root2, root1)
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

	a, _, _ := Stage(writeExe(t, srcDir, "a", "AAA"))
	b, _, _ := Stage(writeExe(t, srcDir, "b", "BBB"))
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

func TestSweep_KeepsAcquiredSnapshot(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	srcDir := t.TempDir()

	p, _, err := Stage(writeExe(t, srcDir, "enso", "PINNED"))
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}

	// Pin the snapshot the way a backend does for a live worker. Sweep(0)
	// is the harshest case (mtime is irrelevant — remove all): the in-use
	// lock alone must keep the binary a guest is still mmap-executing.
	release, err := Acquire(p)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if n, err := Sweep(0); err != nil || n != 0 {
		t.Fatalf("Sweep(0) under lock removed %d (err %v), want 0", n, err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("acquired snapshot was swept: %v", err)
	}

	// Released (worker torn down) → the same Sweep reclaims it.
	release()
	release() // idempotent — Teardown paths may overlap
	if n, err := Sweep(0); err != nil || n != 1 {
		t.Fatalf("Sweep(0) after release removed %d (err %v), want 1", n, err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("released snapshot survived Sweep(0): %v", err)
	}
}
