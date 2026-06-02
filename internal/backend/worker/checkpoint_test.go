// SPDX-License-Identifier: AGPL-3.0-or-later

package worker

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/TaraTheStar/enso/internal/session"
)

// TestNewCheckpointFn_SnapshotsRecordsAndPrunes exercises the full
// local-backend checkpoint orchestration: each genuine user turn
// snapshots the cwd, records a checkpoint row, and prunes to the
// retention cap (removing the freed on-disk snapshots).
func TestNewCheckpointFn_SnapshotsRecordsAndPrunes(t *testing.T) {
	// Isolate the state dir so checkpointsRoot resolves under the test tmp.
	stateDir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateDir)

	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "a.txt"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}

	store, err := session.OpenAt(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	w, err := session.NewSession(store, "qwen", "local", cwd)
	if err != nil {
		t.Fatal(err)
	}
	sid := w.SessionID()

	// Retain 2: after 3 turns, only the 2 newest checkpoints survive.
	fn := newCheckpointFn(store, w, cwd, sid, 2)
	if fn == nil {
		t.Fatal("newCheckpointFn returned nil")
	}

	base := filepath.Join(stateDir, "enso", "checkpoints", sid)
	for seq := 1; seq <= 3; seq++ {
		// Mutate the tree between turns so each snapshot is distinct.
		if err := os.WriteFile(filepath.Join(cwd, "a.txt"), []byte{byte('0' + seq)}, 0o644); err != nil {
			t.Fatal(err)
		}
		fn(seq)
		// The snapshot dir for this seq must exist with the captured content.
		snap := filepath.Join(base, strconv.Itoa(seq))
		got, err := os.ReadFile(filepath.Join(snap, "a.txt"))
		if err != nil {
			t.Fatalf("turn %d: snapshot not written: %v", seq, err)
		}
		if string(got) != string([]byte{byte('0' + seq)}) {
			t.Errorf("turn %d: snapshot content = %q", seq, got)
		}
	}

	// DB: only seqs 2 and 3 remain (retain=2).
	cps, err := session.ListCheckpoints(store, sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(cps) != 2 || cps[0].Seq != 2 || cps[1].Seq != 3 {
		t.Fatalf("checkpoints after prune = %+v, want seqs [2 3]", cps)
	}

	// Disk: seq 1's snapshot dir is gone; 2 and 3 remain.
	if _, err := os.Stat(filepath.Join(base, "1")); !os.IsNotExist(err) {
		t.Error("pruned snapshot dir 1 should have been removed from disk")
	}
	for _, keep := range []string{"2", "3"} {
		if _, err := os.Stat(filepath.Join(base, keep)); err != nil {
			t.Errorf("retained snapshot dir %s missing: %v", keep, err)
		}
	}
}

// TestNewCheckpointFn_BestEffortOnSnapshotFailure confirms a snapshot
// failure (cwd doesn't exist) is swallowed and records nothing — the
// turn must never be blocked by checkpointing.
func TestNewCheckpointFn_BestEffortOnSnapshotFailure(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	store, err := session.OpenAt(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	w, _ := session.NewSession(store, "qwen", "local", "/nope")

	fn := newCheckpointFn(store, w, filepath.Join(t.TempDir(), "does-not-exist"), w.SessionID(), 5)
	fn(1) // must not panic

	cps, _ := session.ListCheckpoints(store, w.SessionID())
	if len(cps) != 0 {
		t.Errorf("a failed snapshot must record no checkpoint, got %+v", cps)
	}
}
