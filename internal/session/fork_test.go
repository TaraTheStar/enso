// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"path/filepath"
	"testing"

	"github.com/TaraTheStar/enso/internal/llm"
)

func TestForkCopiesMessages(t *testing.T) {
	tmp := t.TempDir()
	s, err := OpenAt(filepath.Join(tmp, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	src, err := NewSession(s, "qwen", "local", "/proj")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []llm.Message{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "hi back"},
		{Role: "user", Content: "what about X?"},
	} {
		if err := src.AppendMessage(m, ""); err != nil {
			t.Fatal(err)
		}
	}

	newID, err := Fork(s, src.SessionID())
	if err != nil {
		t.Fatal(err)
	}
	if newID == src.SessionID() {
		t.Fatalf("Fork returned the same id")
	}

	state, err := Load(s, newID)
	if err != nil {
		t.Fatal(err)
	}
	if state.Info.Model != "qwen" || state.Info.Cwd != "/proj" {
		t.Errorf("metadata not carried over: %+v", state.Info)
	}
	if len(state.History) != 3 {
		t.Fatalf("expected 3 messages in fork, got %d", len(state.History))
	}
	if state.History[0].Content != "hello" || state.History[2].Content != "what about X?" {
		t.Errorf("messages not preserved in order: %+v", state.History)
	}

	// Source session must remain intact.
	srcState, err := Load(s, src.SessionID())
	if err != nil {
		t.Fatal(err)
	}
	if len(srcState.History) != 3 {
		t.Errorf("source session was mutated: %d messages", len(srcState.History))
	}
}

func TestForkRejectsMissingSource(t *testing.T) {
	tmp := t.TempDir()
	s, err := OpenAt(filepath.Join(tmp, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := Fork(s, "no-such-id"); err == nil {
		t.Errorf("expected error for missing source")
	}
}

func TestForkAllowsResumeFromNewID(t *testing.T) {
	tmp := t.TempDir()
	s, err := OpenAt(filepath.Join(tmp, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	src, _ := NewSession(s, "qwen", "local", "/proj")
	_ = src.AppendMessage(llm.Message{Role: "user", Content: "hi"}, "")

	newID, err := Fork(s, src.SessionID())
	if err != nil {
		t.Fatal(err)
	}

	// AttachWriter must succeed against the forked session and pick up
	// where the fork left off (seq=1 for the copied message).
	w, err := AttachWriter(s, newID)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.AppendMessage(llm.Message{Role: "assistant", Content: "follow-up"}, ""); err != nil {
		t.Fatal(err)
	}
}
