// SPDX-License-Identifier: AGPL-3.0-or-later

package llm

import (
	"encoding/json"
	"testing"

	"github.com/TaraTheStar/enso/internal/config"
	"google.golang.org/genai"
)

// TestBuildVertexRequest_SystemHoisted confirms role="system" messages
// move to the top-level SystemInstruction rather than appearing in the
// contents list. Vertex's contents array only accepts user/model
// roles — a system entry would be rejected by the API.
func TestBuildVertexRequest_SystemHoisted(t *testing.T) {
	contents, cfg, err := buildVertexRequest(ChatRequest{
		Messages: []Message{
			{Role: "system", Content: "be brief"},
			{Role: "user", Content: "hi"},
		},
	}, 0)
	if err != nil {
		t.Fatalf("buildVertexRequest: %v", err)
	}
	if cfg.SystemInstruction == nil {
		t.Fatalf("SystemInstruction not set")
	}
	if got := len(cfg.SystemInstruction.Parts); got != 1 {
		t.Fatalf("SystemInstruction parts: want 1, got %d", got)
	}
	if cfg.SystemInstruction.Parts[0].Text != "be brief" {
		t.Fatalf("SystemInstruction text: %q", cfg.SystemInstruction.Parts[0].Text)
	}
	if got := len(contents); got != 1 {
		t.Fatalf("contents length: want 1 (system hoisted), got %d", got)
	}
	if contents[0].Role != genai.RoleUser {
		t.Fatalf("contents[0].Role: %q", contents[0].Role)
	}
}

// TestBuildVertexRequest_SystemMessagesConcatenate verifies multiple
// system messages collapse into one SystemInstruction with separate
// text parts — agent harness flows tend to layer instructions
// (role-prompt + tool-prompt + workspace-prompt), so dropping any of
// them would silently degrade the agent.
func TestBuildVertexRequest_SystemMessagesConcatenate(t *testing.T) {
	_, cfg, err := buildVertexRequest(ChatRequest{
		Messages: []Message{
			{Role: "system", Content: "you are helpful"},
			{Role: "system", Content: "be concise"},
			{Role: "user", Content: "hi"},
		},
	}, 0)
	if err != nil {
		t.Fatalf("buildVertexRequest: %v", err)
	}
	if cfg.SystemInstruction == nil || len(cfg.SystemInstruction.Parts) != 2 {
		t.Fatalf("want 2 system parts, got %#v", cfg.SystemInstruction)
	}
}

// TestBuildVertexRequest_ToolCallRoundTrip translates an assistant
// message with tool_calls plus a "tool" role response through the
// adapter and asserts both surface in Vertex's FunctionCall +
// FunctionResponse parts. Gemini matches responses to calls by *name*,
// not id — so the test inspects the name on the FunctionResponse.
func TestBuildVertexRequest_ToolCallRoundTrip(t *testing.T) {
	contents, _, err := buildVertexRequest(ChatRequest{
		Messages: []Message{
			{Role: "user", Content: "what's the weather?"},
			{
				Role: "assistant",
				ToolCalls: []ToolCall{{
					ID:   "call_42",
					Type: "function",
					Function: struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					}{Name: "get_weather", Arguments: `{"city":"SF"}`},
				}},
			},
			{Role: "tool", ToolCallID: "call_42", Content: `{"temp":62,"unit":"F"}`},
		},
	}, 0)
	if err != nil {
		t.Fatalf("buildVertexRequest: %v", err)
	}
	if got := len(contents); got != 3 {
		t.Fatalf("contents length: want 3 (user, model, user-tool-response), got %d", got)
	}

	// Assistant message: role=model, parts include one FunctionCall.
	if contents[1].Role != genai.RoleModel {
		t.Fatalf("contents[1].Role: %q", contents[1].Role)
	}
	var fc *genai.FunctionCall
	for _, p := range contents[1].Parts {
		if p.FunctionCall != nil {
			fc = p.FunctionCall
			break
		}
	}
	if fc == nil {
		t.Fatalf("contents[1] missing FunctionCall part: %#v", contents[1].Parts)
	}
	if fc.Name != "get_weather" {
		t.Fatalf("FunctionCall.Name: %q", fc.Name)
	}
	if got, want := fc.Args["city"], "SF"; got != want {
		t.Fatalf("FunctionCall.Args[city]: got %v, want %v", got, want)
	}

	// Tool response: role=user, parts include FunctionResponse with
	// matching name. Parsed JSON propagates structured (not stringified).
	if contents[2].Role != genai.RoleUser {
		t.Fatalf("contents[2].Role: %q", contents[2].Role)
	}
	var fr *genai.FunctionResponse
	for _, p := range contents[2].Parts {
		if p.FunctionResponse != nil {
			fr = p.FunctionResponse
			break
		}
	}
	if fr == nil {
		t.Fatalf("contents[2] missing FunctionResponse part: %#v", contents[2].Parts)
	}
	if fr.Name != "get_weather" {
		t.Fatalf("FunctionResponse.Name: %q (Gemini matches by name)", fr.Name)
	}
	if got, ok := fr.Response["temp"].(float64); !ok || got != 62 {
		t.Fatalf("FunctionResponse.Response[temp]: got %v (%T)", fr.Response["temp"], fr.Response["temp"])
	}
}

