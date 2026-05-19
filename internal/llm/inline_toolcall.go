// SPDX-License-Identifier: AGPL-3.0-or-later

package llm

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Some GGUF chat templates (Qwen3 / Hermes-style on llama.cpp) emit
// tool calls as inline text in the assistant `content` instead of via
// the OpenAI `tool_calls` channel. Two shapes are seen in the wild,
// both usually wrapped in <tool_call>…</tool_call>:
//
//	pseudo-XML:  <function=read>
//	               <parameter=path>/x</parameter>
//	               <parameter=first_line>63</parameter>
//	             </function>
//
//	JSON:        {"name":"read","arguments":{"path":"/x","first_line":63}}
//
// ParseInlineToolCalls recovers these so a model whose template leaks
// tool calls into text still works, instead of surfacing "model
// produced no visible response". It returns the content with the parsed
// blocks removed and the recovered calls; if nothing parses it returns
// the content unchanged and a nil slice (caller falls back to its
// normal empty-response handling).
var (
	reToolCallBlock = regexp.MustCompile(`(?is)<tool_call>\s*(.*?)\s*</tool_call>`)
	reFunctionBlock = regexp.MustCompile(`(?is)<function\s*=\s*([^>\s]+)\s*>(.*?)</function\s*>`)
	reParameter     = regexp.MustCompile(`(?is)<parameter\s*=\s*([^>\s]+)\s*>(.*?)</parameter\s*>`)
)

func ParseInlineToolCalls(content string) (string, []ToolCall) {
	if content == "" || !strings.Contains(content, "<") {
		return content, nil
	}

	var calls []ToolCall
	cleaned := content

	// Preferred path: explicit <tool_call> wrappers. Strip each whole
	// block from the visible content (so any surrounding prose is kept
	// but the markup never lands in history or the UI replay).
	for _, m := range reToolCallBlock.FindAllStringSubmatch(content, -1) {
		if tc, ok := parseOneInline(strings.TrimSpace(m[1]), len(calls)); ok {
			calls = append(calls, tc)
		}
	}
	if len(calls) > 0 {
		cleaned = reToolCallBlock.ReplaceAllString(content, "")
		return strings.TrimSpace(cleaned), calls
	}

	// Fallback: some llama.cpp builds emit a bare <function=…> block
	// with no <tool_call> wrapper at all.
	for _, m := range reFunctionBlock.FindAllStringSubmatch(content, -1) {
		if tc, ok := parseFunctionXML(m[1], m[2], len(calls)); ok {
			calls = append(calls, tc)
		}
	}
	if len(calls) > 0 {
		cleaned = reFunctionBlock.ReplaceAllString(content, "")
		return strings.TrimSpace(cleaned), calls
	}

	return content, nil
}

// parseOneInline parses the inside of one <tool_call> block, trying the
// pseudo-XML <function=…> shape first, then a raw JSON object.
func parseOneInline(body string, idx int) (ToolCall, bool) {
	if fm := reFunctionBlock.FindStringSubmatch(body); fm != nil {
		return parseFunctionXML(fm[1], fm[2], idx)
	}
	// JSON shape: {"name": "...", "arguments": {...}} (arguments may be
	// an object or an already-encoded string).
	var j struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal([]byte(body), &j); err != nil || j.Name == "" {
		return ToolCall{}, false
	}
	args := strings.TrimSpace(string(j.Arguments))
	if args == "" || args == "null" {
		args = "{}"
	} else if s := ""; json.Unmarshal(j.Arguments, &s) == nil {
		// arguments arrived as a JSON-encoded string of JSON.
		args = s
	}
	return newInlineCall(j.Name, args, idx), true
}

// parseFunctionXML turns `<function=NAME>` + repeated
// `<parameter=KEY>VALUE</parameter>` into a tool call. Each VALUE is
// coerced: a value that is itself valid JSON (number, bool, object,
// array) is kept as-is so a schema expecting `first_line: 63` gets the
// int 63, not the string "63"; anything else becomes a JSON string.
func parseFunctionXML(name, body string, idx int) (ToolCall, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return ToolCall{}, false
	}
	parts := make([]string, 0)
	for _, pm := range reParameter.FindAllStringSubmatch(body, -1) {
		key := strings.TrimSpace(pm[1])
		if key == "" {
			continue
		}
		raw := strings.TrimSpace(pm[2])
		var val string
		if raw != "" && json.Valid([]byte(raw)) && raw != "}" {
			val = raw // already a JSON scalar/object/array
		} else {
			b, _ := json.Marshal(raw)
			val = string(b)
		}
		kb, _ := json.Marshal(key)
		parts = append(parts, fmt.Sprintf("%s:%s", kb, val))
	}
	return newInlineCall(name, "{"+strings.Join(parts, ",")+"}", idx), true
}

func newInlineCall(name, args string, idx int) ToolCall {
	var tc ToolCall
	tc.ID = fmt.Sprintf("call_inline_%d", idx+1)
	tc.Type = "function"
	tc.Function.Name = strings.TrimSpace(name)
	tc.Function.Arguments = args
	return tc
}
