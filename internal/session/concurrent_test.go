// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"path/filepath"
	"sync"
	"testing"

	"github.com/TaraTheStar/enso/internal/llm"
)

// TestConcurrentStoreWriters mirrors the Backend-seam reality under
// LocalBackend: the worker process and the host process each hold an
// independent connection to the SAME sqlite file — the worker appends
// messages, the host appends audit events — and they can race. WAL +
// busy_timeout(5000) (see store.go DSN) is supposed to absorb that.
// This is the regression guard for "two writers, one db file": if a
// future DSN/schema change drops WAL or the timeout, interleaved writes
// start returning "database is locked" and this fails.
func TestConcurrentStoreWriters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")

	// Two independent opens == two processes (host + worker).
	hostStore, err := OpenAt(path)
	if err != nil {
		t.Fatalf("open host store: %v", err)
	}
	defer hostStore.Close()
	workerStore, err := OpenAt(path)
	if err != nil {
		t.Fatalf("open worker store: %v", err)
	}
	defer workerStore.Close()

	// Host owns row creation (as in the real seam wiring).
	const sid = "race-1"
	if _, err := NewSessionWithID(hostStore, sid, "m", "p", t.TempDir()); err != nil {
		t.Fatalf("create session: %v", err)
	}

	msgW, err := AttachWriter(workerStore, sid) // worker: messages
	if err != nil {
		t.Fatalf("worker attach: %v", err)
	}
	evW, err := AttachWriter(hostStore, sid) // host: audit events
	if err != nil {
		t.Fatalf("host attach: %v", err)
	}

	const n = 60
	var wg sync.WaitGroup
	errCh := make(chan error, 2*n)
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			if _, err := msgW.AppendMessage(llm.Message{Role: "user", Content: "ping"}, ""); err != nil {
				errCh <- err
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			if err := evW.AppendEvent("ToolCallEnd", map[string]any{"i": i}); err != nil {
				errCh <- err
				return
			}
		}
	}()
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent write failed (WAL/busy_timeout regression?): %v", err)
	}
}

// TestSharedWriterConcurrentAppends guards H2: a single Writer is shared
// across the top-level agent and every sub-agent (spawn forwards it
// verbatim). Unlocked `w.seq++` would hand two goroutines the same seq and
// collide on the (session_id, seq) PK — a silently dropped message. With
// the counter mutex, every concurrent append must land at a distinct seq.
func TestSharedWriterConcurrentAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shared.db")
	s, err := OpenAt(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	w, err := NewSession(s, "m", "p", t.TempDir())
	if err != nil {
		t.Fatalf("new session: %v", err)
	}

	const goroutines, per = 8, 25
	const want = goroutines * per
	var wg sync.WaitGroup
	errCh := make(chan error, want)
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < per; i++ {
				if _, err := w.AppendMessage(llm.Message{Role: "user", Content: "x"}, "sub"); err != nil {
					errCh <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent shared-writer append failed: %v", err)
	}

	// No PK collisions == all rows present, all seqs distinct.
	var count, distinct int
	if err := s.DB.QueryRow(
		`SELECT COUNT(*), COUNT(DISTINCT seq) FROM messages WHERE session_id = ?`,
		w.SessionID(),
	).Scan(&count, &distinct); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != want || distinct != want {
		t.Errorf("rows=%d distinct-seq=%d, want %d/%d (a mismatch means a dropped or colliding append)",
			count, distinct, want, want)
	}
}

// TestAppendMessageUsage_SurvivesInterleavedAppend guards H2's attribution
// half: usage must attach to the seq returned by its own AppendMessage even
// when another append advances the writer's cursor in between (the old code
// read w.seq at usage time and mis-attributed).
func TestAppendMessageUsage_SurvivesInterleavedAppend(t *testing.T) {
	s, err := OpenAt(filepath.Join(t.TempDir(), "attr.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	w, err := NewSession(s, "m", "p", t.TempDir())
	if err != nil {
		t.Fatalf("new session: %v", err)
	}

	// Parent assistant message gets seq1.
	seq1, err := w.AppendMessage(llm.Message{Role: "assistant", Content: "parent"}, "")
	if err != nil {
		t.Fatalf("append parent: %v", err)
	}
	// A sub-agent append lands in between, advancing the cursor.
	if _, err := w.AppendMessage(llm.Message{Role: "assistant", Content: "child"}, "sub"); err != nil {
		t.Fatalf("append child: %v", err)
	}
	// Stamp the parent's usage against the seq we captured — must NOT bleed
	// onto the child's (now-current) seq.
	parentUsage := llm.MessageUsage{InputTokens: 42, TotalTokens: 50, OutputTokens: 8}
	if err := w.AppendMessageUsage(seq1, parentUsage, ""); err != nil {
		t.Fatalf("usage: %v", err)
	}

	var gotSeq, gotInput int
	if err := s.DB.QueryRow(
		`SELECT seq, input_tokens FROM message_usage WHERE session_id = ? AND agent_id = ''`,
		w.SessionID(),
	).Scan(&gotSeq, &gotInput); err != nil {
		t.Fatalf("read usage: %v", err)
	}
	if gotSeq != seq1 || gotInput != 42 {
		t.Errorf("usage attached to seq=%d input=%d, want seq=%d input=42 (mis-attribution)", gotSeq, gotInput, seq1)
	}
}
