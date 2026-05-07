// SPDX-License-Identifier: AGPL-3.0-or-later

package score

import (
	"strings"
	"testing"
)

func TestParseStream_TypicalRun(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"session_start","id":"sess-1","model":"qwen3.6-27b","cwd":"/x","resumed":false}`,
		`{"type":"reasoning_delta","text":"Let me think about this..."}`,
		`{"type":"assistant_delta","text":"I'll look at the file."}`,
		`{"type":"tool_call_start","name":"read","id":"c1","args":{"path":"/x/foo.go"}}`,
		`{"type":"tool_call_end","name":"read","id":"c1","result":"...","error":null}`,
		`{"type":"tool_call_start","name":"edit","id":"c2","args":{}}`,
		`{"type":"tool_call_end","name":"edit","id":"c2","error":null}`,
		`{"type":"assistant_delta","text":"Done."}`,
		`{"type":"assistant_done"}`,
		`{"type":"session_end","tool_errors":false}`,
	}, "\n")

	m, err := ParseStream(strings.NewReader(stream), []string{"read", "edit", "write", "bash", "grep"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m.SessionID != "sess-1" || m.Model != "qwen3.6-27b" {
		t.Errorf("session id/model wrong: %+v", m)
	}
	if m.TurnCount != 1 {
		t.Errorf("turn count: want 1, got %d", m.TurnCount)
	}
	if m.ToolCalls != 2 || m.ToolErrors != 0 {
		t.Errorf("tool counts wrong: calls=%d errors=%d", m.ToolCalls, m.ToolErrors)
	}
	if len(m.HallucinatedTools) != 0 {
		t.Errorf("unexpected hallucinations: %v", m.HallucinatedTools)
	}
	if m.AssistantBytes != len("I'll look at the file.")+len("Done.") {
		t.Errorf("assistant bytes: %d", m.AssistantBytes)
	}
	if m.ReasoningBytes == 0 {
		t.Errorf("reasoning bytes should be nonzero")
	}
	if m.ThinkLeak {
		t.Errorf("no think leak expected")
	}
	if m.HadError {
		t.Errorf("no error expected")
	}
}

func TestParseStream_HallucinatedTool(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"session_start","id":"s","model":"m"}`,
		`{"type":"tool_call_start","name":"str_replace","id":"c1","args":{}}`,
		`{"type":"tool_call_end","name":"str_replace","id":"c1","error":"unknown tool: str_replace"}`,
		`{"type":"tool_call_start","name":"apply_patch","id":"c2","args":{}}`,
		`{"type":"tool_call_end","name":"apply_patch","id":"c2","error":"unknown tool: apply_patch"}`,
		`{"type":"session_end"}`,
	}, "\n")

	m, err := ParseStream(strings.NewReader(stream), []string{"read", "edit", "write", "bash"})
	if err != nil {
		t.Fatal(err)
	}
	if m.ToolErrors != 2 {
		t.Errorf("expected 2 tool errors, got %d", m.ToolErrors)
	}
	if len(m.HallucinatedTools) != 2 {
		t.Errorf("expected 2 hallucinated tools, got %v", m.HallucinatedTools)
	}
}

func TestParseStream_ThinkLeak(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"session_start","id":"s","model":"m"}`,
		`{"type":"assistant_delta","text":"<think>I should call read first</think> ok let me read"}`,
		`{"type":"assistant_done"}`,
		`{"type":"session_end"}`,
	}, "\n")
	m, _ := ParseStream(strings.NewReader(stream), nil)
	if !m.ThinkLeak {
		t.Errorf("expected think leak detection")
	}
}

func TestParseStream_DeniedCountsAsError(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"session_start","id":"s","model":"m"}`,
		`{"type":"tool_call_start","name":"bash","id":"c1","args":{}}`,
		`{"type":"tool_call_end","name":"bash","id":"c1","denied":true}`,
		`{"type":"session_end"}`,
	}, "\n")
	m, _ := ParseStream(strings.NewReader(stream), nil)
	if m.ToolErrors != 1 {
		t.Errorf("denied tool should count as error, got %d", m.ToolErrors)
	}
}

func TestParseStream_SessionEndError(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"session_start","id":"s","model":"m"}`,
		`{"type":"session_end","error":"chat: connection refused"}`,
	}, "\n")
	m, _ := ParseStream(strings.NewReader(stream), nil)
	if !m.HadError || m.FinalError == "" {
		t.Errorf("expected error capture: %+v", m)
	}
}

func TestParseStream_MalformedLineSkipped(t *testing.T) {
	stream := "not-json\n" +
		`{"type":"session_start","id":"s","model":"m"}` + "\n" +
		"also-not-json\n" +
		`{"type":"assistant_done"}` + "\n"
	m, err := ParseStream(strings.NewReader(stream), nil)
	if err != nil {
		t.Fatal(err)
	}
	if m.TurnCount != 1 || m.SessionID != "s" {
		t.Errorf("malformed lines should be skipped, not abort: %+v", m)
	}
}
