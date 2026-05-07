// SPDX-License-Identifier: AGPL-3.0-or-later

// Package score consumes the newline-delimited JSON event stream emitted by
// `enso run --format json` and produces per-run metrics. The schema mirrors
// cmd/enso/run.go's renderJSONTo: each line is `{"type": "<snake_case>", ...}`.
package score

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
)

// Metrics is the per-run summary the eval runner records.
type Metrics struct {
	// SessionID from the first session_start event (empty for ephemeral
	// runs that don't emit one — but enso run always emits session_start).
	SessionID string

	// Model name from the first session_start event.
	Model string

	// TurnCount counts assistant_done events (one per assistant reply).
	TurnCount int

	// ToolCalls counts tool_call_start events.
	ToolCalls int

	// ToolErrors counts tool_call_end events where error != nil OR
	// denied == true.
	ToolErrors int

	// HallucinatedTools is the set of tool_call_start names that were not
	// in the expected tool list passed to ParseStream. Empty when no
	// expected list was supplied.
	HallucinatedTools []string

	// AssistantBytes is the total UTF-8 byte length of all
	// assistant_delta text payloads (i.e. visible output).
	AssistantBytes int

	// ReasoningBytes is the total byte length of reasoning_delta text
	// (the model's internal thinking, not user-visible).
	ReasoningBytes int

	// ThinkLeak is true if any assistant_delta text contained "<think>"
	// or "</think>" — a known failure mode where reasoning content
	// escapes the reasoning channel into visible output.
	ThinkLeak bool

	// HadError is true if any error event arrived OR session_end carried
	// a non-empty "error" field.
	HadError bool

	// FinalError, if non-empty, is the message from the last error or
	// session_end-with-error event.
	FinalError string
}

// ParseStream reads JSONL events from r and returns aggregate metrics.
// expectedTools is the set of tool names that *should* be available; any
// tool_call_start with a name outside this set is recorded in
// HallucinatedTools. Pass nil/empty to skip hallucination tracking.
func ParseStream(r io.Reader, expectedTools []string) (Metrics, error) {
	known := make(map[string]bool, len(expectedTools))
	for _, name := range expectedTools {
		known[name] = true
	}

	var m Metrics
	hallucinated := make(map[string]bool)

	sc := bufio.NewScanner(r)
	// Tool results can be large; bump the line buffer ceiling.
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)

	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var evt map[string]any
		if err := json.Unmarshal(line, &evt); err != nil {
			// Skip malformed lines — don't fail the whole run on one
			// stray non-JSON line (e.g. a stderr leak).
			continue
		}
		t, _ := evt["type"].(string)
		switch t {
		case "session_start":
			if s, ok := evt["id"].(string); ok && s != "" {
				m.SessionID = s
			}
			if s, ok := evt["model"].(string); ok && s != "" {
				m.Model = s
			}
		case "assistant_delta":
			if s, ok := evt["text"].(string); ok {
				m.AssistantBytes += len(s)
				if !m.ThinkLeak && (strings.Contains(s, "<think>") || strings.Contains(s, "</think>")) {
					m.ThinkLeak = true
				}
			}
		case "reasoning_delta":
			if s, ok := evt["text"].(string); ok {
				m.ReasoningBytes += len(s)
			}
		case "assistant_done":
			m.TurnCount++
		case "tool_call_start":
			m.ToolCalls++
			if len(known) > 0 {
				if name, ok := evt["name"].(string); ok && !known[name] {
					hallucinated[name] = true
				}
			}
		case "tool_call_end":
			if e, ok := evt["error"].(string); ok && e != "" {
				m.ToolErrors++
			}
			if d, _ := evt["denied"].(bool); d {
				m.ToolErrors++
			}
		case "error":
			m.HadError = true
			if msg, ok := evt["message"].(string); ok {
				m.FinalError = msg
			}
		case "session_end":
			if e, ok := evt["error"].(string); ok && e != "" {
				m.HadError = true
				m.FinalError = e
			}
		}
	}
	if err := sc.Err(); err != nil {
		return m, err
	}

	if len(hallucinated) > 0 {
		m.HallucinatedTools = make([]string, 0, len(hallucinated))
		for name := range hallucinated {
			m.HallucinatedTools = append(m.HallucinatedTools, name)
		}
	}

	return m, nil
}