// TestBuildVertexRequest_NonJSONToolOutputWrapped verifies a tool that
// returned plain text (the common case for bash/edit/read tools) gets
// wrapped into {"content": "..."} rather than failing JSON parse —
// Gemini's FunctionResponse needs a structured payload.
func TestBuildVertexRequest_NonJSONToolOutputWrapped(t *testing.T) {
	contents, _, err := buildVertexRequest(ChatRequest{
		Messages: []Message{
			{
				Role: "assistant",
				ToolCalls: []ToolCall{{
					ID: "call_1", Type: "function",
					Function: struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					}{Name: "bash", Arguments: `{}`},
				}},
			},
			{Role: "tool", ToolCallID: "call_1", Content: "hello world\n"},
		},
	}, 0)
	if err != nil {
		t.Fatalf("buildVertexRequest: %v", err)
	}
	fr := contents[1].Parts[0].FunctionResponse
	if fr == nil {
		t.Fatalf("missing FunctionResponse")
	}
	if got := fr.Response["content"]; got != "hello world\n" {
		t.Fatalf("Response[content]: %v", got)
	}
}

// TestBuildVertexRequest_ConsecutiveToolResponsesCollapse verifies
// that parallel tool-calls in a single assistant turn produce one user
// Content holding all FunctionResponse parts — mirrors how Bedrock
// batches ToolResult blocks and matches what Gemini expects (one
// per-turn boundary, not one per tool).
func TestBuildVertexRequest_ConsecutiveToolResponsesCollapse(t *testing.T) {
	contents, _, err := buildVertexRequest(ChatRequest{
		Messages: []Message{
			{
				Role: "assistant",
				ToolCalls: []ToolCall{
					{ID: "a", Type: "function", Function: struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					}{Name: "f1", Arguments: `{}`}},
					{ID: "b", Type: "function", Function: struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					}{Name: "f2", Arguments: `{}`}},
				},
			},
			{Role: "tool", ToolCallID: "a", Content: `{"x":1}`},
			{Role: "tool", ToolCallID: "b", Content: `{"y":2}`},
		},
	}, 0)
	if err != nil {
		t.Fatalf("buildVertexRequest: %v", err)
	}
	// Expect 2 contents: assistant (with 2 FunctionCall parts), user
	// (with 2 FunctionResponse parts).
	if got := len(contents); got != 2 {
		t.Fatalf("contents length: want 2, got %d", got)
	}
	if got := len(contents[1].Parts); got != 2 {
		t.Fatalf("user tool-response parts: want 2, got %d", got)
	}
	for _, part := range contents[1].Parts {
		if part.FunctionResponse == nil {
			t.Fatalf("expected all parts to be FunctionResponse: %#v", part)
		}
	}
}

