// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TaraTheStar/enso/internal/backend/workspace"
)

func needTool(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Skipf("%s not on PATH", name)
	}
}

func write(t *testing.T, p, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		return "<missing>"
	}
	return string(b)
}

// TestOverlayThreeWayCommitNoConflict: agent edits + creates + deletes;
// no host drift → interactive commit applies all of it per-file.
func TestOverlayThreeWayCommitNoConflict(t *testing.T) {
	needTool(t, "cp")
	needTool(t, "diff")
	ctx := context.Background()

	proj := t.TempDir()
	write(t, filepath.Join(proj, "keep.txt"), "original\n")
	write(t, filepath.Join(proj, "gone.txt"), "delete me\n")

	ov, err := workspace.New(ctx, proj)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Real project untouched while the agent "runs".
	if read(t, filepath.Join(proj, "keep.txt")) != "original\n" {
		t.Fatal("overlay must not mutate the project during the session")
	}
	write(t, filepath.Join(ov.Copy, "keep.txt"), "modified by agent\n")
	write(t, filepath.Join(ov.Copy, "new.txt"), "created by agent\n")
	if err := os.Remove(filepath.Join(ov.Copy, "gone.txt")); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	if err := workspace.Resolve(ctx, ov, true, strings.NewReader("c\n"), &out); err != nil {
		t.Fatalf("Resolve commit: %v", err)
	}
	if got := read(t, filepath.Join(proj, "keep.txt")); got != "modified by agent\n" {
		t.Errorf("modification not applied: %q", got)
	}
	if got := read(t, filepath.Join(proj, "new.txt")); got != "created by agent\n" {
		t.Errorf("creation not applied: %q", got)
	}
	if _, err := os.Stat(filepath.Join(proj, "gone.txt")); !os.IsNotExist(err) {
		t.Errorf("deletion not applied (gone.txt still present)")
	}
	if _, err := os.Stat(ov.Copy); !os.IsNotExist(err) {
		t.Errorf("a clean commit should remove the throwaway copy")
	}
}

// The original footgun, fixed: the agent changes file A while the host
// concurrently changes a DIFFERENT file B. Commit must apply A and
// LEAVE B's host edit intact (no blind rsync --delete from a stale
// snapshot).
func TestThreeWayPreservesConcurrentHostEditToOtherFile(t *testing.T) {
	needTool(t, "cp")
	needTool(t, "diff")
	ctx := context.Background()

	proj := t.TempDir()
	write(t, filepath.Join(proj, "a.txt"), "a-v1\n")
	write(t, filepath.Join(proj, "b.txt"), "b-v1\n")

	ov, err := workspace.New(ctx, proj)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	write(t, filepath.Join(ov.Copy, "a.txt"), "a-v2 by agent\n") // agent edits A
	write(t, filepath.Join(proj, "b.txt"), "b-v2 by host\n")     // host edits B concurrently

	var out strings.Builder
	if err := workspace.Resolve(ctx, ov, true, strings.NewReader("c\n"), &out); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := read(t, filepath.Join(proj, "a.txt")); got != "a-v2 by agent\n" {
		t.Errorf("agent change to A must be applied; got %q", got)
	}
	if got := read(t, filepath.Join(proj, "b.txt")); got != "b-v2 by host\n" {
		t.Errorf("concurrent host edit to B must be PRESERVED; got %q (regressed footgun)", got)
	}
}

// Both sides edit the SAME file → conflict. Plain commit applies the
// safe set only (here empty) and must NOT clobber the host's version;
// the copy is kept for manual merge.
func TestThreeWayConflictNotClobberedByCommit(t *testing.T) {
	needTool(t, "cp")
	needTool(t, "diff")
	ctx := context.Background()

	proj := t.TempDir()
	write(t, filepath.Join(proj, "f.txt"), "v1\n")
	ov, err := workspace.New(ctx, proj)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	write(t, filepath.Join(ov.Copy, "f.txt"), "agent edit\n")
	write(t, filepath.Join(proj, "f.txt"), "host edit\n")

	var out strings.Builder
	if err := workspace.Resolve(ctx, ov, true, strings.NewReader("c\n"), &out); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := read(t, filepath.Join(proj, "f.txt")); got != "host edit\n" {
		t.Errorf("conflicted file must NOT be clobbered by plain commit; got %q", got)
	}
	s := out.String()
	if !strings.Contains(s, "conflict") || !strings.Contains(s, "f.txt") {
		t.Errorf("must report the conflict and the file:\n%s", s)
	}
	if _, err := os.Stat(ov.Copy); err != nil {
		t.Errorf("copy must be kept when conflicts remain: %v", err)
	}
}

// [f]orce + explicit 'overwrite' applies the agent's version even over
// a conflicting host edit.
func TestThreeWayForceOverwritesConflict(t *testing.T) {
	needTool(t, "cp")
	needTool(t, "diff")
	ctx := context.Background()

	proj := t.TempDir()
	write(t, filepath.Join(proj, "f.txt"), "v1\n")
	ov, err := workspace.New(ctx, proj)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	write(t, filepath.Join(ov.Copy, "f.txt"), "agent wins\n")
	write(t, filepath.Join(proj, "f.txt"), "host edit\n")

	var out strings.Builder
	if err := workspace.Resolve(ctx, ov, true, strings.NewReader("f\noverwrite\n"), &out); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := read(t, filepath.Join(proj, "f.txt")); got != "agent wins\n" {
		t.Errorf("explicit force must apply the agent version; got %q", got)
	}
}

