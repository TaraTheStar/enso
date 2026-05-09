// SPDX-License-Identifier: AGPL-3.0-or-later

package bubble

import (
	"errors"
	"strings"
	"testing"

	"github.com/TaraTheStar/enso/internal/bus"
	"github.com/TaraTheStar/enso/internal/ui/blocks"
)

// TestStreamingAccumulates: AssistantDelta payloads accumulate in the
// in-flight block; nothing graduates to scrollback yet. (The first
// delta does return a Cmd — the spinner tick that drives the bottom
// indicator — but no Println, which we verify indirectly by checking
// nothing landed in conversation history.)
func TestStreamingAccumulates(t *testing.T) {
	m := &model{}
	for _, chunk := range []string{"hello ", "world", "!"} {
		out, _ := m.handleBusEvent(bus.Event{Type: bus.EventAssistantDelta, Payload: chunk})
		if out != m {
			t.Fatalf("model identity changed unexpectedly")
		}
	}
	if h := m.conv.History(); len(h) != 0 {
		t.Fatalf("nothing should have graduated yet; got %d history entries", len(h))
	}
	a, ok := m.conv.Live().(*blocks.Assistant)
	if !ok {
		t.Fatalf("live block not Assistant: %T", m.conv.Live())
	}
	if got, want := a.Text, "hello world!"; got != want {
		t.Fatalf("assistant text = %q, want %q", got, want)
	}
}

// TestAssistantDoneGraduates: a completed message graduates to
// scrollback (returns a non-nil Cmd) and clears the live block.
func TestAssistantDoneGraduates(t *testing.T) {
	m := &model{}
	m.handleBusEvent(bus.Event{Type: bus.EventAssistantDelta, Payload: "partial reply"})

	_, cmd := m.handleBusEvent(bus.Event{Type: bus.EventAssistantDone})
	if cmd == nil {
		t.Fatal("done must return a Cmd that prints to scrollback")
	}
	if m.conv.Live() != nil {
		t.Fatalf("live block not cleared after done: %T", m.conv.Live())
	}
}

// TestAssistantDoneEmpty: a done event with nothing buffered (e.g. a
// completion that produced only a tool call) must NOT print an empty
// line to scrollback.
func TestAssistantDoneEmpty(t *testing.T) {
	m := &model{}
	_, cmd := m.handleBusEvent(bus.Event{Type: bus.EventAssistantDone})
	if cmd != nil {
		t.Fatalf("done with no live block should be a no-op, got Cmd")
	}
}

// TestToolCallStartFlushesAssistantBlock: when a tool call begins
// during a partial assistant reply, the assistant block must graduate
// before the new tool block becomes live — otherwise scrollback order
// no longer matches the agent's output sequence.
func TestToolCallStartFlushesAssistantBlock(t *testing.T) {
	m := &model{}
	m.handleBusEvent(bus.Event{Type: bus.EventAssistantDelta, Payload: "let me check"})

	_, cmd := m.handleBusEvent(bus.Event{
		Type:    bus.EventToolCallStart,
		Payload: map[string]any{"name": "Bash", "id": "1", "args": map[string]any{}},
	})
	if cmd == nil {
		t.Fatal("tool start must graduate the in-flight assistant block")
	}
	if _, ok := m.conv.Live().(*blocks.Tool); !ok {
		t.Fatalf("live block not Tool after start: %T", m.conv.Live())
	}
}

// TestErrorResetsLive: an error event during a live stream drops the
// partial message (it's incoherent) and surfaces an error block.
func TestErrorResetsLive(t *testing.T) {
	m := &model{}
	m.handleBusEvent(bus.Event{Type: bus.EventAssistantDelta, Payload: "midway through"})

	_, cmd := m.handleBusEvent(bus.Event{Type: bus.EventError, Payload: errors.New("boom")})
	if cmd == nil {
		t.Fatal("error must return a Cmd")
	}
	if m.conv.Live() != nil {
		t.Fatalf("live block not cleared on error: %T", m.conv.Live())
	}
}

