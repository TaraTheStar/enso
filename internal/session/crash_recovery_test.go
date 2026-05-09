// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/TaraTheStar/enso/internal/llm"
)

// TestCrashMidToolCall_ResumeProducesValidHistory simulates the most
// common crash scenario the codebase has to recover from: enso is
// killed (SIGKILL, OOM, power loss) while a `bash` tool is running,
// AFTER the assistant message with tool_calls has been persisted but
// BEFORE the tool reply lands. On resume, Load + backfillInterrupted
// must produce a history the model can continue from — assistant
// tool_call followed by a tool reply (synthetic "interrupted").
//
// Without this synthetic reply, the next chat completion would get a
// 400 from the API: "messages with tool_calls must be followed by
// tool messages". This test locks in that recovery.
func TestCrashMidToolCall_ResumeProducesValidHistory(t *testing.T) {
	tmp := t.TempDir()
	s, err := OpenAt(filepath.Join(tmp, "crash.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	w, err := NewSession(s, "model-x", "prov-y", "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	id := w.SessionID()

	// Up to but NOT including the tool reply — exactly the on-disk
	// state if the process died mid-bash.
	if err := w.AppendMessage(llm.Message{Role: "user", Content: "list files"}, ""); err != nil {
		t.Fatal(err)
	}
	asst := mkAsstWithCalls("running ls", "call_abc")
	asst.ToolCalls[0].Function.Name = "bash"
	asst.ToolCalls[0].Function.Arguments = `{"cmd":"ls"}`
	if err := w.AppendMessage(asst, ""); err != nil {
		t.Fatal(err)
	}

	// Resume: Load reads the persisted rows and backfills synthetic
	// tool replies for any tool_call without a matching tool message.
	state, err := Load(s, id)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if !state.Interrupted {
		t.Errorf("expected Interrupted=true on mid-tool-call crash, got false")
	}

	// History order must remain: user, assistant(tool_calls), [synthetic tool].
	if len(state.History) < 3 {
		t.Fatalf("history too short: %d, want >=3", len(state.History))
	}
	last := state.History[len(state.History)-1]
	if last.Role != "tool" {
		t.Errorf("last message role=%q, want 'tool' (synthetic reply)", last.Role)
	}
	if last.ToolCallID != "call_abc" {
		t.Errorf("last message ToolCallID=%q, want 'call_abc'", last.ToolCallID)
	}
	if !strings.Contains(last.Content, "interrupted") {
		t.Errorf("synthetic reply should mention interruption: %q", last.Content)
	}
}

// TestCrashMidToolCall_PartialReplies covers the "two tools called,
// one finished writing" race: assistant called bash + read, bash
// reply persisted, read reply didn't. backfill should synthesise
// only the missing reply, leaving the real bash reply alone.
func TestCrashMidToolCall_PartialReplies(t *testing.T) {
	tmp := t.TempDir()
	s, err := OpenAt(filepath.Join(tmp, "partial.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	w, err := NewSession(s, "model-x", "prov-y", "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	id := w.SessionID()

	if err := w.AppendMessage(llm.Message{Role: "user", Content: "do two things"}, ""); err != nil {
		t.Fatal(err)
	}
	asst := mkAsstWithCalls("running both", "call_bash", "call_read")
	if err := w.AppendMessage(asst, ""); err != nil {
		t.Fatal(err)
	}
	// Only the bash reply made it to disk before the crash.
	if err := w.AppendMessage(mkTool("call_bash", "main.go run.go"), ""); err != nil {
		t.Fatal(err)
	}

	state, err := Load(s, id)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Interrupted {
		t.Errorf("expected Interrupted=true with one missing reply")
	}

	var foundReal, foundSynth bool
	for _, m := range state.History {
		if m.Role != "tool" {
			continue
		}
		switch m.ToolCallID {
		case "call_bash":
			foundReal = m.Content == "main.go run.go"
		case "call_read":
			foundSynth = strings.Contains(m.Content, "interrupted")
		}
	}
	if !foundReal {
		t.Errorf("real bash tool reply got clobbered by backfill")
	}
	if !foundSynth {
		t.Errorf("missing read tool reply not synthesised")
	}
}

// TestCrashMidToolCall_RestartCanContinue is the integration sanity
// check: after a crash + Load + backfill, attaching a new Writer and
// appending a new turn must not violate any invariants (ordering,
// tool-call ID uniqueness, etc.). Catches regressions where backfill
// would write into the DB and conflict with later writes.
func TestCrashMidToolCall_RestartCanContinue(t *testing.T) {
	tmp := t.TempDir()
	s, err := OpenAt(filepath.Join(tmp, "restart.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	w, err := NewSession(s, "model-x", "prov-y", "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	id := w.SessionID()
	if err := w.AppendMessage(llm.Message{Role: "user", Content: "first"}, ""); err != nil {
		t.Fatal(err)
	}
	asst := mkAsstWithCalls("running", "call_xyz")
	if err := w.AppendMessage(asst, ""); err != nil {
		t.Fatal(err)
	}

	// Load (which performs in-memory backfill — does NOT write).
	if _, err := Load(s, id); err != nil {
		t.Fatalf("load: %v", err)
	}

	// Restart-style: attach a new writer and continue the conversation.
	w2, err := AttachWriter(s, id)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	if err := w2.AppendMessage(llm.Message{Role: "user", Content: "second"}, ""); err != nil {
		t.Fatalf("append after backfill: %v", err)
	}

	// Reload and confirm the user's "second" message is present and
	// ordered after the (now-stable) earlier turn.
	state, err := Load(s, id)
	if err != nil {
		t.Fatal(err)
	}
	last := state.History[len(state.History)-1]
	if last.Role != "user" || last.Content != "second" {
		t.Errorf("last message=%+v, want user='second'", last)
	}
}