// TestBuildVertexRequest_ToolSchemaTranslation confirms the OpenAI
// ToolDef JSON schema passes through verbatim into Vertex's
// ParametersJsonSchema field. We deliberately do NOT translate into
// the SDK's *Schema type — it strips features like oneOf and
// additionalProperties that the agent's tool definitions rely on.
func TestBuildVertexRequest_ToolSchemaTranslation(t *testing.T) {
	schema := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"path": map[string]interface{}{"type": "string"},
		},
		"required":             []interface{}{"path"},
		"additionalProperties": false,
	}
	_, cfg, err := buildVertexRequest(ChatRequest{
		Messages: []Message{{Role: "user", Content: "x"}},
		Tools: []ToolDef{{
			Type: "function",
			Function: ToolFunctionDef{
				Name:        "read_file",
				Description: "Read a file from disk",
				Parameters:  schema,
			},
		}},
	}, 0)
	if err != nil {
		t.Fatalf("buildVertexRequest: %v", err)
	}
	if len(cfg.Tools) != 1 || len(cfg.Tools[0].FunctionDeclarations) != 1 {
		t.Fatalf("Tools shape: %+v", cfg.Tools)
	}
	decl := cfg.Tools[0].FunctionDeclarations[0]
	if decl.Name != "read_file" {
		t.Errorf("Name: %q", decl.Name)
	}
	if decl.Description != "Read a file from disk" {
		t.Errorf("Description: %q", decl.Description)
	}
	// ParametersJsonSchema must be the raw map (preserves
	// additionalProperties, which the SDK's *Schema doesn't model).
	got, ok := decl.ParametersJsonSchema.(map[string]interface{})
	if !ok {
		t.Fatalf("ParametersJsonSchema: want map[string]interface{}, got %T", decl.ParametersJsonSchema)
	}
	if got["additionalProperties"] != false {
		t.Errorf("additionalProperties not preserved: %+v", got)
	}
}

// TestBuildVertexRequest_DefaultMaxTokens covers the zero→default
// fallback. Gemini accepts an unset MaxOutputTokens, but mirroring
// Bedrock with an explicit ceiling keeps adapter behaviour comparable.
func TestBuildVertexRequest_DefaultMaxTokens(t *testing.T) {
	_, cfg, err := buildVertexRequest(ChatRequest{
		Messages: []Message{{Role: "user", Content: "x"}},
	}, 0)
	if err != nil {
		t.Fatalf("buildVertexRequest: %v", err)
	}
	if cfg.MaxOutputTokens != int32(defaultVertexMaxTokens) {
		t.Fatalf("MaxOutputTokens: want %d (default), got %d", defaultVertexMaxTokens, cfg.MaxOutputTokens)
	}
}

// TestApplyVertexThinking_TogglesIncludeThoughts confirms enabling
// extended thinking flips IncludeThoughts on. Without that flag,
// Gemini won't return Thought parts even on 2.5 models.
func TestApplyVertexThinking_TogglesIncludeThoughts(t *testing.T) {
	_, cfg, err := buildVertexRequest(ChatRequest{
		Messages: []Message{{Role: "user", Content: "x"}},
	}, 16000)
	if err != nil {
		t.Fatalf("buildVertexRequest: %v", err)
	}
	applyVertexThinking(cfg, 8000)
	if cfg.ThinkingConfig == nil {
		t.Fatalf("ThinkingConfig nil")
	}
	if !cfg.ThinkingConfig.IncludeThoughts {
		t.Fatalf("IncludeThoughts not enabled")
	}
	if cfg.ThinkingConfig.ThinkingBudget == nil || *cfg.ThinkingConfig.ThinkingBudget != 8000 {
		t.Fatalf("ThinkingBudget: %v", cfg.ThinkingConfig.ThinkingBudget)
	}
}

// TestApplyVertexThinking_ZeroBudgetLeavesDynamic verifies that when
// the user enables thinking but leaves the budget at zero, we set
// IncludeThoughts but leave ThinkingBudget nil — Gemini's dynamic mode
// stays in effect. This differs from Anthropic's behaviour, where
// budget=0 falls back to a hardcoded default.
func TestApplyVertexThinking_ZeroBudgetLeavesDynamic(t *testing.T) {
	cfg := &genai.GenerateContentConfig{}
	applyVertexThinking(cfg, 0)
	if !cfg.ThinkingConfig.IncludeThoughts {
		t.Fatalf("IncludeThoughts not enabled")
	}
	if cfg.ThinkingConfig.ThinkingBudget != nil {
		t.Fatalf("ThinkingBudget: want nil (dynamic), got %v", *cfg.ThinkingConfig.ThinkingBudget)
	}
}

