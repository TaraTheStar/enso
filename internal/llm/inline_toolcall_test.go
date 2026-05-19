// SPDX-License-Identifier: AGPL-3.0-or-later

package llm

import (
	"encoding/json"
	"testing"
)

// The exact shape from the bug report: Qwen3 on llama.cpp leaking a
// pseudo-XML tool call into the assistant text.
func TestParseInlineToolCalls_ReportedQwenXML(t *testing.T) {
	in := "thinking...\n<tool_call>\n<function=read>\n<parameter=path>\n" +
		"/home/user/go/src/github.com/TaraTheStar/enso/README.md\n</parameter>\n" +
		"<parameter=first_line>\n63\n</parameter>\n" +
		"<parameter=last_line>\n100\n</parameter>\n</function>\n</tool_call>"

	cleaned, calls := ParseInlineToolCalls(in)
	if len(calls) != 1 {
		t.Fatalf("want 1 recovered call, got %d", len(calls))
	}
	c := calls[0]
	if c.Function.Name != "read" {
		t.Errorf("name = %q, want read", c.Function.Name)
	}
	if c.ID == "" || c.Type != "function" {
		t.Errorf("call must have id+type, got id=%q type=%q", c.ID, c.Type)
	}
	if cleaned != "thinking..." {
		t.Errorf("surrounding prose must survive, markup stripped; got %q", cleaned)
	}

	var args map[string]any
	if err := json.Unmarshal([]byte(c.Function.Arguments), &args); err != nil {
		t.Fatalf("arguments must be valid JSON: %v (%s)", err, c.Function.Arguments)
	}
	if args["path"] != "/home/user/go/src/github.com/TaraTheStar/enso/README.md" {
		t.Errorf("path = %v", args["path"])
	}
	// Numeric params must coerce to JSON numbers, not the string "63",
	// or a schema expecting an int rejects the call.
	if f, ok := args["first_line"].(float64); !ok || f != 63 {
		t.Errorf("first_line = %v (%T), want number 63", args["first_line"], args["first_line"])
	}
	if f, ok := args["last_line"].(float64); !ok || f != 100 {
		t.Errorf("last_line = %v (%T), want number 100", args["last_line"], args["last_line"])
	}
}

func TestParseInlineToolCalls_JSONShape(t *testing.T) {
	in := `ok<tool_call>{"name":"bash","arguments":{"cmd":"ls","timeout":30}}</tool_call>`
	cleaned, calls := ParseInlineToolCalls(in)
	if len(calls) != 1 || calls[0].Function.Name != "bash" {
		t.Fatalf("want 1 bash call, got %+v", calls)
	}
	if cleaned != "ok" {
		t.Errorf("cleaned = %q, want %q", cleaned, "ok")
	}
	var a map[string]any
	if err := json.Unmarshal([]byte(calls[0].Function.Arguments), &a); err != nil {
		t.Fatalf("bad args json: %v", err)
	}
	if a["cmd"] != "ls" {
		t.Errorf("cmd = %v", a["cmd"])
	}
}

func TestParseInlineToolCalls_BareFunctionNoWrapper(t *testing.T) {
	in := "<function=list_dir><parameter=path>.</parameter></function>"
	_, calls := ParseInlineToolCalls(in)
	if len(calls) != 1 || calls[0].Function.Name != "list_dir" {
		t.Fatalf("bare <function=> must parse, got %+v", calls)
	}
}

func TestParseInlineToolCalls_MultipleCalls(t *testing.T) {
	in := "<tool_call><function=a><parameter=x>1</parameter></function></tool_call>" +
		"<tool_call><function=b><parameter=y>z</parameter></function></tool_call>"
	_, calls := ParseInlineToolCalls(in)
	if len(calls) != 2 {
		t.Fatalf("want 2 calls, got %d", len(calls))
	}
	if calls[0].ID == calls[1].ID {
		t.Errorf("recovered calls must have distinct ids, both %q", calls[0].ID)
	}
}

func TestParseInlineToolCalls_NoToolCallIsPassthrough(t *testing.T) {
	in := "Just a normal answer with a < less-than sign and no calls."
	cleaned, calls := ParseInlineToolCalls(in)
	if calls != nil {
		t.Errorf("must not invent calls, got %+v", calls)
	}
	if cleaned != in {
		t.Errorf("content must be returned unchanged when nothing parses")
	}
}

func TestParseInlineToolCalls_EmptyArgs(t *testing.T) {
	in := "<tool_call><function=help></function></tool_call>"
	_, calls := ParseInlineToolCalls(in)
	if len(calls) != 1 {
		t.Fatalf("want 1 call, got %d", len(calls))
	}
	if calls[0].Function.Arguments != "{}" {
		t.Errorf("no params must yield {}, got %q", calls[0].Function.Arguments)
	}
}
