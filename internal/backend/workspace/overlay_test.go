// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

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
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestOverlayCloneDiffCommitDiscard(t *testing.T) {
	needTool(t, "cp")
	needTool(t, "diff")
	needTool(t, "rsync")
	ctx := context.Background()

	proj := t.TempDir()
	write(t, filepath.Join(proj, "keep.txt"), "original\n")
	write(t, filepath.Join(proj, "gone.txt"), "delete me\n")

	ov, err := workspace.New(ctx, proj)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Fresh clone is identical → not changed.
	if _, changed, err := ov.Changed(ctx); err != nil || changed {
		t.Fatalf("fresh clone should be unchanged: changed=%v err=%v", changed, err)
	}

	// Agent works in the COPY only; the project must stay pristine.
	write(t, filepath.Join(ov.Copy, "keep.txt"), "modified by agent\n")
	write(t, filepath.Join(ov.Copy, "new.txt"), "created by agent\n")
	if err := os.Remove(filepath.Join(ov.Copy, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(proj, "keep.txt")); string(b) != "original\n" {
		t.Fatal("project was mutated while the agent ran — overlay not isolating")
	}

	summary, changed, err := ov.Changed(ctx)
	if err != nil || !changed {
		t.Fatalf("divergence not detected: changed=%v err=%v", changed, err)
	}
	for _, want := range []string{"keep.txt", "new.txt", "gone.txt"} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary missing %q:\n%s", want, summary)
		}
	}

	// Commit mirrors the copy back: modification + creation + deletion.
	if err := ov.Commit(ctx); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(proj, "keep.txt")); string(b) != "modified by agent\n" {
		t.Errorf("commit did not apply modification: %q", b)
	}
	if _, err := os.Stat(filepath.Join(proj, "new.txt")); err != nil {
		t.Errorf("commit did not apply creation: %v", err)
	}
	if _, err := os.Stat(filepath.Join(proj, "gone.txt")); !os.IsNotExist(err) {
		t.Errorf("commit did not apply deletion (gone.txt still present)")
	}
	if _, err := os.Stat(ov.Copy); !os.IsNotExist(err) {
		t.Errorf("Commit should remove the throwaway copy")
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
	// Non-interactive: must NOT commit, must NOT destroy — keep + tell.
	if err := workspace.Resolve(ctx, ov, false, nil, &out); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(proj, "f.txt")); string(b) != "v1\n" {
		t.Errorf("non-interactive Resolve must not commit; project changed to %q", b)
	}
	if _, err := os.Stat(keptCopy); err != nil {
		t.Errorf("non-interactive Resolve must keep the copy, got: %v", err)
	}
	if !strings.Contains(out.String(), keptCopy) {
		t.Errorf("Resolve should tell the user where the kept copy is:\n%s", out.String())
	}
}

func TestResolveInteractiveCommit(t *testing.T) {
	needTool(t, "cp")
	needTool(t, "diff")
	needTool(t, "rsync")
	ctx := context.Background()

	proj := t.TempDir()
	write(t, filepath.Join(proj, "f.txt"), "v1\n")
	ov, err := workspace.New(ctx, proj)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	write(t, filepath.Join(ov.Copy, "f.txt"), "v2 from agent\n")

	var out strings.Builder
	if err := workspace.Resolve(ctx, ov, true, strings.NewReader("c\n"), &out); err != nil {
		t.Fatalf("Resolve commit: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(proj, "f.txt")); string(b) != "v2 from agent\n" {
		t.Errorf("interactive commit did not apply: %q", b)
	}
}