// TestApplyVertexThinking_NoAnthropicConstraints verifies that Vertex
// thinking does NOT clamp temperature or clear top_p — those are
// Anthropic-specific rules. Sampling settings the caller provided must
// survive applyVertexThinking unchanged.
func TestApplyVertexThinking_NoAnthropicConstraints(t *testing.T) {
	_, cfg, err := buildVertexRequest(ChatRequest{
		Messages:    []Message{{Role: "user", Content: "x"}},
		Temperature: 0.7,
		TopP:        0.9,
	}, 16000)
	if err != nil {
		t.Fatalf("buildVertexRequest: %v", err)
	}
	applyVertexThinking(cfg, 0)
	if cfg.Temperature == nil || *cfg.Temperature != 0.7 {
		t.Fatalf("Temperature stomped: %v", cfg.Temperature)
	}
	if cfg.TopP == nil || *cfg.TopP != 0.9 {
		t.Fatalf("TopP cleared: %v", cfg.TopP)
	}
}

// TestProviderFactory_VertexType confirms the factory dispatches
// type="vertex" to a VertexClient with the GCP fields threaded.
func TestProviderFactory_VertexType(t *testing.T) {
	client, err := newChatClient(config.ProviderConfig{
		Type:        "vertex",
		Model:       "gemini-2.5-pro",
		GCPProject:  "acme-prod",
		GCPLocation: "europe-west4",
		MaxTokens:   4096,
	})
	if err != nil {
		t.Fatalf("newChatClient: %v", err)
	}
	vc, ok := client.(*VertexClient)
	if !ok {
		t.Fatalf("want *VertexClient, got %T", client)
	}
	if vc.Project != "acme-prod" || vc.Location != "europe-west4" {
		t.Fatalf("GCP fields not threaded: %+v", vc)
	}
	if vc.MaxTokens != 4096 {
		t.Fatalf("MaxTokens: %d", vc.MaxTokens)
	}
}

// TestProviderFactory_VertexExtendedThinking confirms thinking fields
// thread through the factory — without this the toml field would be
// silently dropped on the floor.
func TestProviderFactory_VertexExtendedThinking(t *testing.T) {
	client, err := newChatClient(config.ProviderConfig{
		Type:                   "vertex",
		Model:                  "gemini-2.5-pro",
		GCPProject:             "acme",
		ExtendedThinking:       true,
		ExtendedThinkingBudget: 12000,
	})
	if err != nil {
		t.Fatalf("newChatClient: %v", err)
	}
	vc := client.(*VertexClient)
	if !vc.ExtendedThinking || vc.ExtendedThinkingBudget != 12000 {
		t.Fatalf("thinking config not threaded: %+v", vc)
	}
}

// TestBuildVertexRequest_MalformedArgsReturnsError keeps the build
// strict about its inputs — if the agent ever produces a tool_calls
// entry with non-JSON arguments, we want to fail loudly at translate
// time, not stream a confusing error from Vertex.
func TestBuildVertexRequest_MalformedArgsReturnsError(t *testing.T) {
	_, _, err := buildVertexRequest(ChatRequest{
		Messages: []Message{{
			Role: "assistant",
			ToolCalls: []ToolCall{{
				ID: "call_1", Type: "function",
				Function: struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}{Name: "f", Arguments: `not json`},
			}},
		}},
	}, 0)
	if err == nil {
		t.Fatal("want error for malformed tool-call arguments, got nil")
	}
}

// Tiny sanity check on json round-trip to keep the test honest about
// what genai.FunctionCall.Args looks like — Args is map[string]any,
// which json.Unmarshal produces directly.
func TestVertex_ArgsMapRoundTrip(t *testing.T) {
	src := `{"a":1,"b":"two","c":[3,4]}`
	var got map[string]any
	if err := json.Unmarshal([]byte(src), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["a"].(float64) != 1 {
		t.Fatalf("a: %v", got["a"])
	}
}
