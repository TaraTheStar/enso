// SPDX-License-Identifier: AGPL-3.0-or-later

package lima

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TaraTheStar/enso/internal/backend/workspace"
)

// TestOverlayReuseAndDrift_RealVM is the regression test for the
// persistent-VM overlay write-back, exercised through the REAL
// workspace.NewAt refresh across sequential runs — the path that kept
// breaking and had no coverage:
//
//	run 1  NewAt → create VM → guest write must reach host merged
//	commit Cleanup() (clears merged in place, inode preserved)
//	run 2  NewAt again, SAME stage path → VM is REUSED (no drift) →
//	       guest write must STILL reach the refreshed host merged
//	       (the bug: NewAt rotated merged's inode, the reused VM's 9p
//	       export was stranded on the dead inode, writes lost)
//	run 3  config change (MountSource) → VM recreated and rebound
//
// Real-VM gated: environment limits SKIP; a genuine regression FAILS.
func TestOverlayReuseAndDrift_RealVM(t *testing.T) {
	if testing.Short() {
		t.Skip("boots a real Lima VM; skipped in -short")
	}
	limactl, err := exec.LookPath(limactlBin)
	if err != nil {
		t.Skip("limactl (Lima) not on PATH")
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("no /dev/kvm: hardware virtualization unavailable for a Lima VM")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test executable: %v", err)
	}

	proj := t.TempDir()
	if err := os.WriteFile(filepath.Join(proj, "seed.txt"), []byte("seed\n"), 0o644); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	stage := t.TempDir() // the stable per-project stage dir (lima.StageDir analogue)
	name := vmName(proj)
	t.Cleanup(func() {
		cl, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		_ = exec.CommandContext(cl, limactl, "stop", "--force", name).Run()
		_ = exec.CommandContext(cl, limactl, "delete", "--force", name).Run()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	b := &Backend{Exe: exe}

	guestWrite := func(fname, content string) error {
		cl, cancel := context.WithTimeout(ctx, 90*time.Second)
		defer cancel()
		out, err := exec.CommandContext(cl, limactl, "shell", name, "--",
			"sh", "-c", "printf %s "+shArg(content)+" > "+shArg(filepath.Join(proj, fname))).CombinedOutput()
		if err != nil {
			return &guestErr{err, out}
		}
		return nil
	}

	// --- run 1: fresh VM ------------------------------------------------
	ov, err := workspace.NewAt(ctx, proj, stage, io.Discard)
	if err != nil {
		t.Fatalf("NewAt run1: %v", err)
	}
	b.MountSource = ov.Copy
	if err := b.ensureRunning(ctx, limactl, name, proj, exe); err != nil {
		t.Skipf("Lima VM could not be brought up (environment, not enso): %v", err)
	}
	if err := guestWrite("r1.txt", "run1"); err != nil {
		t.Skipf("guest write failed (environment): %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(ov.Copy, "r1.txt")); err != nil || string(got) != "run1" {
		t.Fatalf("run1 write-back broken (got %q, err %v)", got, err)
	}
	// Commit semantics: Cleanup clears merged IN PLACE (inode kept).
	if err := ov.Cleanup(); err != nil {
		t.Fatalf("run1 cleanup: %v", err)
	}

	// --- run 2: VM REUSED (no config drift) -----------------------------
	// NewAt refreshes contents; MountSource path string is unchanged, so
	// ensureRunning must find the VM Running and reuse it. The guest
	// write must still reach the host merged — this is the core fix.
	ov, err = workspace.NewAt(ctx, proj, stage, io.Discard)
	if err != nil {
		t.Fatalf("NewAt run2: %v", err)
	}
	b.MountSource = ov.Copy
	if err := b.ensureRunning(ctx, limactl, name, proj, exe); err != nil {
		t.Fatalf("run2 ensureRunning (reuse) failed: %v", err)
	}
	if st := vmStatus(ctx, limactl, name); st != "Running" {
		t.Fatalf("run2: VM should be reused (Running), got %q", st)
	}
	if err := guestWrite("r2.txt", "run2"); err != nil {
		t.Fatalf("run2 guest write failed: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(ov.Copy, "r2.txt")); err != nil || string(got) != "run2" {
		t.Fatalf("REUSE WRITE-BACK BROKEN: reused VM's guest write did not reach the "+
			"refreshed host merged (got %q, err %v) — Resolve would see no changes "+
			"and silently discard", got, err)
	}

	// --- run 3: config drift forces recreate + rebind -------------------
	other := t.TempDir()
	b.MountSource = other
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatalf("mk other: %v", err)
	}
	if err := b.ensureRunning(ctx, limactl, name, proj, exe); err != nil {
		t.Fatalf("run3 drift recreate failed: %v", err)
	}
	if err := guestWrite("r3.txt", "run3"); err != nil {
		t.Fatalf("run3 guest write failed: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(other, "r3.txt")); err != nil || string(got) != "run3" {
		t.Fatalf("drift recreate did not rebind the mount (got %q, err %v)", got, err)
	}
}

// shArg single-quotes a string for safe use inside `sh -c`.
func shArg(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

type guestErr struct {
	err error
	out []byte
}

func (e *guestErr) Error() string { return e.err.Error() + ": " + string(e.out) }
