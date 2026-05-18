// SPDX-License-Identifier: AGPL-3.0-or-later

package lima

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// These tests pin the exact process-tree semantic the lima Teardown fix
// depends on, deterministically and without a Lima VM. The worker is
// `limactl shell <vm> … __worker`; `limactl shell` forks an ssh child,
// so the real tree is leader(limactl) → child(ssh). It is modelled here
// by sh → sleep: a leader that forks a long-lived child which never
// exits on its own (exactly like the in-VM `enso __worker`, which
// blocks until the Channel EOFs).
//
// childAlive reports whether the modelled "ssh" grandchild is still
// running after the leader is signalled.
func spawnLeaderWithChild(t *testing.T, setpgid bool) (*exec.Cmd, int) {
	t.Helper()
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	// sh (leader) backgrounds sleep (the "ssh" child), records its pid,
	// then waits — so SIGKILLing only the leader leaves sleep orphaned,
	// the precise bug shape.
	cmd := exec.Command("sh", "-c",
		"sleep 600 & echo $! > "+pidFile+"; wait")
	if setpgid {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start leader: %v", err)
	}
	var pid int
	for i := 0; i < 200; i++ {
		if b, err := os.ReadFile(pidFile); err == nil {
			if p, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && p > 0 {
				pid = p
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatal("never observed the child pid")
	}
	return cmd, pid
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// TestTeardown_LeaderOnlyKill_OrphansChild reproduces the reported bug:
// the OLD Teardown (cmd.Process.Kill() — SIGKILL the limactl leader
// only, no Setpgid) leaves the ssh child running. That orphaned ssh is
// what held the VM's SSH session open so the in-guest worker never
// EOFed and the terminal saw a lingering process.
func TestTeardown_LeaderOnlyKill_OrphansChild(t *testing.T) {
	cmd, child := spawnLeaderWithChild(t, false)
	_ = cmd.Process.Kill() // exactly the old Teardown
	_ = cmd.Wait()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !alive(child) {
		time.Sleep(20 * time.Millisecond)
	}
	if !alive(child) {
		t.Fatalf("expected the child to be ORPHANED by a leader-only kill (the bug); it died")
	}
	_ = syscall.Kill(child, syscall.SIGKILL) // cleanup
	t.Logf("confirmed: leader-only SIGKILL orphaned child pid=%d (the bug)", child)
}

// TestTeardown_ProcessGroupKill_ReapsChild proves the fix: with Setpgid
// at Start (pgid == leader pid) a single signal to the negative pid
// reaps the leader AND the ssh child together — no orphan, the session
// closes, the in-guest worker EOFs, the terminal is freed. The VM is
// never touched here (the persistent-substrate design is unchanged).
func TestTeardown_ProcessGroupKill_ReapsChild(t *testing.T) {
	cmd, child := spawnLeaderWithChild(t, true)

	pgid := cmd.Process.Pid // Setpgid ⇒ pgid == leader pid
	if err := syscall.Kill(-pgid, syscall.SIGTERM); err != nil {
		t.Fatalf("group signal: %v", err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		<-done
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && alive(child) {
		time.Sleep(20 * time.Millisecond)
	}
	if alive(child) {
		_ = syscall.Kill(child, syscall.SIGKILL)
		t.Fatalf("child pid=%d survived a process-group kill — fix ineffective", child)
	}
	t.Logf("confirmed: process-group signal reaped both leader and child pid=%d (the fix)", child)
}
