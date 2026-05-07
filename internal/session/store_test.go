// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/TaraTheStar/enso/internal/llm"
)

func TestStore_OpenAtAppliesMigrations(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "test.db")

	s, err := OpenAt(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	// Tables created by migrations are accessible.
	for _, table := range []string{"sessions", "messages", "tool_calls", "events"} {
		var n int
		if err := s.DB.QueryRow(
			`SELECT COUNT(*) FROM ` + table,
		).Scan(&n); err != nil {
			t.Errorf("table %s: %v", table, err)
		}
	}
}

func TestStore_MigrationsIdempotent(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "test.db")

	s, err := OpenAt(path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	s.Close()

	// Re-opening on the same DB must not error (CREATE TABLE IF NOT EXISTS).
	s2, err := OpenAt(path)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	defer s2.Close()
}

func TestNewSessionAndAppend_Roundtrip(t *testing.T) {
	tmp := t.TempDir()
	s, err := OpenAt(filepath.Join(tmp, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	w, err := NewSession(s, "model-x", "prov-y", "/some/cwd")
	if err != nil {
		t.Fatalf("new session: %v", err)
	}

	// Append a couple of messages.
	if err := w.AppendMessage(llm.Message{Role: "user", Content: "hi"}, ""); err != nil {
		t.Fatalf("append user: %v", err)
	}
	if err := w.AppendMessage(llm.Message{Role: "assistant", Content: "hello"}, ""); err != nil {
		t.Fatalf("append asst: %v", err)
	}

	// Persist a tool call + event for the new tables.
	if err := w.AppendToolCall("call_1", "read", map[string]any{"path": "x.go"}, "ok", "full out", "ok"); err != nil {
		t.Fatalf("append tool: %v", err)
	}
	if err := w.AppendEvent("UserMessage", "hi"); err != nil {
		t.Fatalf("append event: %v", err)
	}

	// Resume and check shape.
	state, err := Load(s, w.SessionID())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(state.History) != 2 {
		t.Errorf("history len = %d, want 2", len(state.History))
	}
	if state.History[0].Content != "hi" || state.History[1].Content != "hello" {
		t.Errorf("history mismatch: %+v", state.History)
	}

	// Tool call row exists.
	var nTC int
	if err := s.DB.QueryRow(
		`SELECT COUNT(*) FROM tool_calls WHERE session_id = ?`, w.SessionID(),
	).Scan(&nTC); err != nil {
		t.Fatal(err)
	}
	if nTC != 1 {
		t.Errorf("tool_calls count = %d, want 1", nTC)
	}

	// Event row exists.
	var nEv int
	if err := s.DB.QueryRow(
		`SELECT COUNT(*) FROM events WHERE session_id = ?`, w.SessionID(),
	).Scan(&nEv); err != nil {
		t.Fatal(err)
	}
	if nEv != 1 {
		t.Errorf("events count = %d, want 1", nEv)
	}
}

func TestAttachWriter_SeedsSeqFromMaxes(t *testing.T) {
	tmp := t.TempDir()
	s, err := OpenAt(filepath.Join(tmp, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	w1, err := NewSession(s, "m", "p", "/c")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		_ = w1.AppendMessage(llm.Message{Role: "user", Content: "msg"}, "")
	}
	_ = w1.AppendToolCall("c1", "read", nil, "ok", "ok", "ok")
	_ = w1.AppendEvent("UserMessage", nil)

	// Reattach: subsequent appends should not collide on the PRIMARY KEY.
	w2, err := AttachWriter(s, w1.SessionID())
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	if err := w2.AppendMessage(llm.Message{Role: "user", Content: "after attach"}, ""); err != nil {
		t.Errorf("append after attach: %v", err)
	}
	if err := w2.AppendToolCall("c2", "read", nil, "ok", "ok", "ok"); err != nil {
		t.Errorf("append tool after attach: %v", err)
	}
	if err := w2.AppendEvent("UserMessage", nil); err != nil {
		t.Errorf("append event after attach: %v", err)
	}
}

func TestSubAgentMessagesPersistAndAreFiltered(t *testing.T) {
	tmp := t.TempDir()
	s, err := OpenAt(filepath.Join(tmp, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	w, err := NewSession(s, "m", "p", "/c")
	if err != nil {
		t.Fatal(err)
	}

	// Top-level user message + assistant.
	if err := w.AppendMessage(llm.Message{Role: "user", Content: "do thing"}, ""); err != nil {
		t.Fatalf("top user: %v", err)
	}
	if err := w.AppendMessage(llm.Message{Role: "assistant", Content: "ok"}, ""); err != nil {
		t.Fatalf("top asst: %v", err)
	}

	// Sub-agent transcript with its own AgentID.
	const subID = "sub-abc12345"
	if err := w.AppendMessage(llm.Message{Role: "user", Content: "sub prompt"}, subID); err != nil {
		t.Fatalf("sub user: %v", err)
	}
	if err := w.AppendMessage(llm.Message{Role: "assistant", Content: "sub answer"}, subID); err != nil {
		t.Fatalf("sub asst: %v", err)
	}

	// Top-level Load filters out sub-agent rows.
	state, err := Load(s, w.SessionID())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(state.History) != 2 {
		t.Errorf("top history len = %d, want 2 (sub-agent rows must be filtered out)", len(state.History))
	}
	for _, m := range state.History {
		if strings.Contains(m.Content, "sub ") {
			t.Errorf("sub-agent leaked into top-level resume: %+v", m)
		}
	}

	// LoadAgentTranscript pulls the sub-agent's history.
	subHist, err := LoadAgentTranscript(s, w.SessionID(), subID)
	if err != nil {
		t.Fatalf("load transcript: %v", err)
	}
	if len(subHist) != 2 {
		t.Errorf("sub transcript len = %d, want 2", len(subHist))
	}
	if subHist[0].Content != "sub prompt" || subHist[1].Content != "sub answer" {
		t.Errorf("sub transcript mismatch: %+v", subHist)
	}
}

func TestListRecent(t *testing.T) {
	tmp := t.TempDir()
	s, err := OpenAt(filepath.Join(tmp, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	for i := 0; i < 3; i++ {
		if _, err := NewSession(s, "m", "p", "/c"); err != nil {
			t.Fatalf("new session: %v", err)
		}
	}
	infos, err := ListRecent(s, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(infos) != 3 {
		t.Errorf("got %d, want 3", len(infos))
	}
}
