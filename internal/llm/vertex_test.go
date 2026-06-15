// SPDX-License-Identifier: AGPL-3.0-or-later

package llm

import (
	"encoding/json"
	"strings"
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
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{"type": "string"},
		},
		"required":             []any{"path"},
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
	got, ok := decl.ParametersJsonSchema.(map[string]any)
	if !ok {
		t.Fatalf("ParametersJsonSchema: want map[string]any, got %T", decl.ParametersJsonSchema)
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

// TestApplyVertexSafety_TranslatesShortNames confirms the user-facing
// short-name keys map onto the SDK's typed HarmCategory / threshold
// enums. The map iterates non-deterministically, so the test checks
// the resulting set rather than its order.
func TestApplyVertexSafety_TranslatesShortNames(t *testing.T) {
	cfg := &genai.GenerateContentConfig{}
	err := applyVertexSafety(cfg, map[string]string{
		"hate_speech":       "BLOCK_NONE",
		"harassment":        "BLOCK_MEDIUM_AND_ABOVE",
		"dangerous_content": "BLOCK_ONLY_HIGH",
		"sexually_explicit": "OFF",
	})
	if err != nil {
		t.Fatalf("applyVertexSafety: %v", err)
	}
	if got := len(cfg.SafetySettings); got != 4 {
		t.Fatalf("SafetySettings len=%d, want 4", got)
	}
	want := map[genai.HarmCategory]genai.HarmBlockThreshold{
		genai.HarmCategoryHateSpeech:       genai.HarmBlockThresholdBlockNone,
		genai.HarmCategoryHarassment:       genai.HarmBlockThresholdBlockMediumAndAbove,
		genai.HarmCategoryDangerousContent: genai.HarmBlockThresholdBlockOnlyHigh,
		genai.HarmCategorySexuallyExplicit: genai.HarmBlockThresholdOff,
	}
	for _, s := range cfg.SafetySettings {
		expected, ok := want[s.Category]
		if !ok {
			t.Errorf("unexpected category %v", s.Category)
			continue
		}
		if s.Threshold != expected {
			t.Errorf("category %v: threshold=%v, want %v", s.Category, s.Threshold, expected)
		}
	}
}

// TestApplyVertexSafety_CaseInsensitive covers the lowercase-threshold
// path. Users coming from cloud-vendor copy-paste tend to mix cases
// (Gemini docs show BLOCK_NONE; vertex.googleapis.com REST shows
// "blockNone"). We standardise on the AWS-style uppercase enum but
// accept any case for ergonomics.
func TestApplyVertexSafety_CaseInsensitive(t *testing.T) {
	cfg := &genai.GenerateContentConfig{}
	err := applyVertexSafety(cfg, map[string]string{
		"Hate_Speech": "block_none",
		" HARASSMENT": "Off",
	})
	if err != nil {
		t.Fatalf("applyVertexSafety: %v", err)
	}
	if len(cfg.SafetySettings) != 2 {
		t.Fatalf("len=%d", len(cfg.SafetySettings))
	}
}

// TestApplyVertexSafety_UnknownCategoryFails pins the fail-loud
// behaviour for typos. The agent loop never sees a silently-permissive
// safety config; the user's first Chat call surfaces the error.
func TestApplyVertexSafety_UnknownCategoryFails(t *testing.T) {
	cfg := &genai.GenerateContentConfig{}
	err := applyVertexSafety(cfg, map[string]string{"hate-speech": "BLOCK_NONE"})
	if err == nil {
		t.Fatal("want error for unknown category (the docs use hate_speech with underscore)")
	}
	if !strings.Contains(err.Error(), "hate-speech") {
		t.Fatalf("error should name the bad category: %v", err)
	}
}

// TestApplyVertexSafety_UnknownThresholdFails pins the threshold-side
// fail-loud. AWS Bedrock has analogous values; users blending the two
// configs would otherwise see a 400 mid-stream.
func TestApplyVertexSafety_UnknownThresholdFails(t *testing.T) {
	cfg := &genai.GenerateContentConfig{}
	err := applyVertexSafety(cfg, map[string]string{"hate_speech": "BLOCK_ALL"})
	if err == nil {
		t.Fatal("want error for unknown threshold")
	}
	if !strings.Contains(err.Error(), "BLOCK_ALL") {
		t.Fatalf("error should name the bad threshold: %v", err)
	}
}

// TestApplyVertexSafety_EmptyMapNoop covers the "unconditionally call
// this" callsite contract: an empty/nil map leaves SafetySettings
// alone so Gemini's defaults stay in effect. Without this, every
// Vertex request would carry a no-op SafetySettings header.
func TestApplyVertexSafety_EmptyMapNoop(t *testing.T) {
	cfg := &genai.GenerateContentConfig{}
	if err := applyVertexSafety(cfg, nil); err != nil {
		t.Fatalf("nil map: %v", err)
	}
	if err := applyVertexSafety(cfg, map[string]string{}); err != nil {
		t.Fatalf("empty map: %v", err)
	}
	if cfg.SafetySettings != nil {
		t.Fatalf("SafetySettings must stay nil for empty input: %+v", cfg.SafetySettings)
	}
}

// TestProviderFactory_VertexSafety confirms the safety map threads
// through the factory onto VertexClient.
func TestProviderFactory_VertexSafety(t *testing.T) {
	client, err := newChatClient(config.ProviderConfig{
		Type:        "vertex",
		Model:       "gemini-2.5-pro",
		GCPProject:  "p",
		GCPLocation: "us-central1",
		VertexSafety: map[string]string{
			"hate_speech":       "BLOCK_NONE",
			"sexually_explicit": "BLOCK_MEDIUM_AND_ABOVE",
		},
	})
	if err != nil {
		t.Fatalf("newChatClient: %v", err)
	}
	vc := client.(*VertexClient)
	if len(vc.Safety) != 2 {
		t.Fatalf("Safety not threaded: %+v", vc.Safety)
	}
}

// TestVertexUserParts_ImageInline confirms a user-role Message with
// inline image bytes lands as a Vertex Part with InlineData (not
// FileData, since the bytes are inline). MIME passes through verbatim.
func TestVertexUserParts_ImageInline(t *testing.T) {
	imgBytes := []byte{0x89, 0x50, 0x4e, 0x47}
	parts, err := vertexUserParts(Message{
		Role:    "user",
		Content: "what is this?",
		Parts:   []MessagePart{NewImagePart("image/png", imgBytes)},
	})
	if err != nil {
		t.Fatalf("vertexUserParts: %v", err)
	}
	if len(parts) != 2 {
		t.Fatalf("parts: want 2 (text + image), got %d", len(parts))
	}
	if parts[1].InlineData == nil {
		t.Fatalf("Parts[1].InlineData nil: %+v", parts[1])
	}
	if parts[1].InlineData.MIMEType != "image/png" {
		t.Fatalf("MIMEType=%q", parts[1].InlineData.MIMEType)
	}
}

// TestVertexUserParts_URIImage covers the URI passthrough — Vertex
// resolves http(s)/gs:// URIs server-side, so URI-only parts work on
// Vertex (unlike Bedrock).
func TestVertexUserParts_URIImage(t *testing.T) {
	parts, err := vertexUserParts(Message{
		Role:  "user",
		Parts: []MessagePart{NewImagePartURI("gs://bucket/cat.jpg")},
	})
	if err != nil {
		t.Fatalf("vertexUserParts: %v", err)
	}
	if parts[0].FileData == nil || parts[0].FileData.FileURI != "gs://bucket/cat.jpg" {
		t.Fatalf("FileData not threaded: %+v", parts[0])
	}
}

// TestVertexFunctionResponseImageParts covers the tool-result image
// path: a read-tool PNG result attaches as a FunctionResponse.Parts
// inline blob alongside the textual response payload.
func TestVertexFunctionResponseImageParts(t *testing.T) {
	out, err := vertexFunctionResponseImageParts(Message{
		Role:       "tool",
		ToolCallID: "call_42",
		Content:    "[image: ok.png]",
		Parts:      []MessagePart{NewImagePart("image/png", []byte("pngdata"))},
	})
	if err != nil {
		t.Fatalf("vertexFunctionResponseImageParts: %v", err)
	}
	if len(out) != 1 || out[0].InlineData == nil {
		t.Fatalf("want 1 inline-data part: %+v", out)
	}
	if string(out[0].InlineData.Data) != "pngdata" || out[0].InlineData.MIMEType != "image/png" {
		t.Fatalf("blob fields: data=%q mime=%q", out[0].InlineData.Data, out[0].InlineData.MIMEType)
	}
}

// TestVertexPart_MissingMIMEFails pins the fail-loud contract: data
// without a mime type is a caller bug, not a silent drop. Gemini
// would reject the request server-side anyway with a less clear error.
func TestVertexPart_MissingMIMEFails(t *testing.T) {
	if _, err := vertexPart(MessagePart{Type: "image", Data: []byte("x")}); err == nil {
		t.Fatal("want error: image data without mime_type")
	}
}

// TestSynthVertexToolCallID is the regression for colliding Gemini
// tool-call IDs: parallel calls to the same tool must get distinct IDs,
// while an explicit id from the API is passed through untouched.
func TestSynthVertexToolCallID(t *testing.T) {
	// Explicit id wins verbatim (idx ignored).
	if got := synthVertexToolCallID("call_real", "bash", 7); got != "call_real" {
		t.Errorf("provided id: got %q, want call_real", got)
	}

	// Two parallel same-tool calls (empty ids, increasing idx) must
	// differ — this is the collision the fix closes.
	a := synthVertexToolCallID("", "bash", 0)
	b := synthVertexToolCallID("", "bash", 1)
	if a == b {
		t.Fatalf("parallel same-tool calls collided: both %q", a)
	}
	if a != "call_bash_0" || b != "call_bash_1" {
		t.Errorf("synthesised ids: got %q, %q; want call_bash_0, call_bash_1", a, b)
	}
}

// TestVertexBlockMessage covers the block-detection that turns a silent
// empty turn into a surfaced error.
func TestVertexBlockMessage(t *testing.T) {
	cases := []struct {
		name      string
		fr        genai.FinishReason
		br        genai.BlockedReason
		wantBlock bool
		contains  string
	}{
		{"normal stop", genai.FinishReasonStop, "", false, ""},
		{"max tokens not a block", genai.FinishReasonMaxTokens, "", false, ""},
		{"safety finish", genai.FinishReasonSafety, "", true, "safety"},
		{"recitation finish", genai.FinishReasonRecitation, "", true, "recitation"},
		{"prohibited finish", genai.FinishReasonProhibitedContent, "", true, "prohibited"},
		{"prompt block wins", genai.FinishReasonStop, genai.BlockedReasonSafety, true, "prompt blocked"},
		{"unspecified block ignored", genai.FinishReasonStop, genai.BlockedReasonUnspecified, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := vertexBlockMessage(tc.fr, tc.br)
			if tc.wantBlock {
				if got == "" {
					t.Fatalf("expected a block message, got none")
				}
				if !strings.Contains(got, tc.contains) {
					t.Errorf("message %q does not contain %q", got, tc.contains)
				}
			} else if got != "" {
				t.Errorf("expected no block, got %q", got)
			}
		})
	}
}

// TestMapVertexFinishReason confirms MAX_TOKENS maps to FinishLength so
// the agent's truncation auto-recovery engages on the Vertex path too.
func TestMapVertexFinishReason(t *testing.T) {
	if got := mapVertexFinishReason(genai.FinishReasonMaxTokens); got != FinishLength {
		t.Errorf("MAX_TOKENS mapped to %q, want %q", got, FinishLength)
	}
	if got := mapVertexFinishReason(genai.FinishReasonStop); got != "" {
		t.Errorf("STOP mapped to %q, want empty", got)
	}
}
