// SPDX-License-Identifier: AGPL-3.0-or-later

package bubble

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/TaraTheStar/azoth/llm"
	"github.com/TaraTheStar/enso/internal/bus"
	"github.com/TaraTheStar/enso/internal/ui/blocks"
)

// mkToolCall builds an llm.ToolCall — the nested Function struct is
// awkward to write as a literal, so centralise it for the tests.
func mkToolCall(id, name, args string) llm.ToolCall {
	var tc llm.ToolCall
	tc.ID = id
	tc.Function.Name = name
	tc.Function.Arguments = args
	return tc
}

// TestNoticeEvent_RendersInline: an EventNotice graduates a Notice block
// to scrollback with its text (the image-attach confirmation path).
func TestNoticeEvent_RendersInline(t *testing.T) {
	c := &conversation{}
	out := c.HandleEvent(bus.Event{Type: bus.EventNotice, Payload: "📎 attached shot.png"})
	if len(out) != 1 {
		t.Fatalf("want 1 graduated block, got %d", len(out))
	}
	nb, ok := out[0].(*blocks.Notice)
	if !ok || nb.Text != "📎 attached shot.png" {
		t.Fatalf("want Notice block with the text, got %#v", out[0])
	}
	if rendered := ansi.Strip(renderBlock(nb, 80, true)); !strings.Contains(rendered, "attached shot.png") {
		t.Fatalf("notice did not render its text: %q", rendered)
	}
	// An empty notice produces nothing (no blank line in scrollback).
	if out := c.HandleEvent(bus.Event{Type: bus.EventNotice, Payload: ""}); len(out) != 0 {
		t.Fatalf("empty notice should graduate no blocks, got %d", len(out))
	}
}

// TestHistoryBlocks_ReconstructsToolCalls: a resumed history with a
// tool call + its result row replays as User → Tool (with the call
// signature and output) → Assistant, not just the prose around the call.
func TestHistoryBlocks_ReconstructsToolCalls(t *testing.T) {
	history := []llm.Message{
		{Role: "user", Content: "list the files"},
		{Role: "assistant", ToolCalls: []llm.ToolCall{mkToolCall("call_1", "Bash", `{"command":"ls"}`)}},
		{Role: "tool", ToolCallID: "call_1", Name: "Bash", Content: "file1\nfile2"},
		{Role: "assistant", Content: "Here are your files."},
	}
	got := historyBlocks(history)
	if len(got) != 3 {
		t.Fatalf("want 3 blocks (user, tool, assistant), got %d: %#v", len(got), got)
	}
	if u, ok := got[0].(*blocks.User); !ok || u.Text != "list the files" {
		t.Fatalf("block 0: want User{list the files}, got %#v", got[0])
	}
	tb, ok := got[1].(*blocks.Tool)
	if !ok {
		t.Fatalf("block 1: want Tool, got %#v", got[1])
	}
	if tb.Name != "Bash" || tb.Output != "file1\nfile2" {
		t.Fatalf("tool block: name=%q output=%q", tb.Name, tb.Output)
	}
	if !strings.Contains(tb.Call, "Bash(") || !strings.Contains(tb.Call, "command=ls") {
		t.Fatalf("tool call signature not formatted like the live path: %q", tb.Call)
	}
	// Replayed tool blocks never animate as "running" (StartedAt zero).
	if tb.Running() {
		t.Fatalf("replayed tool block should not report Running()")
	}
	if a, ok := got[2].(*blocks.Assistant); !ok || a.Text != "Here are your files." {
		t.Fatalf("block 2: want Assistant{Here are your files.}, got %#v", got[2])
	}
}

// TestHistoryBlocks_ReplaysReasoning: a persisted assistant turn with
// reasoning replays a Reasoning block ABOVE its prose, matching the live
// stream order (reasoning graduates above the response).
func TestHistoryBlocks_ReplaysReasoning(t *testing.T) {
	history := []llm.Message{
		{Role: "user", Content: "solve it"},
		{Role: "assistant", Content: "the answer is 4", Reasoning: "2+2 is 4"},
	}
	got := historyBlocks(history)
	if len(got) != 3 {
		t.Fatalf("want 3 blocks (user, reasoning, assistant), got %d: %#v", len(got), got)
	}
	r, ok := got[1].(*blocks.Reasoning)
	if !ok {
		t.Fatalf("block 1: want Reasoning, got %#v", got[1])
	}
	if r.Text != "2+2 is 4" || !r.Closed {
		t.Fatalf("reasoning block: text=%q closed=%v", r.Text, r.Closed)
	}
	if a, ok := got[2].(*blocks.Assistant); !ok || a.Text != "the answer is 4" {
		t.Fatalf("block 2: want Assistant after Reasoning, got %#v", got[2])
	}
}