// TestErrorAcceptsStringPayload: some publishers send error payloads
// as strings, not error values; both must produce a graduated Error
// block.
func TestErrorAcceptsStringPayload(t *testing.T) {
	m := &model{}
	_, cmd := m.handleBusEvent(bus.Event{Type: bus.EventError, Payload: "stringy error"})
	if cmd == nil {
		t.Fatal("error with string payload must still produce a Cmd")
	}
}

// TestToolCallStartUpdatesStatus: the model's status line populates
// with the tool name so the user sees what's running during otherwise-
// silent tool execution.
func TestToolCallStartUpdatesStatus(t *testing.T) {
	m := &model{}
	m.handleBusEvent(bus.Event{
		Type:    bus.EventToolCallStart,
		Payload: map[string]any{"name": "Bash", "id": "1", "args": map[string]any{}},
	})
	if !strings.Contains(m.statusLine, "Bash") {
		t.Fatalf("status line missing tool name: %q", m.statusLine)
	}
}

// TestToolCallEndClearsStatus: when a tool call finishes the live
// status returns to idle so the model name shows again.
func TestToolCallEndClearsStatus(t *testing.T) {
	m := &model{statusLine: "running Bash"}
	m.handleBusEvent(bus.Event{Type: bus.EventToolCallStart, Payload: map[string]any{"name": "Bash", "id": "1"}})
	m.handleBusEvent(bus.Event{
		Type:    bus.EventToolCallEnd,
		Payload: map[string]any{"name": "Bash", "id": "1", "result": "done"},
	})
	if m.statusLine != "" {
		t.Fatalf("status line should be cleared after tool end, got %q", m.statusLine)
	}
}

// TestReasoningSurvivesAssistantDone: a single LLM completion may emit
// only reasoning then no content; AssistantDone must NOT graduate a
// reasoning block (the reasoning continues across tool-call rounds
// until something non-reasoning arrives).
func TestReasoningSurvivesAssistantDone(t *testing.T) {
	m := &model{}
	m.handleBusEvent(bus.Event{Type: bus.EventReasoningDelta, Payload: "thinking..."})
	if _, ok := m.conv.Live().(*blocks.Reasoning); !ok {
		t.Fatalf("live block not Reasoning after delta: %T", m.conv.Live())
	}
	_, cmd := m.handleBusEvent(bus.Event{Type: bus.EventAssistantDone})
	if cmd != nil {
		t.Fatalf("AssistantDone should not graduate a Reasoning block, got Cmd")
	}
	if _, ok := m.conv.Live().(*blocks.Reasoning); !ok {
		t.Fatalf("Reasoning block lost after AssistantDone: %T", m.conv.Live())
	}
}

// TestRenderMarkdownThemeBuilds: the custom glamour theme must build
// successfully and produce non-empty output for trivial markdown
// without panicking. Catches regressions in buildMarkdownTheme (e.g.
// missing palette key, malformed StyleConfig) at test time rather
// than the first time an assistant block graduates in the live UI.
func TestRenderMarkdownThemeBuilds(t *testing.T) {
	out := renderMarkdown("# heading\n\nbody **bold** and `code`.\n", 80)
	if out == "" {
		t.Fatal("renderMarkdown returned empty output for non-empty input")
	}
	if strings.Contains(out, "**bold**") {
		t.Errorf("markdown bold marker not consumed; output still contains literal **bold**: %q", out)
	}
}

// TestBackspaceRespectsUTF8: backspace must drop a rune, not a byte;
// otherwise multi-byte characters get split into invalid sequences.
// Cursor moves back by the rune width.
func TestBackspaceRespectsUTF8(t *testing.T) {
	cases := []struct {
		in      string
		wantBuf string
		wantPos int
	}{
		{"hello", "hell", 4},
		{"café", "caf", 3},
		{"\U0001F600", "", 0},
		{"", "", 0},
	}
	for _, tc := range cases {
		s := &inputState{buf: tc.in, cursor: len(tc.in)}
		s.backspace()
		if s.buf != tc.wantBuf {
			t.Errorf("backspace(%q): buf=%q want %q", tc.in, s.buf, tc.wantBuf)
		}
		if s.cursor != tc.wantPos {
			t.Errorf("backspace(%q): cursor=%d want %d", tc.in, s.cursor, tc.wantPos)
		}
	}
}
