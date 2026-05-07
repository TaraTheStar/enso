// SPDX-License-Identifier: AGPL-3.0-or-later

package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveRestrictedNoRoots(t *testing.T) {
	ac := &AgentContext{Cwd: "/cwd"}
	got, err := resolveRestricted("foo.txt", ac)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(got) || filepath.Base(got) != "foo.txt" {
		t.Errorf("expected absolute path ending in foo.txt, got %q", got)
	}
}

func TestResolveRestrictedAllowsUnderRoot(t *testing.T) {
	cwd, _ := filepath.Abs(t.TempDir())
	ac := &AgentContext{Cwd: cwd, RestrictedRoots: []string{cwd}}
	if _, err := resolveRestricted("inside.go", ac); err != nil {
		t.Errorf("relative path under cwd should be allowed, got %v", err)
	}
}

func TestResolveRestrictedRejectsAbsoluteEscape(t *testing.T) {
	cwd, _ := filepath.Abs(t.TempDir())
	ac := &AgentContext{Cwd: cwd, RestrictedRoots: []string{cwd}}
	if _, err := resolveRestricted("/etc/passwd", ac); err == nil {
		t.Errorf("/etc/passwd should be rejected when cwd is the only root")
	}
}

func TestResolveRestrictedRejectsRelativeEscape(t *testing.T) {
	cwd, _ := filepath.Abs(t.TempDir())
	ac := &AgentContext{Cwd: cwd, RestrictedRoots: []string{cwd}}
	if _, err := resolveRestricted("../sibling/file", ac); err == nil {
		t.Errorf("../sibling should be rejected")
	}
}

func TestResolveRestrictedAllowsAdditionalDir(t *testing.T) {
	cwd, _ := filepath.Abs(t.TempDir())
	extra, _ := filepath.Abs(t.TempDir())
	ac := &AgentContext{Cwd: cwd, RestrictedRoots: []string{cwd, extra}}

	got, err := resolveRestricted(filepath.Join(extra, "notes.md"), ac)
	if err != nil {
		t.Fatalf("path under additional root should be allowed: %v", err)
	}
	if !strings.HasPrefix(got, extra) {
		t.Errorf("returned path = %q, want under %q", got, extra)
	}
}

func TestResolveRestrictedEmptyPath(t *testing.T) {
	if _, err := resolveRestricted("", &AgentContext{Cwd: "/x"}); err == nil {
		t.Errorf("empty path must error")
	}
}

// A symlink at /cwd/secrets pointing at /etc/passwd must be rejected
// even though /cwd/secrets is lexically under /cwd. Without symlink
// resolution this is a straight cwd-confinement bypass.
func TestResolveRestrictedRejectsSymlinkEscape(t *testing.T) {
	cwd := mustTempDir(t)
	outside := mustTempDir(t)
	target := filepath.Join(outside, "secret")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(cwd, "escape")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	ac := &AgentContext{Cwd: cwd, RestrictedRoots: []string{cwd}}
	if _, err := resolveRestricted("escape", ac); err == nil {
		t.Errorf("symlink to %s must be rejected", target)
	}
}

// A symlink that resolves back into the allowed root is fine — the
// guard rejects only escapes, not all symlinks.
func TestResolveRestrictedAllowsSymlinkInsideRoot(t *testing.T) {
	cwd := mustTempDir(t)
	target := filepath.Join(cwd, "real.txt")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(cwd, "alias")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	ac := &AgentContext{Cwd: cwd, RestrictedRoots: []string{cwd}}
	if _, err := resolveRestricted("alias", ac); err != nil {
		t.Errorf("in-root symlink should be allowed: %v", err)
	}
}

// Symlinks in an intermediate directory component must be evaluated
// too: /cwd/sub -> /etc, then read(/cwd/sub/passwd) escapes via the
// parent component even though the leaf doesn't itself exist.
func TestResolveRestrictedRejectsSymlinkInParent(t *testing.T) {
	cwd := mustTempDir(t)
	outside := mustTempDir(t)
	link := filepath.Join(cwd, "sub")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	ac := &AgentContext{Cwd: cwd, RestrictedRoots: []string{cwd}}
	if _, err := resolveRestricted("sub/anything.txt", ac); err == nil {
		t.Errorf("path under symlinked directory must be rejected")
	}
}

// Chained symlinks (A → B → outside) must also be caught.
func TestResolveRestrictedRejectsSymlinkChain(t *testing.T) {
	cwd := mustTempDir(t)
	outside := mustTempDir(t)
	target := filepath.Join(outside, "leaf")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	mid := filepath.Join(cwd, "mid")
	if err := os.Symlink(target, mid); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	link := filepath.Join(cwd, "head")
	if err := os.Symlink(mid, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	ac := &AgentContext{Cwd: cwd, RestrictedRoots: []string{cwd}}
	if _, err := resolveRestricted("head", ac); err == nil {
		t.Errorf("chained symlink leaving root must be rejected")
	}
}

// Writing a file that doesn't exist yet under cwd must succeed — the
// resolver walks up to the deepest existing ancestor instead of
// failing closed when the leaf is absent.
func TestResolveRestrictedAllowsNewFileUnderCwd(t *testing.T) {
	cwd := mustTempDir(t)
	ac := &AgentContext{Cwd: cwd, RestrictedRoots: []string{cwd}}
	if _, err := resolveRestricted("not-yet/sub/created.txt", ac); err != nil {
		t.Errorf("new file under cwd should be allowed, got %v", err)
	}
}

// mustTempDir returns t.TempDir() with symlinks resolved so tests on
// macOS (where /var → /private/var) compare apples to apples.
func mustTempDir(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}
