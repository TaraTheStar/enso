// SPDX-License-Identifier: AGPL-3.0-or-later

package host

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/TaraTheStar/enso/internal/session"
)

// TestSessionCaptureCheckpoint covers the host-side isolated-backend
// checkpoint wiring: on a MsgCheckpoint the host snapshots the overlay's
// `merged` dir (here a temp dir) into the session's checkpoint store at
// the supplied top-level seq, records the row, and prunes. The capture
// runs in its own goroutine (it must not block the Channel loop), so we
// poll for completion.
func TestSessionCaptureCheckpoint(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	store, err := session.OpenAt(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	w, err := session.NewSession(store, "qwen", "container", "/proj")
	if err != nil {
		t.Fatal(err)
	}
	sid := w.SessionID()

	// Stand in for the overlay's `merged` dir.
	merged := t.TempDir()
	if err := os.WriteFile(filepath.Join(merged, "a.txt"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := &Session{
		writer:        w,
		ckptStore:     store,
		ckptMergedDir: merged,
		ckptRetain:    20,
	}
	s.captureCheckpoint(3) // seq 3 (the host's last-applied top-level seq)

	base, err := session.CheckpointStoreDir(sid)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		cps, _ := session.ListCheckpoints(store, sid)
		return len(cps) == 1
	})

	cps, _ := session.ListCheckpoints(store, sid)
	if len(cps) != 1 || cps[0].Seq != 3 {
		t.Fatalf("checkpoints = %+v, want one at seq 3", cps)
	}
	got, err := os.ReadFile(filepath.Join(base, "3", "a.txt"))
	if err != nil || string(got) != "v1" {
		t.Fatalf("snapshot of merged dir not captured: got %q err %v", got, err)
	}
}

// TestSessionCaptureCheckpoint_Guards confirms captureCheckpoint is an
// inert no-op when checkpointing isn't configured (local backend /
// ephemeral) or the seq is invalid — never panicking, never recording.
func TestSessionCaptureCheckpoint_Guards(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	store, err := session.OpenAt(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	w, _ := session.NewSession(store, "qwen", "local", "/proj")

	// No checkpointer configured (ckptStore/ckptMergedDir unset): no-op.
	(&Session{writer: w}).captureCheckpoint(1)
	// Configured but seq <= 0: no-op.
	(&Session{writer: w, ckptStore: store, ckptMergedDir: t.TempDir(), ckptRetain: 5}).captureCheckpoint(0)

	// Give any (erroneously spawned) goroutine a beat, then assert nothing
	// was recorded.
	time.Sleep(50 * time.Millisecond)
	if cps, _ := session.ListCheckpoints(store, w.SessionID()); len(cps) != 0 {
		t.Errorf("guarded captureCheckpoint must record nothing, got %+v", cps)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met within 2s")
}