// force without the explicit 'overwrite' confirmation aborts.
func TestThreeWayForceRequiresConfirmation(t *testing.T) {
	needTool(t, "cp")
	needTool(t, "diff")
	ctx := context.Background()

	proj := t.TempDir()
	write(t, filepath.Join(proj, "f.txt"), "v1\n")
	ov, err := workspace.New(ctx, proj)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	write(t, filepath.Join(ov.Copy, "f.txt"), "agent\n")
	write(t, filepath.Join(proj, "f.txt"), "host\n")

	var out strings.Builder
	// 'f' then a non-"overwrite" answer → abort, then 'k' to exit.
	if err := workspace.Resolve(ctx, ov, true, strings.NewReader("f\nno\nk\n"), &out); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := read(t, filepath.Join(proj, "f.txt")); got != "host\n" {
		t.Errorf("unconfirmed force must not change the project; got %q", got)
	}
	if !strings.Contains(out.String(), "force aborted") {
		t.Errorf("must report the abort:\n%s", out.String())
	}
}

func TestResolveNonInteractiveKeeps(t *testing.T) {
	needTool(t, "cp")
	needTool(t, "diff")
	ctx := context.Background()

	proj := t.TempDir()
	write(t, filepath.Join(proj, "f.txt"), "v1\n")
	ov, err := workspace.New(ctx, proj)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	write(t, filepath.Join(ov.Copy, "f.txt"), "v2 from agent\n")
	keptCopy := ov.Copy

	var out strings.Builder
	if err := workspace.Resolve(ctx, ov, false, nil, &out); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := read(t, filepath.Join(proj, "f.txt")); got != "v1\n" {
		t.Errorf("non-interactive Resolve must not commit; got %q", got)
	}
	if _, err := os.Stat(keptCopy); err != nil {
		t.Errorf("non-interactive Resolve must keep the copy: %v", err)
	}
	if !strings.Contains(out.String(), keptCopy) {
		t.Errorf("Resolve must tell the user where the kept copy is:\n%s", out.String())
	}
}

func TestResolveNoAgentChangeDiscards(t *testing.T) {
	needTool(t, "cp")
	needTool(t, "diff")
	ctx := context.Background()

	proj := t.TempDir()
	write(t, filepath.Join(proj, "f.txt"), "v1\n")
	ov, err := workspace.New(ctx, proj)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Agent changed nothing; host edits the project concurrently.
	write(t, filepath.Join(proj, "f.txt"), "host only\n")

	var out strings.Builder
	if err := workspace.Resolve(ctx, ov, true, strings.NewReader("c\n"), &out); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := read(t, filepath.Join(proj, "f.txt")); got != "host only\n" {
		t.Errorf("no agent change → project (incl. host edit) must be untouched; got %q", got)
	}
	if _, err := os.Stat(ov.Copy); !os.IsNotExist(err) {
		t.Errorf("no agent change → copy should be discarded")
	}
}

// KeepPath must make Cleanup a hard no-op — the copy the user was told
// is safe must survive a subsequent Cleanup() (regression: the new
// Cleanup removed o.Copy unconditionally).
func TestKeepPathThenCleanupKeepsCopy(t *testing.T) {
	needTool(t, "cp")
	needTool(t, "diff")
	ctx := context.Background()

	proj := t.TempDir()
	write(t, filepath.Join(proj, "f.txt"), "v1\n")
	ov, err := workspace.New(ctx, proj)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	write(t, filepath.Join(ov.Copy, "f.txt"), "agent\n")

	kept := ov.KeepPath()
	if err := ov.Cleanup(); err != nil {
		t.Fatalf("Cleanup after KeepPath: %v", err)
	}
	if _, err := os.Stat(kept); err != nil {
		t.Errorf("KeepPath copy must survive Cleanup, but it's gone: %v", err)
	}
	if got := read(t, filepath.Join(kept, "f.txt")); got != "agent\n" {
		t.Errorf("kept copy contents lost: %q", got)
	}
}

// PruneKept must retain only the `keep` most recent merged.kept-* and
// delete the rest — bounding the documented accumulation leak.
func TestPruneKeptCap(t *testing.T) {
	stage := t.TempDir()
	var dirs []string
	for i := range 6 {
		d := filepath.Join(stage, fmt.Sprintf("merged.kept-%d", i))
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		// Distinct, increasing mtimes (i=5 newest).
		mt := time.Unix(1_700_000_000+int64(i)*60, 0)
		if err := os.Chtimes(d, mt, mt); err != nil {
			t.Fatal(err)
		}
		dirs = append(dirs, d)
	}
	// An unrelated sibling must be untouched.
	keepMe := filepath.Join(stage, "merged")
	if err := os.MkdirAll(keepMe, 0o755); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	workspace.PruneKept(stage, 2, &out)

	left, _ := filepath.Glob(filepath.Join(stage, "merged.kept-*"))
	if len(left) != 2 {
		t.Fatalf("want 2 kept copies retained, got %d: %v", len(left), left)
	}
	for _, want := range []string{dirs[5], dirs[4]} { // the 2 newest
		if _, err := os.Stat(want); err != nil {
			t.Errorf("newest copy %s must be retained: %v", want, err)
		}
	}
	if _, err := os.Stat(keepMe); err != nil {
		t.Errorf("non-kept sibling must be untouched: %v", err)
	}
}

func TestResolveViewShowsAgentDiff(t *testing.T) {
	needTool(t, "cp")
	needTool(t, "diff")
	ctx := context.Background()

	proj := t.TempDir()
	write(t, filepath.Join(proj, "f.txt"), "alpha\n")
	ov, err := workspace.New(ctx, proj)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	write(t, filepath.Join(ov.Copy, "f.txt"), "bravo\n")

	var out strings.Builder
	if err := workspace.Resolve(ctx, ov, true, strings.NewReader("v\nk\n"), &out); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "-alpha") || !strings.Contains(s, "+bravo") {
		t.Errorf("[v]iew must print the agent's unified diff hunks:\n%s", s)
	}
}
