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
