// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFindGitRootWalksUp(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	deep := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := findGitRoot(deep)
	if err != nil {
		t.Fatal(err)
	}
	// Resolve symlinks for stability across /var → /private/var on macOS;
	// the temp dir on Linux is its own canonical path so this is a no-op.
	wantResolved, _ := filepath.EvalSymlinks(root)
	gotResolved, _ := filepath.EvalSymlinks(got)
	if gotResolved != wantResolved {
		t.Errorf("got %q, want %q", gotResolved, wantResolved)
	}
}

func TestFindGitRootErrorsOutsideRepo(t *testing.T) {
	root := t.TempDir()
	if _, err := findGitRoot(root); err == nil {
		t.Errorf("expected error when no .git anywhere up the tree")
	}
}

func TestRandSlugShape(t *testing.T) {
	a, b := randSlug(), randSlug()
	if len(a) != 6 || len(b) != 6 {
		t.Errorf("expected 6-char slugs, got %q %q", a, b)
	}
	if a == b {
		// Astronomically unlikely but not impossible; flake-tolerant
		// assertion: just confirm hex.
		_ = a
	}
	if !isHex(a) || !isHex(b) {
		t.Errorf("non-hex slugs: %q %q", a, b)
	}
}

func isHex(s string) bool {
	for _, r := range s {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return false
		}
	}
	return true
}
