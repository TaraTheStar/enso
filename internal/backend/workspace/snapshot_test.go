// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestSnapshotAndRestore covers the full per-turn checkpoint cycle:
// snapshot a project, mutate it (modify, create, delete), then restore
// and confirm the tree matches the snapshot exactly.
func TestSnapshotAndRestore(t *testing.T) {
	ctx := context.Background()
	proj := t.TempDir()
	writeFile(t, filepath.Join(proj, "keep.txt"), "original")
	writeFile(t, filepath.Join(proj, "sub/mod.txt"), "v1")
	writeFile(t, filepath.Join(proj, "sub/gone.txt"), "delete-me")

	snap := filepath.Join(t.TempDir(), "snap")
	if err := SnapshotTree(ctx, proj, snap); err != nil {
		t.Fatal(err)
	}

	// Mutate the working tree: modify, create, delete.
	writeFile(t, filepath.Join(proj, "sub/mod.txt"), "v2-changed")
	writeFile(t, filepath.Join(proj, "new.txt"), "agent-created")
	if err := os.Remove(filepath.Join(proj, "sub/gone.txt")); err != nil {
		t.Fatal(err)
	}

	changed, err := RestoreTree(ctx, snap, proj)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(changed)
	want := []string{"new.txt", "sub/gone.txt", "sub/mod.txt"}
	if !equalStrSlice(changed, want) {
		t.Errorf("changed = %v, want %v", changed, want)
	}

	// Modified file reverted.
	if got := readFile(t, filepath.Join(proj, "sub/mod.txt")); got != "v1" {
		t.Errorf("mod.txt = %q, want v1 (reverted)", got)
	}
	// Agent-created file removed.
	if _, err := os.Stat(filepath.Join(proj, "new.txt")); !os.IsNotExist(err) {
		t.Errorf("new.txt should have been removed on restore")
	}
	// Agent-deleted file recreated.
	if got := readFile(t, filepath.Join(proj, "sub/gone.txt")); got != "delete-me" {
		t.Errorf("gone.txt = %q, want delete-me (recreated)", got)
	}
	// Untouched file intact.
	if got := readFile(t, filepath.Join(proj, "keep.txt")); got != "original" {
		t.Errorf("keep.txt = %q, want original", got)
	}
}

// TestSnapshotExcludesGit confirms .git is neither snapshotted nor
// touched on restore — git internals stay with git.
func TestSnapshotExcludesGit(t *testing.T) {
	ctx := context.Background()
	proj := t.TempDir()
	writeFile(t, filepath.Join(proj, "src.go"), "package main")
	writeFile(t, filepath.Join(proj, ".git/HEAD"), "ref: refs/heads/main")

	snap := filepath.Join(t.TempDir(), "snap")
	if err := SnapshotTree(ctx, proj, snap); err != nil {
		t.Fatal(err)
	}
	// .git must not be copied into the snapshot.
	if _, err := os.Stat(filepath.Join(snap, ".git")); !os.IsNotExist(err) {
		t.Errorf(".git should be excluded from the snapshot")
	}

	// Mutate .git (as a git operation would) and a tracked file; restore.
	writeFile(t, filepath.Join(proj, ".git/HEAD"), "ref: refs/heads/feature")
	writeFile(t, filepath.Join(proj, "src.go"), "package main // edited")

	changed, err := RestoreTree(ctx, snap, proj)
	if err != nil {
		t.Fatal(err)
	}
	if !equalStrSlice(changed, []string{"src.go"}) {
		t.Errorf("changed = %v, want [src.go] only (.git untouched)", changed)
	}
	// .git left exactly as the user/git left it.
	if got := readFile(t, filepath.Join(proj, ".git/HEAD")); got != "ref: refs/heads/feature" {
		t.Errorf(".git/HEAD was modified on restore: %q", got)
	}
}

// TestRestorePreservesSymlinks confirms a symlink in the project is
// snapshotted and restored as a symlink (cp -a), not dereferenced.
func TestRestorePreservesSymlinks(t *testing.T) {
	ctx := context.Background()
	proj := t.TempDir()
	writeFile(t, filepath.Join(proj, "target.txt"), "data")
	if err := os.Symlink("target.txt", filepath.Join(proj, "link")); err != nil {
		t.Fatal(err)
	}
	snap := filepath.Join(t.TempDir(), "snap")
	if err := SnapshotTree(ctx, proj, snap); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(filepath.Join(snap, "link"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("snapshot dereferenced the symlink instead of preserving it")
	}
}

func equalStrSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
