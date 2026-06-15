// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

// TestContainedDst is the S10 regression: applyPaths must refuse any rel
// whose destination escapes the project root, lexically or through a
// host symlink, before cp/RemoveAll touches it.
func TestContainedDst(t *testing.T) {
	proj := t.TempDir()
	outside := t.TempDir()
	o := &Overlay{Project: proj}

	// Normal in-tree path is allowed.
	if dst, err := o.containedDst("a/b.txt"); err != nil {
		t.Errorf("in-tree path rejected: %v", err)
	} else if dst != filepath.Join(proj, "a/b.txt") {
		t.Errorf("dst = %q, want %q", dst, filepath.Join(proj, "a/b.txt"))
	}

	// Lexical escape via `..`.
	if _, err := o.containedDst("../escape.txt"); err == nil {
		t.Error("lexical .. escape should be refused")
	}

	// Symlink escape: a symlink in the project tree pointing OUTSIDE
	// must not be a springboard for writing past the root.
	if err := os.Symlink(outside, filepath.Join(proj, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := o.containedDst("link/payload.txt"); err == nil {
		t.Error("symlink-traversal escape should be refused")
	}

	// A symlink that stays INSIDE the project is fine.
	if err := os.Mkdir(filepath.Join(proj, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(proj, "real"), filepath.Join(proj, "inner")); err != nil {
		t.Fatal(err)
	}
	if _, err := o.containedDst("inner/x.txt"); err != nil {
		t.Errorf("in-tree symlink wrongly refused: %v", err)
	}
}

// TestSafeCopyInto_AtomicAndModePreserved checks the fd-anchored copy
// creates intermediate dirs, lands the bytes, and preserves perms.
func TestSafeCopyInto_AtomicAndModePreserved(t *testing.T) {
	proj := t.TempDir()
	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "f")
	if err := os.WriteFile(src, []byte("hello"), 0o640); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := safeCopyInto(proj, "a/b/c.txt", src, fi); err != nil {
		t.Fatalf("safeCopyInto: %v", err)
	}
	dst := filepath.Join(proj, "a/b/c.txt")
	got, err := os.ReadFile(dst)
	if err != nil || string(got) != "hello" {
		t.Fatalf("content = %q, %v; want hello", got, err)
	}
	di, _ := os.Lstat(dst)
	if di.Mode().Perm() != 0o640 {
		t.Errorf("perm = %o, want 640", di.Mode().Perm())
	}
}

// TestSafeCopyInto_SymlinkLeaf recreates an agent-authored symlink.
func TestSafeCopyInto_SymlinkLeaf(t *testing.T) {
	proj := t.TempDir()
	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "link")
	if err := os.Symlink("target/path", src); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Lstat(src)
	if err := safeCopyInto(proj, "sub/link", src, fi); err != nil {
		t.Fatalf("safeCopyInto symlink: %v", err)
	}
	got, err := os.Readlink(filepath.Join(proj, "sub/link"))
	if err != nil || got != "target/path" {
		t.Fatalf("readlink = %q, %v; want target/path", got, err)
	}
}

// TestSafeCopyInto_RefusesSymlinkedParent is the TOCTOU guard: a symlink
// component in the destination's parent chain must not let the write
// escape the project root. O_NOFOLLOW makes the openat fail rather than
// follow it, so the outside directory stays empty.
func TestSafeCopyInto_RefusesSymlinkedParent(t *testing.T) {
	proj := t.TempDir()
	outside := t.TempDir()
	// A symlinked parent component, as if swapped in after the check.
	if err := os.Symlink(outside, filepath.Join(proj, "evil")); err != nil {
		t.Fatal(err)
	}
	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "f")
	if err := os.WriteFile(src, []byte("pwned"), 0o600); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Lstat(src)
	if err := safeCopyInto(proj, "evil/escape.txt", src, fi); err == nil {
		t.Fatal("expected safeCopyInto to refuse a symlinked parent component")
	}
	if _, err := os.Stat(filepath.Join(outside, "escape.txt")); !os.IsNotExist(err) {
		t.Fatal("write escaped through the symlink into the outside dir")
	}
}

// TestSafeDelete_RemovesAndRefusesSymlinkedParent covers the delete leg.
func TestSafeDelete_RemovesAndRefusesSymlinkedParent(t *testing.T) {
	proj := t.TempDir()
	// Normal delete.
	if err := os.WriteFile(filepath.Join(proj, "gone.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := safeDelete(proj, "gone.txt"); err != nil {
		t.Fatalf("safeDelete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(proj, "gone.txt")); !os.IsNotExist(err) {
		t.Fatal("file not deleted")
	}
	// Missing path is a no-op.
	if err := safeDelete(proj, "never/existed.txt"); err != nil {
		t.Fatalf("safeDelete missing should be nil: %v", err)
	}
	// A victim file behind a symlinked parent must survive.
	outside := t.TempDir()
	victim := filepath.Join(outside, "keep.txt")
	if err := os.WriteFile(victim, []byte("safe"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(proj, "evil")); err != nil {
		t.Fatal(err)
	}
	_ = safeDelete(proj, "evil/keep.txt")
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("delete escaped through symlink and removed an outside file: %v", err)
	}
}
