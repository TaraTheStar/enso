// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"path/filepath"
	"testing"

	"github.com/TaraTheStar/enso/internal/llm"
)

// TestAppendMessageUsage_RoundTrip exercises the persistence side of
// real-token-accounting: append a message, attach usage, read it back
// from the message_usage table by (session, seq, agent_id).
func TestAppendMessageUsage_RoundTrip(t *testing.T) {
	tmp := t.TempDir()
	s, err := OpenAt(filepath.Join(tmp, "u.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	w, err := NewSession(s, "test-model", "test-prov", tmp)
	if err != nil {
		t.Fatalf("new session: %v", err)
	}

	if err := w.AppendMessage(llm.Message{Role: "user", Content: "hi"}, ""); err != nil {
		t.Fatalf("append user: %v", err)
	}
	if err := w.AppendMessage(llm.Message{Role: "assistant", Content: "hello"}, ""); err != nil {
		t.Fatalf("append asst: %v", err)
	}

	usage := llm.MessageUsage{
		InputTokens:      100,
		OutputTokens:     50,
		CacheReadTokens:  20,
		CacheWriteTokens: 5,
		ReasoningTokens:  10,
		TotalTokens:      185,
	}
	if err := w.AppendMessageUsage(usage, ""); err != nil {
		t.Fatalf("append usage: %v", err)
	}

	// Read back. The usage row should attach to the assistant message's
	// seq (the most recently appended).
	var got llm.MessageUsage
	var foundSeq int
	err = s.DB.QueryRow(
		`SELECT seq, input_tokens, output_tokens, cache_read_tokens,
		        cache_write_tokens, reasoning_tokens, total_tokens
		 FROM message_usage WHERE session_id = ? AND agent_id = ''`,
		w.SessionID(),
	).Scan(&foundSeq, &got.InputTokens, &got.OutputTokens,
		&got.CacheReadTokens, &got.CacheWriteTokens,
		&got.ReasoningTokens, &got.TotalTokens)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}

	// seq=2 because user was seq=1, assistant was seq=2.
	if foundSeq != 2 {
		t.Errorf("seq = %d, want 2 (the assistant message)", foundSeq)
	}
	if got != usage {
		t.Errorf("usage = %+v, want %+v", got, usage)
	}
}

// TestAppendMessageUsage_ReEmissionIdempotent confirms ON CONFLICT
// REPLACE: emitting usage twice for the same message overwrites
// rather than failing.
func TestAppendMessageUsage_ReEmissionIdempotent(t *testing.T) {
	tmp := t.TempDir()
	s, err := OpenAt(filepath.Join(tmp, "u.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	w, err := NewSession(s, "m", "p", tmp)
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	if err := w.AppendMessage(llm.Message{Role: "assistant", Content: "x"}, ""); err != nil {
		t.Fatalf("append: %v", err)
	}

	first := llm.MessageUsage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15}
	if err := w.AppendMessageUsage(first, ""); err != nil {
		t.Fatalf("first usage: %v", err)
	}
	second := llm.MessageUsage{InputTokens: 100, OutputTokens: 50, TotalTokens: 150}
	if err := w.AppendMessageUsage(second, ""); err != nil {
		t.Fatalf("second usage: %v", err)
	}

	var rows int
	if err := s.DB.QueryRow(
		`SELECT COUNT(*) FROM message_usage WHERE session_id = ?`, w.SessionID(),
	).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 1 {
		t.Errorf("got %d rows, want 1 (ON CONFLICT REPLACE)", rows)
	}

	var got llm.MessageUsage
	if err := s.DB.QueryRow(
		`SELECT input_tokens, output_tokens, total_tokens
		 FROM message_usage WHERE session_id = ?`, w.SessionID(),
	).Scan(&got.InputTokens, &got.OutputTokens, &got.TotalTokens); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got.InputTokens != 100 || got.TotalTokens != 150 {
		t.Errorf("usage not overwritten: got %+v, want second", got)
	}
}

// TestLoad_RehydratesMessageUsage exercises the resume path: a session
// that recorded usage during its first run should come back with the
// MessageUsage map and LastUsage populated, keyed by post-backfill
// History indices.
func TestLoad_RehydratesMessageUsage(t *testing.T) {
	tmp := t.TempDir()
	s, err := OpenAt(filepath.Join(tmp, "u.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	w, err := NewSession(s, "m", "p", tmp)
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	sid := w.SessionID()

	// Three turns: user, asst (with usage), user, asst (with usage).
	if err := w.AppendMessage(llm.Message{Role: "user", Content: "first"}, ""); err != nil {
		t.Fatalf("u1: %v", err)
	}
	if err := w.AppendMessage(llm.Message{Role: "assistant", Content: "ack1"}, ""); err != nil {
		t.Fatalf("a1: %v", err)
	}
	u1 := llm.MessageUsage{InputTokens: 10, OutputTokens: 4, TotalTokens: 14}
	if err := w.AppendMessageUsage(u1, ""); err != nil {
		t.Fatalf("usage1: %v", err)
	}
	if err := w.AppendMessage(llm.Message{Role: "user", Content: "second"}, ""); err != nil {
		t.Fatalf("u2: %v", err)
	}
	if err := w.AppendMessage(llm.Message{Role: "assistant", Content: "ack2"}, ""); err != nil {
		t.Fatalf("a2: %v", err)
	}
	u2 := llm.MessageUsage{InputTokens: 20, OutputTokens: 8, TotalTokens: 28}
	if err := w.AppendMessageUsage(u2, ""); err != nil {
		t.Fatalf("usage2: %v", err)
	}

	state, err := Load(s, sid)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(state.History) != 4 {
		t.Fatalf("history len = %d, want 4", len(state.History))
	}
	if len(state.MessageUsage) != 2 {
		t.Errorf("MessageUsage len = %d, want 2", len(state.MessageUsage))
	}
	// Assistant messages are at History indices 1 and 3 (0=user, 1=asst,
	// 2=user, 3=asst). Verify both rows mapped to those indices.
	if got, ok := state.MessageUsage[1]; !ok || got != u1 {
		t.Errorf("MessageUsage[1] = %+v ok=%v, want %+v", got, ok, u1)
	}
	if got, ok := state.MessageUsage[3]; !ok || got != u2 {
		t.Errorf("MessageUsage[3] = %+v ok=%v, want %+v", got, ok, u2)
	}
	// LastUsage tracks the highest-seq row.
	if state.LastUsage == nil || *state.LastUsage != u2 {
		t.Errorf("LastUsage = %+v, want %+v", state.LastUsage, u2)
	}
}

// TestLoad_NoUsageRows confirms that resuming a pre-real-token-accounting
// session (no message_usage rows) gives empty MessageUsage and nil
// LastUsage rather than failing the load.
func TestLoad_NoUsageRows(t *testing.T) {
	tmp := t.TempDir()
	s, err := OpenAt(filepath.Join(tmp, "u.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	w, err := NewSession(s, "m", "p", tmp)
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	if err := w.AppendMessage(llm.Message{Role: "user", Content: "x"}, ""); err != nil {
		t.Fatalf("append: %v", err)
	}

	state, err := Load(s, w.SessionID())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(state.MessageUsage) != 0 {
		t.Errorf("MessageUsage = %+v, want empty", state.MessageUsage)
	}
	if state.LastUsage != nil {
		t.Errorf("LastUsage = %+v, want nil", state.LastUsage)
	}
}

// TestAppendMessageUsage_NoOpWithoutMessage ensures that calling
// AppendMessageUsage on a fresh writer (no AppendMessage yet) is a
// safe no-op — the documented contract on tools.SessionWriter.
func TestAppendMessageUsage_NoOpWithoutMessage(t *testing.T) {
	tmp := t.TempDir()
	s, err := OpenAt(filepath.Join(tmp, "u.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	w, err := NewSession(s, "m", "p", tmp)
	if err != nil {
		t.Fatalf("new session: %v", err)
	}

	if err := w.AppendMessageUsage(llm.MessageUsage{InputTokens: 99}, ""); err != nil {
		t.Errorf("AppendMessageUsage on fresh writer should be no-op, got: %v", err)
	}

	var n int
	if err := s.DB.QueryRow(
		`SELECT COUNT(*) FROM message_usage`,
	).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("rows = %d, want 0 (no orphan inserts)", n)
	}
}
