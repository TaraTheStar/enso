// SPDX-License-Identifier: AGPL-3.0-or-later

package llm

import (
	"encoding/json"
	"testing"
)

// TestChunkEnvelope_DeltaUnwraps regression-tests the bug where chunks were
// decoded directly into ChatResponseDelta instead of ChatCompletionChunk,
// silently dropping all assistant content.
func TestChunkEnvelope_DeltaUnwraps(t *testing.T) {
	raw := []byte(`{
		"id": "chatcmpl-x",
		"object": "chat.completion.chunk",
		"choices": [
			{ "index": 0, "delta": { "role": "assistant", "content": "hello world" } }
		]
	}`)
	var chunk ChatCompletionChunk
	if err := json.Unmarshal(raw, &chunk); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(chunk.Choices) != 1 {
		t.Fatalf("got %d choices, want 1", len(chunk.Choices))
	}
	if chunk.Choices[0].Delta.Content != "hello world" {
		t.Errorf("content = %q, want %q", chunk.Choices[0].Delta.Content, "hello world")
	}
	// And confirm a tool-call chunk also unwraps correctly.
	rawTool := []byte(`{
		"choices": [
			{ "index": 0, "delta": { "tool_calls": [
				{ "index": 0, "id": "call_1", "function": { "name": "read", "arguments": "{}" } }
			] } }
		]
	}`)
	var toolChunk ChatCompletionChunk
	if err := json.Unmarshal(rawTool, &toolChunk); err != nil {
		t.Fatalf("unmarshal tool: %v", err)
	}
	if len(toolChunk.Choices[0].Delta.ToolCalls) != 1 {
		t.Fatalf("got %d tool calls, want 1", len(toolChunk.Choices[0].Delta.ToolCalls))
	}
}

func TestAccumulator_NameAndArgsSplitAcrossDeltas(t *testing.T) {
	acc := NewToolCallAccumulator()

	mustMerge(t, acc, ChatResponseDelta{ToolCalls: []DTCall{{
		Index: 0, ID: "call_1",
		Function: dtFunction("read", `{"pa`),
	}}})
	// Subsequent deltas may omit the id; index keeps them tied.
	mustMerge(t, acc, ChatResponseDelta{ToolCalls: []DTCall{{
		Index:    0,
		Function: dtFunction("_file", `th": "README.md"}`),
	}}})

	calls := acc.Finalize()
	if len(calls) != 1 {
		t.Fatalf("got %d calls, want 1", len(calls))
	}
	c := calls[0]
	if c.ID != "call_1" {
		t.Errorf("id = %q, want call_1", c.ID)
	}
	if c.Function.Name != "read_file" {
		t.Errorf("name = %q, want read_file", c.Function.Name)
	}
	if c.Function.Arguments != `{"path": "README.md"}` {
		t.Errorf("args = %q, want full json", c.Function.Arguments)
	}
}

func TestAccumulator_ArgumentsAsObjectGetMarshalled(t *testing.T) {
	// llama.cpp #20198: function.arguments may stream as a JSON object instead
	// of a string. The accumulator must marshal it back to its string form so
	// downstream JSON parsing of args still works.
	acc := NewToolCallAccumulator()
	mustMerge(t, acc, ChatResponseDelta{ToolCalls: []DTCall{{
		Index: 0, ID: "call_1",
		Function: dtFunctionRaw("write", map[string]any{"path": "x", "content": "y"}),
	}}})

	calls := acc.Finalize()
	if len(calls) != 1 {
		t.Fatalf("got %d calls, want 1", len(calls))
	}
	got := calls[0].Function.Arguments
	// Map iteration is unordered, so compare both possible key orderings.
	want1 := `{"content":"y","path":"x"}`
	want2 := `{"path":"x","content":"y"}`
	if got != want1 && got != want2 {
		t.Errorf("args = %q, want one of [%s, %s]", got, want1, want2)
	}
}

func TestAccumulator_ParallelCallsKeyedByIndex(t *testing.T) {
	acc := NewToolCallAccumulator()

	// Two calls interleaved across deltas, distinguished by index.
	mustMerge(t, acc, ChatResponseDelta{ToolCalls: []DTCall{
		{Index: 0, ID: "a", Function: dtFunction("read", `{"path":"a.txt"}`)},
		{Index: 1, ID: "b", Function: dtFunction("read", `{"pa`)},
	}})
	mustMerge(t, acc, ChatResponseDelta{ToolCalls: []DTCall{
		{Index: 1, Function: dtFunction("", `th":"b.txt"}`)},
	}})

	calls := acc.Finalize()
	if len(calls) != 2 {
		t.Fatalf("got %d calls, want 2", len(calls))
	}

	byID := map[string]ToolCall{}
	for _, c := range calls {
		byID[c.ID] = c
	}
	if got := byID["a"].Function.Arguments; got != `{"path":"a.txt"}` {
		t.Errorf("call a args = %q", got)
	}
	if got := byID["b"].Function.Arguments; got != `{"path":"b.txt"}` {
		t.Errorf("call b args = %q", got)
	}
}

func TestAccumulator_FinalizeDropsIncomplete(t *testing.T) {
	acc := NewToolCallAccumulator()
	// Index 0: complete. Index 1: name only, no id. Index 2: id only, no name.
	mustMerge(t, acc, ChatResponseDelta{ToolCalls: []DTCall{
		{Index: 0, ID: "ok", Function: dtFunction("read", `{}`)},
		{Index: 1, Function: dtFunction("read", `{}`)},
		{Index: 2, ID: "no_name"},
	}})

	calls := acc.Finalize()
	if len(calls) != 1 {
		t.Fatalf("got %d calls, want 1 (only the complete one)", len(calls))
	}
	if calls[0].ID != "ok" {
		t.Errorf("survivor id = %q, want ok", calls[0].ID)
	}
}

func TestCoerceArguments(t *testing.T) {
	cases := []struct {
		name string
		in   any
		out  string
	}{
		{"string passes through", `{"x":1}`, `{"x":1}`},
		{"nil becomes empty", nil, ""},
		{"object marshalled", map[string]any{"x": 1.0}, `{"x":1}`},
		{"array marshalled", []any{1.0, 2.0}, `[1,2]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := coerceArguments(tc.in)
			if err != nil {
				t.Fatalf("coerce: %v", err)
			}
			if got != tc.out {
				t.Errorf("got %q, want %q", got, tc.out)
			}
		})
	}
}

// helpers

func mustMerge(t *testing.T, acc *ToolCallAccumulator, d ChatResponseDelta) {
	t.Helper()
	if err := acc.Merge(d); err != nil {
		t.Fatalf("merge: %v", err)
	}
}

func dtFunction(name, args string) (f struct {
	Name      string      `json:"name,omitempty"`
	Arguments interface{} `json:"arguments,omitempty"`
}) {
	f.Name = name
	if args != "" {
		f.Arguments = args
	}
	return f
}

func dtFunctionRaw(name string, args any) (f struct {
	Name      string      `json:"name,omitempty"`
	Arguments interface{} `json:"arguments,omitempty"`
}) {
	f.Name = name
	f.Arguments = args
	return f
}
