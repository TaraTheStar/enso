// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/TaraTheStar/enso/internal/llm"
)

func TestCheckpoint_RecordAndList(t *testing.T) {
	s := openTestStore(t)
	w, err := NewSession(s, "qwen", "local", "/proj")
	if err != nil {
		t.Fatal(err)
	}
	if err := w.RecordCheckpoint(1, "snap-1"); err != nil {
		t.Fatal(err)
	}
	if err := w.RecordCheckpoint(3, "snap-3"); err != nil {
		t.Fatal(err)
	}
	// Re-record at an existing seq replaces the snapshot id (idempotent).
	if err := w.RecordCheckpoint(1, "snap-1b"); err != nil {
		t.Fatal(err)
	}

	cps, err := ListCheckpoints(s, w.SessionID())
	if err != nil {
		t.Fatal(err)
	}
	if len(cps) != 2 {
		t.Fatalf("want 2 checkpoints, got %d: %+v", len(cps), cps)
	}
	if cps[0].Seq != 1 || cps[0].Snapshot != "snap-1b" {
		t.Errorf("checkpoint[0] = %+v, want seq=1 snapshot=snap-1b", cps[0])
	}
	if cps[1].Seq != 3 || cps[1].Snapshot != "snap-3" {
		t.Errorf("checkpoint[1] = %+v, want seq=3 snapshot=snap-3", cps[1])
	}
}

func TestCheckpoint_RecordRejectsInvalid(t *testing.T) {
	s := openTestStore(t)
	w, _ := NewSession(s, "qwen", "local", "/proj")
	if err := w.RecordCheckpoint(0, "snap"); err == nil {
		t.Error("seq=0 should be rejected")
	}
	if err := w.RecordCheckpoint(1, ""); err == nil {
		t.Error("empty snapshot should be rejected")
	}
}

func TestCheckpoint_DeleteAfter(t *testing.T) {
	s := openTestStore(t)
	w, _ := NewSession(s, "qwen", "local", "/proj")
	for _, c := range []struct {
		seq  int
		snap string
	}{{1, "s1"}, {3, "s3"}, {5, "s5"}, {7, "s7"}} {
		if err := w.RecordCheckpoint(c.seq, c.snap); err != nil {
			t.Fatal(err)
		}
	}

	// Restore to checkpoint 5 => keep seq <= 4 => delete checkpoints at 5 and 7.
	freed, err := DeleteCheckpointsAfter(s, w.SessionID(), 4)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(freed)
	if want := []string{"s5", "s7"}; !equalStrs(freed, want) {
		t.Errorf("freed = %v, want %v", freed, want)
	}
	cps, _ := ListCheckpoints(s, w.SessionID())
	if len(cps) != 2 || cps[0].Seq != 1 || cps[1].Seq != 3 {
		t.Errorf("remaining checkpoints wrong: %+v", cps)
	}
}