// TestHistoryBlocks_ReasoningThenToolNoProse: a reasoning-only turn that
// went straight to a tool call replays Reasoning → Tool (no empty
// Assistant block in between).
func TestHistoryBlocks_ReasoningThenToolNoProse(t *testing.T) {
	history := []llm.Message{
		{Role: "assistant", Reasoning: "I should list files", ToolCalls: []llm.ToolCall{mkToolCall("c1", "Bash", `{"command":"ls"}`)}},
		{Role: "tool", ToolCallID: "c1", Content: "a\nb"},
	}
	got := historyBlocks(history)
	if len(got) != 2 {
		t.Fatalf("want 2 blocks (reasoning, tool), got %d: %#v", len(got), got)
	}
	if _, ok := got[0].(*blocks.Reasoning); !ok {
		t.Fatalf("block 0: want Reasoning, got %#v", got[0])
	}
	if _, ok := got[1].(*blocks.Tool); !ok {
		t.Fatalf("block 1: want Tool, got %#v", got[1])
	}
}

// TestHistoryBlocks_TextAndToolSameTurn: an assistant message carrying
// BOTH prose and a tool call emits the prose first, then the tool block
// — matching the live event order.
func TestHistoryBlocks_TextAndToolSameTurn(t *testing.T) {
	history := []llm.Message{
		{Role: "assistant", Content: "let me check", ToolCalls: []llm.ToolCall{mkToolCall("c1", "Read", `{"path":"x"}`)}},
		{Role: "tool", ToolCallID: "c1", Content: "contents"},
	}
	got := historyBlocks(history)
	if len(got) != 2 {
		t.Fatalf("want 2 blocks (assistant text + tool), got %d: %#v", len(got), got)
	}
	if a, ok := got[0].(*blocks.Assistant); !ok || a.Text != "let me check" {
		t.Fatalf("block 0 should be the assistant prose, got %#v", got[0])
	}
	if tb, ok := got[1].(*blocks.Tool); !ok || tb.Output != "contents" {
		t.Fatalf("block 1 should be the Tool with its output, got %#v", got[1])
	}
}

// TestHistoryBlocks_ToolCallWithoutResult: an interrupted turn (tool
// call with no matching result row) still replays the call, with empty
// output rather than being dropped.
func TestHistoryBlocks_ToolCallWithoutResult(t *testing.T) {
	history := []llm.Message{
		{Role: "assistant", ToolCalls: []llm.ToolCall{mkToolCall("c1", "Bash", `{"command":"sleep 100"}`)}},
	}
	got := historyBlocks(history)
	if len(got) != 1 {
		t.Fatalf("want 1 tool block, got %d", len(got))
	}
	if tb, ok := got[0].(*blocks.Tool); !ok || tb.Output != "" || tb.Name != "Bash" {
		t.Fatalf("want Tool{Bash, empty output}, got %#v", got[0])
	}
}

// TestHistoryBlocks_SkipsEmptyAndNonReplayedRows: whitespace-only prose,
// empty assistant turns, standalone tool rows, and system rows produce
// no blocks.
func TestHistoryBlocks_SkipsEmptyAndNonReplayedRows(t *testing.T) {
	history := []llm.Message{
		{Role: "user", Content: "   "},                     // whitespace-only → skipped
		{Role: "assistant", Content: ""},                   // empty → skipped
		{Role: "tool", ToolCallID: "orphan", Content: "x"}, // consumed into a (missing) call, not emitted alone
		{Role: "system", Content: "sys"},                   // not replayed
	}
	if got := historyBlocks(history); len(got) != 0 {
		t.Fatalf("want 0 blocks, got %d: %#v", len(got), got)
	}
}

// TestToolBlockFromCall_BadArguments: malformed JSON arguments degrade
// to a bare "name()" signature instead of panicking or dropping the call.
func TestToolBlockFromCall_BadArguments(t *testing.T) {
	tb := toolBlockFromCall(mkToolCall("c1", "Weird", `{not valid json`), "out")
	if tb.Name != "Weird" || tb.Output != "out" {
		t.Fatalf("name/output not preserved: %#v", tb)
	}
	if tb.Call != "Weird()" {
		t.Fatalf("bad-args call should fall back to Weird(); got %q", tb.Call)
	}
}
