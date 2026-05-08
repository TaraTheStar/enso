// SPDX-License-Identifier: AGPL-3.0-or-later

package mcp

import (
	"context"
	"fmt"
	"strings"

	mcpclient "github.com/mark3labs/mcp-go/client"
	mcpproto "github.com/mark3labs/mcp-go/mcp"

	"github.com/TaraTheStar/enso/internal/tools"
)

// Naming rules. OpenAI's function-call schema requires
// `^[a-zA-Z0-9_-]{1,64}$`. We split that 64-char budget across the
// `mcp__<server>__<tool>` prefix structure so any valid pair produces a
// valid full name. Anything outside this charset would either get
// rejected upstream (HTTP 400 on the next request — confusing failure
// mode) or, worse, smuggle control / display chars into TUI logs.
const (
	maxServerNameLen = 24
	maxToolNameLen   = 32
	mcpToolPrefix    = "mcp__"
)

var nameValidChars = func() [256]bool {
	var ok [256]bool
	for c := 'a'; c <= 'z'; c++ {
		ok[c] = true
	}
	for c := 'A'; c <= 'Z'; c++ {
		ok[c] = true
	}
	for c := '0'; c <= '9'; c++ {
		ok[c] = true
	}
	ok['_'] = true
	ok['-'] = true
	return ok
}()

// validateName reports whether s is a valid server or tool name and, if
// not, an error describing why. The same charset / length rules apply
// to both — they're the components of an OpenAI-spec-compliant tool
// name.
func validateName(s string, maxLen int) error {
	if s == "" {
		return fmt.Errorf("empty")
	}
	if len(s) > maxLen {
		return fmt.Errorf("length %d exceeds limit %d", len(s), maxLen)
	}
	for i := 0; i < len(s); i++ {
		if !nameValidChars[s[i]] {
			return fmt.Errorf("invalid char %q at offset %d (allowed: a-z A-Z 0-9 _ -)", s[i], i)
		}
	}
	return nil
}

// adaptTool wraps one MCP tool as a tools.Tool that proxies calls back
// over the MCP client. Naming convention: `mcp__<server>__<tool>` (per
// PLAN §6). Returns nil when the tool name from the server fails
// validation — the caller should skip nil entries and ideally log so
// the user knows their MCP server returned a junk name.
//
// onTransportError fires when CallTool itself returns a Go error
// (transport-level failure — broken pipe, EOF, closed connection).
// Tool-level errors come back as res.IsError with a successful Go
// return and don't trigger this path. May be nil if the caller
// doesn't care to track per-server health.
func adaptTool(serverName string, cli *mcpclient.Client, mt mcpproto.Tool, onTransportError func(error)) tools.Tool {
	if err := validateName(mt.Name, maxToolNameLen); err != nil {
		return nil
	}
	return &mcpTool{
		fullName:         mcpToolPrefix + serverName + "__" + mt.Name,
		remoteName:       mt.Name,
		description:      mt.Description,
		parameters:       schemaToMap(mt.InputSchema),
		client:           cli,
		onTransportError: onTransportError,
	}
}

type mcpTool struct {
	fullName         string
	remoteName       string
	description      string
	parameters       map[string]interface{}
	client           *mcpclient.Client
	onTransportError func(error)
}

func (t *mcpTool) Name() string                       { return t.fullName }
func (t *mcpTool) Description() string                { return t.description }
func (t *mcpTool) Parameters() map[string]interface{} { return t.parameters }

func (t *mcpTool) Run(ctx context.Context, args map[string]interface{}, _ *tools.AgentContext) (tools.Result, error) {
	var req mcpproto.CallToolRequest
	req.Params.Name = t.remoteName
	req.Params.Arguments = args

	res, err := t.client.CallTool(ctx, req)
	if err != nil {
		// User-cancellation isn't a transport failure — preserve the
		// canonical sentinels so the agent loop's cancel handling
		// still matches, and don't smear the sidebar with a "server
		// failed" badge for a deliberate Ctrl-C.
		if ctx.Err() == nil && t.onTransportError != nil {
			t.onTransportError(err)
		}
		return tools.Result{}, fmt.Errorf("mcp %s: %w", t.fullName, err)
	}

	text := joinTextContent(res.Content)
	if res.IsError {
		return tools.Result{
			LLMOutput:  "error: " + text,
			FullOutput: text,
		}, nil
	}
	return tools.Result{LLMOutput: text, FullOutput: text}, nil
}

// joinTextContent concatenates the text-content fragments. Non-text content
// (image, audio, embedded resource) is silently dropped — text is what the
// model can reason about.
func joinTextContent(items []mcpproto.Content) string {
	var b strings.Builder
	for _, c := range items {
		if tc, ok := mcpproto.AsTextContent(c); ok {
			b.WriteString(tc.Text)
			b.WriteByte('\n')
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// schemaToMap converts the MCP-protocol input schema into the JSON-Schema
// object shape the LLM client expects on `tools[i].function.parameters`.
func schemaToMap(s mcpproto.ToolInputSchema) map[string]interface{} {
	out := map[string]interface{}{
		"type": "object",
	}
	if s.Type != "" {
		out["type"] = s.Type
	}
	if s.Properties != nil {
		out["properties"] = s.Properties
	} else {
		// OpenAI-compatible servers expect a properties object even when empty.
		out["properties"] = map[string]interface{}{}
	}
	if len(s.Required) > 0 {
		out["required"] = s.Required
	}
	if s.AdditionalProperties != nil {
		out["additionalProperties"] = s.AdditionalProperties
	}
	return out
}