func TestCheckpoint_Prune(t *testing.T) {
	s := openTestStore(t)
	w, _ := NewSession(s, "qwen", "local", "/proj")
	for i := 1; i <= 5; i++ {
		if err := w.RecordCheckpoint(i, "snap-"+string(rune('0'+i))); err != nil {
			t.Fatal(err)
		}
	}
	// Keep the 2 newest (seq 4,5); prune 1,2,3.
	freed, err := PruneCheckpoints(s, w.SessionID(), 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(freed) != 3 {
		t.Errorf("freed = %v, want 3 ids", freed)
	}
	cps, _ := ListCheckpoints(s, w.SessionID())
	if len(cps) != 2 || cps[0].Seq != 4 || cps[1].Seq != 5 {
		t.Errorf("remaining after prune wrong: %+v", cps)
	}
}

func TestCheckpoint_CascadesOnSessionDiscard(t *testing.T) {
	s := openTestStore(t)
	w, _ := NewSession(s, "qwen", "local", "/proj")
	_, _ = w.AppendMessage(llm.Message{Role: "user", Content: "hi"}, "")
	if err := w.RecordCheckpoint(1, "snap-1"); err != nil {
		t.Fatal(err)
	}
	if err := w.Discard(); err != nil {
		t.Fatal(err)
	}
	cps, err := ListCheckpoints(s, w.SessionID())
	if err != nil {
		t.Fatal(err)
	}
	if len(cps) != 0 {
		t.Errorf("checkpoints should cascade-delete with the session, got %+v", cps)
	}
}

func TestTruncateAfter(t *testing.T) {
	s := openTestStore(t)
	w, _ := NewSession(s, "qwen", "local", "/proj")
	var seqs []int
	for _, m := range []llm.Message{
		{Role: "user", Content: "q1"},
		{Role: "assistant", Content: "a1"},
		{Role: "user", Content: "q2"},
		{Role: "assistant", Content: "a2"},
		{Role: "user", Content: "q3"},
	} {
		seq, err := w.AppendMessage(m, "")
		if err != nil {
			t.Fatal(err)
		}
		seqs = append(seqs, seq)
	}
	// Stamp usage on the last message so we can confirm usage is pruned too.
	if err := w.AppendMessageUsage(seqs[4], llm.MessageUsage{InputTokens: 10}, ""); err != nil {
		t.Fatal(err)
	}

	// Rewind to before q2 (seq 3) => keep seq <= 2.
	if err := TruncateAfter(s, w.SessionID(), 2); err != nil {
		t.Fatal(err)
	}
	state, err := Load(s, w.SessionID())
	if err != nil {
		t.Fatal(err)
	}
	if len(state.History) != 2 {
		t.Fatalf("after truncate want 2 messages, got %d: %+v", len(state.History), state.History)
	}
	if state.History[0].Content != "q1" || state.History[1].Content != "a1" {
		t.Errorf("wrong messages kept: %+v", state.History)
	}

	// A re-attached writer must continue from the new MAX(seq)=2, so the
	// next append lands at seq 3 (overwriting the old, now-deleted slot
	// cleanly — no PK collision).
	w2, err := AttachWriter(s, w.SessionID())
	if err != nil {
		t.Fatal(err)
	}
	seq, err := w2.AppendMessage(llm.Message{Role: "user", Content: "new q2"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if seq != 3 {
		t.Errorf("post-truncate append seq = %d, want 3", seq)
	}
}

func TestForkAt_SeqBounded(t *testing.T) {
	s := openTestStore(t)
	src, _ := NewSession(s, "qwen", "local", "/proj")
	for _, m := range []llm.Message{
		{Role: "user", Content: "q1"},
		{Role: "assistant", Content: "a1"},
		{Role: "user", Content: "q2"},
		{Role: "assistant", Content: "a2"},
	} {
		if _, err := src.AppendMessage(m, ""); err != nil {
			t.Fatal(err)
		}
	}

	// Branch at seq 2 (keep q1+a1 only).
	newID, err := ForkAt(s, src.SessionID(), 2)
	if err != nil {
		t.Fatal(err)
	}
	state, err := Load(s, newID)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.History) != 2 {
		t.Fatalf("bounded fork want 2 messages, got %d: %+v", len(state.History), state.History)
	}
	if state.History[0].Content != "q1" || state.History[1].Content != "a1" {
		t.Errorf("bounded fork wrong messages: %+v", state.History)
	}

	// Source remains intact.
	srcState, _ := Load(s, src.SessionID())
	if len(srcState.History) != 4 {
		t.Errorf("source mutated by ForkAt: %d messages", len(srcState.History))
	}
}

func TestForkAt_PreservesFlags(t *testing.T) {
	s := openTestStore(t)
	src, _ := NewSession(s, "qwen", "local", "/proj")
	if _, err := src.AppendMessage(llm.Message{Role: "user", Content: "u"}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := src.AppendMessage(llm.Message{
		Role: "assistant", Content: "synthetic+reasoning", Synthetic: true, Reasoning: "because",
	}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := src.AppendMessage(llm.Message{Role: "user", Content: "ig", Ignored: true}, ""); err != nil {
		t.Fatal(err)
	}

	newID, err := Fork(s, src.SessionID())
	if err != nil {
		t.Fatal(err)
	}
	state, err := Load(s, newID)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.History) != 3 {
		t.Fatalf("want 3, got %d", len(state.History))
	}
	if !state.History[1].Synthetic {
		t.Error("synthetic flag not preserved across fork")
	}
	if state.History[1].Reasoning != "because" {
		t.Errorf("reasoning not preserved: %q", state.History[1].Reasoning)
	}
	if !state.History[2].Ignored {
		t.Error("ignored flag not preserved across fork")
	}
}

func TestListRewindPoints(t *testing.T) {
	s := openTestStore(t)
	w, _ := NewSession(s, "qwen", "local", "/proj")
	// Two user turns with checkpoints + intervening assistant rows.
	s1, _ := w.AppendMessage(llm.Message{Role: "user", Content: "first question"}, "")
	_ = mustRecord(t, w, s1, "snap-1")
	_, _ = w.AppendMessage(llm.Message{Role: "assistant", Content: "answer"}, "")
	s3, _ := w.AppendMessage(llm.Message{Role: "user", Content: "second question"}, "")
	_ = mustRecord(t, w, s3, "snap-3")

	pts, err := ListRewindPoints(s, w.SessionID())
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 2 {
		t.Fatalf("want 2 rewind points, got %d: %+v", len(pts), pts)
	}
	if pts[0].Seq != s1 || pts[0].Preview != "first question" {
		t.Errorf("point[0] = %+v, want seq=%d preview=%q", pts[0], s1, "first question")
	}
	if pts[1].Seq != s3 || pts[1].Preview != "second question" {
		t.Errorf("point[1] = %+v, want seq=%d preview=%q", pts[1], s3, "second question")
	}
}

func TestCheckpointStoreDir(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/tmp/xdgstate")
	dir, err := CheckpointStoreDir("sess-abc")
	if err != nil {
		t.Fatal(err)
	}
	if dir != "/tmp/xdgstate/enso/checkpoints/sess-abc" {
		t.Errorf("CheckpointStoreDir = %q", dir)
	}
}

// TestCaptureCheckpoint exercises the shared snapshot→record→prune→
// cleanup orchestration both call sites (local worker + isolated host)
// use: each turn snapshots via the injected snapFn, records a row, and
// prunes to the retention cap, removing the freed on-disk snapshots.
func TestCaptureCheckpoint(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	s := openTestStore(t)
	w, err := NewSession(s, "qwen", "local", "/proj")
	if err != nil {
		t.Fatal(err)
	}
	sid := w.SessionID()
	base, err := CheckpointStoreDir(sid)
	if err != nil {
		t.Fatal(err)
	}

	// A self-contained snapshot: write one marker file into dst (avoids a
	// session→workspace dependency; the engine itself is tested in the
	// workspace package).
	snap := func(_ context.Context, dst string) error {
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, "marker"), []byte("snap"), 0o644)
	}

	for seq := 1; seq <= 3; seq++ {
		if err := CaptureCheckpoint(s, w, seq, 2, snap); err != nil {
			t.Fatalf("capture seq %d: %v", seq, err)
		}
		if _, err := os.Stat(filepath.Join(base, strconv.Itoa(seq), "marker")); err != nil {
			t.Fatalf("seq %d snapshot not written: %v", seq, err)
		}
	}

	// retain=2 → only seqs 2,3 survive in the DB...
	cps, _ := ListCheckpoints(s, sid)
	if len(cps) != 2 || cps[0].Seq != 2 || cps[1].Seq != 3 {
		t.Fatalf("checkpoints after prune = %+v, want seqs [2 3]", cps)
	}
	// ...and on disk (seq 1's dir was removed when pruned).
	if _, err := os.Stat(filepath.Join(base, "1")); !os.IsNotExist(err) {
		t.Error("pruned snapshot dir 1 should be gone from disk")
	}
	for _, keep := range []string{"2", "3"} {
		if _, err := os.Stat(filepath.Join(base, keep)); err != nil {
			t.Errorf("retained snapshot dir %s missing: %v", keep, err)
		}
	}
}

// TestCaptureCheckpoint_NoRecordOnSnapshotFailure confirms a snapshot
// failure records no row (best-effort) and the error surfaces for logging.
func TestCaptureCheckpoint_NoRecordOnSnapshotFailure(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	s := openTestStore(t)
	w, _ := NewSession(s, "qwen", "local", "/proj")

	boom := func(context.Context, string) error { return os.ErrPermission }
	if err := CaptureCheckpoint(s, w, 1, 5, boom); err == nil {
		t.Fatal("expected snapshot failure to surface an error")
	}
	cps, _ := ListCheckpoints(s, w.SessionID())
	if len(cps) != 0 {
		t.Errorf("a failed snapshot must record no checkpoint, got %+v", cps)
	}
}

// TestSweepCheckpoints verifies `enso prune`'s on-disk reclamation:
// orphan session dirs (no session row) are removed whole, orphan <seq>
// subdirs of a live session (no checkpoint row) are removed, and
// referenced snapshot dirs are kept.
func TestSweepCheckpoints(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	t.Setenv("XDG_DATA_HOME", t.TempDir()) // session.Open() reads DataDir

	// Use the DEFAULT store (the one SweepCheckpoints opens internally).
	store, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	live, err := NewSession(store, "qwen", "local", "/proj")
	if err != nil {
		t.Fatal(err)
	}
	lid := live.SessionID()
	if err := live.RecordCheckpoint(1, "1"); err != nil {
		t.Fatal(err)
	}
	store.Close() // SweepCheckpoints opens its own handle

	// Lay out snapshot dirs: a referenced one + an orphan seq for the live
	// session, and a whole orphan session dir.
	root := filepath.Join(state, "enso", "checkpoints")
	mkdir := func(parts ...string) string {
		p := filepath.Join(append([]string{root}, parts...)...)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	referenced := mkdir(lid, "1") // has a checkpoint row → keep
	orphanSeq := mkdir(lid, "2")  // live session, no row → remove
	mkdir("ghost-session", "1")   // no session row → remove whole dir

	n, err := SweepCheckpoints(0)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("removed = %d, want 2 (one orphan seq + one orphan session)", n)
	}
	if _, err := os.Stat(referenced); err != nil {
		t.Errorf("referenced snapshot dir should be kept: %v", err)
	}
	if _, err := os.Stat(orphanSeq); !os.IsNotExist(err) {
		t.Error("orphan seq dir of a live session should be removed")
	}
	if _, err := os.Stat(filepath.Join(root, "ghost-session")); !os.IsNotExist(err) {
		t.Error("orphan session dir should be removed whole")
	}
}

// TestSweepCheckpoints_OlderThan confirms a too-recent orphan is spared
// when an age floor is set.
func TestSweepCheckpoints_OlderThan(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	root := filepath.Join(state, "enso", "checkpoints")
	fresh := filepath.Join(root, "ghost", "1")
	if err := os.MkdirAll(fresh, 0o755); err != nil {
		t.Fatal(err)
	}

	// Age floor of an hour spares a just-created orphan.
	n, err := SweepCheckpoints(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("removed = %d, want 0 (orphan younger than the age floor)", n)
	}
	if _, err := os.Stat(filepath.Join(root, "ghost")); err != nil {
		t.Errorf("a fresh orphan should be spared by --older-than: %v", err)
	}
}

func mustRecord(t *testing.T, w *Writer, seq int, snap string) int {
	t.Helper()
	if err := w.RecordCheckpoint(seq, snap); err != nil {
		t.Fatal(err)
	}
	return seq
}

func equalStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
