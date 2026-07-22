// SPDX-License-Identifier: AGPL-3.0-or-later

package provider

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	llm "github.com/TaraTheStar/azoth/llm"

	"github.com/TaraTheStar/enso/internal/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
)

// TestBuildConverseInput_SystemHoisted confirms role="system" messages
// move to the top-level System field rather than appearing in Messages
// — Converse rejects system entries in the Messages array.
func TestBuildConverseInput_SystemHoisted(t *testing.T) {
	in, err := buildConverseInput(llm.ChatRequest{
		Messages: []llm.Message{
			{Role: "system", Content: "be brief"},
			{Role: "user", Content: "hi"},
		},
	}, "anthropic.claude-3-5-sonnet-20241022-v2:0", 1024)
	if err != nil {
		t.Fatalf("buildConverseInput: %v", err)
	}
	if got := len(in.System); got != 1 {
		t.Fatalf("System length: want 1, got %d", got)
	}
	sys, ok := in.System[0].(*types.SystemContentBlockMemberText)
	if !ok {
		t.Fatalf("System[0] type: want *SystemContentBlockMemberText, got %T", in.System[0])
	}
	if sys.Value != "be brief" {
		t.Fatalf("System text: %q", sys.Value)
	}
	if got := len(in.Messages); got != 1 {
		t.Fatalf("Messages length: want 1 (system hoisted), got %d", got)
	}
	if in.Messages[0].Role != types.ConversationRoleUser {
		t.Fatalf("Messages[0].Role: %v", in.Messages[0].Role)
	}
}

// TestBuildConverseInput_ToolCallRoundTrip translates an assistant
// message with tool_calls plus a "tool" role response back through the
// adapter and asserts both surface in the right Bedrock shapes. This
// is the round-trip the agent loop generates after every tool turn.
func TestBuildConverseInput_ToolCallRoundTrip(t *testing.T) {
	in, err := buildConverseInput(llm.ChatRequest{
		Messages: []llm.Message{
			{Role: "user", Content: "what's the weather?"},
			{
				Role: "assistant",
				ToolCalls: []llm.ToolCall{{
					ID:   "call_42",
					Type: "function",
					Function: struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					}{Name: "get_weather", Arguments: `{"city":"sf"}`},
				}},
			},
			{Role: "tool", ToolCallID: "call_42", Content: "65F sunny"},
		},
	}, "anthropic.claude-3-5-sonnet-20241022-v2:0", 1024)
	if err != nil {
		t.Fatalf("buildConverseInput: %v", err)
	}

	if got := len(in.Messages); got != 3 {
		t.Fatalf("Messages length: want 3 (user, assistant w/ toolUse, user w/ toolResult), got %d", got)
	}

	// Assistant message carries the ToolUse block.
	assist := in.Messages[1]
	if assist.Role != types.ConversationRoleAssistant {
		t.Fatalf("Messages[1].Role: %v", assist.Role)
	}
	use, ok := assist.Content[0].(*types.ContentBlockMemberToolUse)
	if !ok {
		t.Fatalf("Messages[1].Content[0] type: %T", assist.Content[0])
	}
	if id := derefString(use.Value.ToolUseId); id != "call_42" {
		t.Fatalf("ToolUseId: %q", id)
	}
	if name := derefString(use.Value.Name); name != "get_weather" {
		t.Fatalf("Name: %q", name)
	}
	// Input is a document.Interface — marshal back to verify the args
	// survived the round-trip.
	gotJSON, err := use.Value.Input.MarshalSmithyDocument()
	if err != nil {
		t.Fatalf("MarshalSmithyDocument: %v", err)
	}
	var gotArgs map[string]string
	if err := json.Unmarshal(gotJSON, &gotArgs); err != nil {
		t.Fatalf("unmarshal args: %v: %s", err, gotJSON)
	}
	if gotArgs["city"] != "sf" {
		t.Fatalf("args[city]: %q", gotArgs["city"])
	}

	// Tool result lands in a user message (Bedrock has no "tool" role).
	toolMsg := in.Messages[2]
	if toolMsg.Role != types.ConversationRoleUser {
		t.Fatalf("Messages[2].Role: want user (tool result), got %v", toolMsg.Role)
	}
	res, ok := toolMsg.Content[0].(*types.ContentBlockMemberToolResult)
	if !ok {
		t.Fatalf("Messages[2].Content[0] type: %T", toolMsg.Content[0])
	}
	if id := derefString(res.Value.ToolUseId); id != "call_42" {
		t.Fatalf("ToolResult.ToolUseId: %q", id)
	}
	txt, ok := res.Value.Content[0].(*types.ToolResultContentBlockMemberText)
	if !ok {
		t.Fatalf("ToolResult.Content[0] type: %T", res.Value.Content[0])
	}
	if txt.Value != "65F sunny" {
		t.Fatalf("ToolResult text: %q", txt.Value)
	}
}

// TestBuildConverseInput_ConsecutiveToolResultsCollapse asserts that a
// parallel-tool-call round (two `tool` messages back-to-back) lands as
// a single user Message with two ToolResult blocks. Bedrock requires
// strict user/assistant alternation, so emitting two adjacent user
// Messages here would 400.
func TestBuildConverseInput_ConsecutiveToolResultsCollapse(t *testing.T) {
	in, err := buildConverseInput(llm.ChatRequest{
		Messages: []llm.Message{
			{Role: "tool", ToolCallID: "a", Content: "result-a"},
			{Role: "tool", ToolCallID: "b", Content: "result-b"},
		},
	}, "anthropic.claude-3-5-sonnet-20241022-v2:0", 1024)
	if err != nil {
		t.Fatalf("buildConverseInput: %v", err)
	}
	if got := len(in.Messages); got != 1 {
		t.Fatalf("Messages length: want 1 (collapsed), got %d", got)
	}
	if got := len(in.Messages[0].Content); got != 2 {
		t.Fatalf("Collapsed message Content length: want 2, got %d", got)
	}
}

// TestBuildConverseInput_ToolSchemaTranslation verifies an OpenAI-shape
// tool definition surfaces as a Converse ToolSpecification with the
// schema wrapped as a JSON document.
func TestBuildConverseInput_ToolSchemaTranslation(t *testing.T) {
	in, err := buildConverseInput(llm.ChatRequest{
		Messages: []llm.Message{{Role: "user", Content: "go"}},
		Tools: []llm.ToolDef{{
			Type: "function",
			Function: llm.ToolFunctionDef{
				Name:        "lookup",
				Description: "find a thing",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"q": map[string]any{"type": "string"},
					},
					"required": []string{"q"},
				},
			},
		}},
	}, "amazon.nova-pro-v1:0", 1024)
	if err != nil {
		t.Fatalf("buildConverseInput: %v", err)
	}
	if in.ToolConfig == nil || len(in.ToolConfig.Tools) != 1 {
		t.Fatalf("ToolConfig.Tools: want 1, got %+v", in.ToolConfig)
	}
	spec, ok := in.ToolConfig.Tools[0].(*types.ToolMemberToolSpec)
	if !ok {
		t.Fatalf("Tools[0] type: %T", in.ToolConfig.Tools[0])
	}
	if derefString(spec.Value.Name) != "lookup" {
		t.Fatalf("Spec.Name: %q", derefString(spec.Value.Name))
	}
	if derefString(spec.Value.Description) != "find a thing" {
		t.Fatalf("Spec.Description: %q", derefString(spec.Value.Description))
	}
	schemaMember, ok := spec.Value.InputSchema.(*types.ToolInputSchemaMemberJson)
	if !ok {
		t.Fatalf("InputSchema type: %T", spec.Value.InputSchema)
	}
	schemaJSON, err := schemaMember.Value.MarshalSmithyDocument()
	if err != nil {
		t.Fatalf("schema marshal: %v", err)
	}
	if !strings.Contains(string(schemaJSON), `"type":"object"`) {
		t.Fatalf("schema missing type=object: %s", schemaJSON)
	}
	if !strings.Contains(string(schemaJSON), `"required":["q"]`) {
		t.Fatalf("schema missing required: %s", schemaJSON)
	}
}

// TestBuildConverseInput_DefaultMaxTokens ensures the adapter applies
// defaultBedrockMaxTokens when the config leaves max_tokens at zero.
// Without this, some Bedrock models return very short responses by
// default — surprising for users coming from OpenAI where the default
// is "unbounded".
func TestBuildConverseInput_DefaultMaxTokens(t *testing.T) {
	in, err := buildConverseInput(llm.ChatRequest{
		Messages: []llm.Message{{Role: "user", Content: "hi"}},
	}, "anthropic.claude-3-5-haiku-20241022-v1:0", 0 /* zero -> default */)
	if err != nil {
		t.Fatalf("buildConverseInput: %v", err)
	}
	if in.InferenceConfig == nil || in.InferenceConfig.MaxTokens == nil {
		t.Fatal("InferenceConfig.MaxTokens not set")
	}
	if got := *in.InferenceConfig.MaxTokens; got != defaultBedrockMaxTokens {
		t.Fatalf("MaxTokens: want %d, got %d", defaultBedrockMaxTokens, got)
	}
}

// TestBuildConverseInput_RequiresModel rejects an empty model ID
// rather than letting an opaque AWS error surface to the user.
func TestBuildConverseInput_RequiresModel(t *testing.T) {
	_, err := buildConverseInput(llm.ChatRequest{
		Messages: []llm.Message{{Role: "user", Content: "hi"}},
	}, "", 1024)
	if err == nil {
		t.Fatal("expected error for empty model")
	}
}

// TestProviderFactory_BedrockType pins the factory's wiring: type =
// "bedrock" must construct a BedrockClient with the AWS-specific
// config fields threaded through. The factory is the only place where
// a typo'd type field becomes a config error rather than silent
// fallback.
func TestProviderFactory_BedrockType(t *testing.T) {
	cfg := config.ProviderConfig{
		Type:       "bedrock",
		Model:      "anthropic.claude-3-5-haiku-20241022-v1:0",
		AWSRegion:  "us-west-2",
		AWSProfile: "dev",
		MaxTokens:  4096,
	}
	client, err := newChatClient(cfg)
	if err != nil {
		t.Fatalf("newChatClient: %v", err)
	}
	bc, ok := client.(*BedrockClient)
	if !ok {
		t.Fatalf("want *BedrockClient, got %T", client)
	}
	if bc.Model != cfg.Model || bc.Region != "us-west-2" || bc.Profile != "dev" {
		t.Fatalf("config not threaded: %+v", bc)
	}
	if bc.MaxTokens != 4096 {
		t.Fatalf("max_tokens not threaded: %d", bc.MaxTokens)
	}
}

// TestProviderFactory_UnknownType ensures a typo'd Type fails loudly
// at startup rather than silently falling back to OpenAI.
func TestProviderFactory_UnknownType(t *testing.T) {
	_, err := newChatClient(config.ProviderConfig{Type: "bedrok" /* typo */})
	if err == nil {
		t.Fatal("expected error for unknown provider type")
	}
	if !strings.Contains(err.Error(), `"bedrok"`) {
		t.Fatalf("error should name the bad type, got: %v", err)
	}
}

// TestApplyExtendedThinking_ForcesAnthropicConstraints confirms that
// enabling extended thinking applies Anthropic's hard constraints:
// temperature=1, top_p cleared, and the thinking config landing in
// AdditionalModelRequestFields with the expected JSON shape. These
// constraints come from the API — violating them gets a 400 back.
func TestApplyExtendedThinking_ForcesAnthropicConstraints(t *testing.T) {
	in, err := buildConverseInput(llm.ChatRequest{
		Messages:    []llm.Message{{Role: "user", Content: "think"}},
		Temperature: 0.7, // should be overwritten
		TopP:        0.9, // should be cleared
	}, "anthropic.claude-3-5-sonnet-20241022-v2:0", 16000)
	if err != nil {
		t.Fatalf("buildConverseInput: %v", err)
	}
	applyExtendedThinking(in, 8000)

	if in.InferenceConfig.Temperature == nil || *in.InferenceConfig.Temperature != 1.0 {
		t.Fatalf("temperature: want 1.0, got %v", in.InferenceConfig.Temperature)
	}
	if in.InferenceConfig.TopP != nil {
		t.Fatalf("top_p: want nil, got %v", *in.InferenceConfig.TopP)
	}
	if in.AdditionalModelRequestFields == nil {
		t.Fatal("AdditionalModelRequestFields not set")
	}
	js, err := in.AdditionalModelRequestFields.MarshalSmithyDocument()
	if err != nil {
		t.Fatalf("marshal additional fields: %v", err)
	}
	want := []string{`"thinking"`, `"type":"enabled"`, `"budget_tokens":8000`}
	for _, w := range want {
		if !strings.Contains(string(js), w) {
			t.Fatalf("additionalModelRequestFields missing %s: %s", w, js)
		}
	}
}

// TestApplyExtendedThinking_BudgetClamps covers the three branches:
// (a) zero → default (4096), (b) below floor → 1024, (c) at-or-above
// max_tokens → max_tokens-1. Silent clamping is deliberate: failing
// loud would just translate to a mid-stream 400 the user can't act on.
func TestApplyExtendedThinking_BudgetClamps(t *testing.T) {
	cases := []struct {
		name      string
		budget    int64
		maxTokens int64
		want      int64 // expected post-clamp budget_tokens
	}{
		{"zero_uses_default", 0, 16000, defaultThinkingBudget},
		{"below_floor_clamps_up", 256, 16000, minThinkingBudget},
		{"above_max_clamps_down", 32000, 16000, 16000 - 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in, err := buildConverseInput(llm.ChatRequest{
				Messages: []llm.Message{{Role: "user", Content: "x"}},
			}, "anthropic.claude-3-5-sonnet-20241022-v2:0", tc.maxTokens)
			if err != nil {
				t.Fatalf("buildConverseInput: %v", err)
			}
			applyExtendedThinking(in, tc.budget)
			js, _ := in.AdditionalModelRequestFields.MarshalSmithyDocument()
			needle := `"budget_tokens":` + itoa(tc.want)
			if !strings.Contains(string(js), needle) {
				t.Fatalf("want %s, got %s", needle, js)
			}
		})
	}
}

// TestProviderFactory_BedrockExtendedThinking confirms the
// extended-thinking config fields thread through the factory onto
// the BedrockClient — without this, the toml field would be silently
// dropped on the floor.
func TestProviderFactory_BedrockExtendedThinking(t *testing.T) {
	client, err := newChatClient(config.ProviderConfig{
		Type:                   "bedrock",
		Model:                  "anthropic.claude-3-5-sonnet-20241022-v2:0",
		ExtendedThinking:       true,
		ExtendedThinkingBudget: 8000,
	})
	if err != nil {
		t.Fatalf("newChatClient: %v", err)
	}
	bc := client.(*BedrockClient)
	if !bc.ExtendedThinking || bc.ExtendedThinkingBudget != 8000 {
		t.Fatalf("thinking config not threaded: %+v", bc)
	}
}

func itoa(n int64) string {
	// tiny helper to keep tests free of strconv import noise
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// TestApplyBedrockGuardrail_WiresStreamConfiguration confirms a
// configured guardrail produces a GuardrailStreamConfiguration on the
// Converse input. We pin the field shape because Bedrock uses two
// different guardrail types (`GuardrailConfiguration` for sync
// Converse, `GuardrailStreamConfiguration` for streaming) — ConverseStream
// requires the latter, and silently picking the wrong one would 400
// the whole turn.
func TestApplyBedrockGuardrail_WiresStreamConfiguration(t *testing.T) {
	in, err := buildConverseInput(llm.ChatRequest{
		Messages: []llm.Message{{Role: "user", Content: "hi"}},
	}, "anthropic.claude-3-5-sonnet-20241022-v2:0", 1024)
	if err != nil {
		t.Fatalf("buildConverseInput: %v", err)
	}
	if err := applyBedrockGuardrail(in, "gr-abc123", "DRAFT", "enabled"); err != nil {
		t.Fatalf("applyBedrockGuardrail: %v", err)
	}
	if in.GuardrailConfig == nil {
		t.Fatal("GuardrailConfig not set")
	}
	if got := derefString(in.GuardrailConfig.GuardrailIdentifier); got != "gr-abc123" {
		t.Errorf("GuardrailIdentifier=%q", got)
	}
	if got := derefString(in.GuardrailConfig.GuardrailVersion); got != "DRAFT" {
		t.Errorf("GuardrailVersion=%q", got)
	}
	if in.GuardrailConfig.Trace != types.GuardrailTraceEnabled {
		t.Errorf("Trace=%v, want enabled", in.GuardrailConfig.Trace)
	}
}

// TestApplyBedrockGuardrail_TraceDefaultsAndAliases covers the empty/
// case-insensitive trace inputs. Empty trace → "enabled" (the cheapest
// useful default — surfaces violations in CloudWatch without the
// full-trace overhead). Case insensitivity matches the rest of the
// config (model ids, region names, etc. are mostly case-insensitive
// across AWS).
func TestApplyBedrockGuardrail_TraceDefaultsAndAliases(t *testing.T) {
	cases := []struct {
		name  string
		trace string
		want  types.GuardrailTrace
	}{
		{"empty defaults to enabled", "", types.GuardrailTraceEnabled},
		{"enabled lowercase", "enabled", types.GuardrailTraceEnabled},
		{"ENABLED uppercase", "ENABLED", types.GuardrailTraceEnabled},
		{"disabled", "Disabled", types.GuardrailTraceDisabled},
		{"enabled_full", "enabled_full", types.GuardrailTraceEnabledFull},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in, err := buildConverseInput(llm.ChatRequest{
				Messages: []llm.Message{{Role: "user", Content: "x"}},
			}, "anthropic.claude-3-5-sonnet-20241022-v2:0", 1024)
			if err != nil {
				t.Fatalf("buildConverseInput: %v", err)
			}
			if err := applyBedrockGuardrail(in, "id", "v1", tc.trace); err != nil {
				t.Fatalf("applyBedrockGuardrail: %v", err)
			}
			if got := in.GuardrailConfig.Trace; got != tc.want {
				t.Fatalf("Trace=%v, want %v", got, tc.want)
			}
		})
	}
}

// TestApplyBedrockGuardrail_FailsLoudOnTypos covers the three error
// paths (missing id, missing version, unknown trace). Without these
// errors, a typo'd config would surface as a confusing 400 mid-stream
// rather than a clear "fix your guardrail config" message at startup.
func TestApplyBedrockGuardrail_FailsLoudOnTypos(t *testing.T) {
	cases := []struct {
		name             string
		id, version, trc string
		wantContains     string
	}{
		{"empty id", "", "v1", "enabled", "id is required"},
		{"empty version", "id", "", "enabled", "version is required"},
		{"unknown trace", "id", "v1", "verbose", "unknown trace"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := &bedrockruntime.ConverseStreamInput{}
			err := applyBedrockGuardrail(in, tc.id, tc.version, tc.trc)
			if err == nil {
				t.Fatal("want error")
			}
			if !strings.Contains(err.Error(), tc.wantContains) {
				t.Fatalf("error %q must mention %q", err, tc.wantContains)
			}
		})
	}
}

// TestBedrockGuardrailHeaders_CollapsesFullToEnabled covers the
// header-path trace mapping. InvokeModel's X-Amzn-Bedrock-Trace header
// only takes ENABLED|DISABLED, so the Converse-only "enabled_full"
// must collapse to ENABLED — documented behaviour, the test pins it.
func TestBedrockGuardrailHeaders_CollapsesFullToEnabled(t *testing.T) {
	hdrs, err := bedrockGuardrailHeaders("gr-x", "DRAFT", "enabled_full")
	if err != nil {
		t.Fatalf("bedrockGuardrailHeaders: %v", err)
	}
	if hdrs["X-Amzn-Bedrock-Trace"] != "ENABLED" {
		t.Fatalf("Trace header=%q, want ENABLED (full collapses)", hdrs["X-Amzn-Bedrock-Trace"])
	}
	if hdrs["X-Amzn-Bedrock-GuardrailIdentifier"] != "gr-x" {
		t.Fatalf("Identifier header missing: %+v", hdrs)
	}
}

// TestBedrockGuardrailHeaders_EmptyIDNoHeaders covers the
// "unconditionally call this" callsite contract: empty id returns nil,
// nil so the caller can skip the iteration cleanly. Without this, the
// anthropic-bedrock client would have to gate the call itself.
func TestBedrockGuardrailHeaders_EmptyIDNoHeaders(t *testing.T) {
	hdrs, err := bedrockGuardrailHeaders("", "DRAFT", "enabled")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if hdrs != nil {
		t.Fatalf("headers must be nil when id is empty, got %+v", hdrs)
	}
}

// TestProviderFactory_BedrockGuardrails confirms the three guardrail
// fields thread through the factory onto BedrockClient.
func TestProviderFactory_BedrockGuardrails(t *testing.T) {
	client, err := newChatClient(config.ProviderConfig{
		Type:                    "bedrock",
		Model:                   "anthropic.claude-3-5-sonnet-20241022-v2:0",
		BedrockGuardrailID:      "gr-abc",
		BedrockGuardrailVersion: "DRAFT",
		BedrockGuardrailTrace:   "enabled_full",
	})
	if err != nil {
		t.Fatalf("newChatClient: %v", err)
	}
	bc := client.(*BedrockClient)
	if bc.GuardrailID != "gr-abc" || bc.GuardrailVersion != "DRAFT" || bc.GuardrailTrace != "enabled_full" {
		t.Fatalf("guardrail fields not threaded: %+v", bc)
	}
}

// TestProviderFactory_OpenAIBackCompat confirms an empty Type still
// constructs an OpenAIClient — existing configs without an explicit
// type field must keep working unchanged.
func TestProviderFactory_OpenAIBackCompat(t *testing.T) {
	client, err := newChatClient(config.ProviderConfig{
		Endpoint: "http://localhost:8080",
		Model:    "llama-3-8b",
	})
	if err != nil {
		t.Fatalf("newChatClient: %v", err)
	}
	if _, ok := client.(*llm.OpenAIClient); !ok {
		t.Fatalf("want *OpenAIClient (empty type), got %T", client)
	}
}

// TestBuildConverseInput_UserImagePart confirms a user-role Message
// with an image Part lands as a ContentBlockMemberImage on the Converse
// request, with raw bytes + the correct ImageFormat — Bedrock requires
// bytes (not a URI) and explicit format enum.
func TestBuildConverseInput_UserImagePart(t *testing.T) {
	imgBytes := []byte{0x89, 0x50, 0x4e, 0x47}
	in, err := buildConverseInput(llm.ChatRequest{
		Messages: []llm.Message{
			{
				Role:    "user",
				Content: "what is in this?",
				Parts:   []llm.MessagePart{llm.NewImagePart("image/png", imgBytes)},
			},
		},
	}, "anthropic.claude-3-5-sonnet-20241022-v2:0", 1024)
	if err != nil {
		t.Fatalf("buildConverseInput: %v", err)
	}
	if len(in.Messages) != 1 || len(in.Messages[0].Content) != 2 {
		t.Fatalf("user content: %+v", in.Messages)
	}
	imgBlk, ok := in.Messages[0].Content[1].(*types.ContentBlockMemberImage)
	if !ok {
		t.Fatalf("Content[1] type: %T", in.Messages[0].Content[1])
	}
	if imgBlk.Value.Format != types.ImageFormatPng {
		t.Fatalf("Format=%v, want png", imgBlk.Value.Format)
	}
	src, ok := imgBlk.Value.Source.(*types.ImageSourceMemberBytes)
	if !ok {
		t.Fatalf("Source type: %T", imgBlk.Value.Source)
	}
	if !bytes.Equal(src.Value, imgBytes) {
		t.Fatalf("image bytes not threaded through")
	}
}

// TestBuildConverseInput_URIImageFails locks the contract that
// Bedrock Converse needs inline bytes — a URI-only image surfaces as
// a clean Go error rather than a confusing 400 mid-stream.
func TestBuildConverseInput_URIImageFails(t *testing.T) {
	_, err := buildConverseInput(llm.ChatRequest{
		Messages: []llm.Message{
			{Role: "user", Parts: []llm.MessagePart{llm.NewImagePartURI("https://example.com/cat.jpg")}},
		},
	}, "anthropic.claude-3-5-sonnet-20241022-v2:0", 1024)
	if err == nil {
		t.Fatal("want error for URI-only image (Bedrock needs bytes)")
	}
}

// TestBedrockImageFormat_UnknownMIMEFails pins the fail-loud
// contract on the mime → format mapper. An unrecognised MIME (HEIC,
// TIFF, AVIF) errors at translate-time so a user trying a fancy
// format sees the limitation up front, not a silent corruption.
func TestBedrockImageFormat_UnknownMIMEFails(t *testing.T) {
	if _, err := bedrockImageFormat("image/heic"); err == nil {
		t.Fatal("want error for image/heic")
	}
	// Aliases / casing tolerated.
	if got, err := bedrockImageFormat("Image/JPG"); err != nil || got != types.ImageFormatJpeg {
		t.Fatalf("image/jpg alias: got %v err=%v", got, err)
	}
}

// TestBedrockToolResultContent_WithImage covers the tool-result
// image path: read tool on a PNG produces a tool_result whose Content
// carries a ToolResultContentBlockMemberImage.
func TestBedrockToolResultContent_WithImage(t *testing.T) {
	in, err := buildConverseInput(llm.ChatRequest{
		Messages: []llm.Message{
			{
				Role:       "tool",
				ToolCallID: "call_42",
				Content:    "[image: ok.png]",
				Parts:      []llm.MessagePart{llm.NewImagePart("image/png", []byte("pngdata"))},
			},
		},
	}, "anthropic.claude-3-5-sonnet-20241022-v2:0", 1024)
	if err != nil {
		t.Fatalf("buildConverseInput: %v", err)
	}
	tr, ok := in.Messages[0].Content[0].(*types.ContentBlockMemberToolResult)
	if !ok {
		t.Fatalf("Content[0] type: %T", in.Messages[0].Content[0])
	}
	if len(tr.Value.Content) != 2 {
		t.Fatalf("tool_result content blocks: want 2 (text + image), got %d", len(tr.Value.Content))
	}
	if _, ok := tr.Value.Content[1].(*types.ToolResultContentBlockMemberImage); !ok {
		t.Fatalf("tool_result content[1] type: %T (want image)", tr.Value.Content[1])
	}
}

// TestApplyBedrockCachePoints_InsertsMarkers confirms cache point
// blocks land after system content, after the last tool, and at the
// tail of the trailing conversation message — the Converse equivalent
// of Anthropic's cache_control:ephemeral markers.
func TestApplyBedrockCachePoints_InsertsMarkers(t *testing.T) {
	in, err := buildConverseInput(llm.ChatRequest{
		Messages: []llm.Message{
			{Role: "system", Content: "be brief"},
			{Role: "user", Content: "hi"},
		},
		Tools: []llm.ToolDef{{
			Type: "function",
			Function: llm.ToolFunctionDef{
				Name: "read", Description: "read", Parameters: map[string]any{"type": "object"},
			},
		}},
	}, "anthropic.claude-3-5-sonnet-20241022-v2:0", 1024)
	if err != nil {
		t.Fatalf("buildConverseInput: %v", err)
	}
	applyBedrockCachePoints(in)

	if len(in.System) != 2 {
		t.Fatalf("System: want 2 (text + cachepoint), got %d", len(in.System))
	}
	if _, ok := in.System[1].(*types.SystemContentBlockMemberCachePoint); !ok {
		t.Fatalf("System[1] type: %T (want SystemContentBlockMemberCachePoint)", in.System[1])
	}

	if in.ToolConfig == nil {
		t.Fatal("ToolConfig nil")
	}
	if n := len(in.ToolConfig.Tools); n != 2 {
		t.Fatalf("Tools: want 2 (spec + cachepoint), got %d", n)
	}
	if _, ok := in.ToolConfig.Tools[1].(*types.ToolMemberCachePoint); !ok {
		t.Fatalf("Tools[1] type: %T (want ToolMemberCachePoint)", in.ToolConfig.Tools[1])
	}

	// Trailing user message should end in a CachePoint.
	if len(in.Messages) != 1 {
		t.Fatalf("Messages: want 1, got %d", len(in.Messages))
	}
	msg := in.Messages[0]
	if n := len(msg.Content); n < 2 {
		t.Fatalf("trailing message Content: want >=2 (text + cachepoint), got %d", n)
	}
	if _, ok := msg.Content[len(msg.Content)-1].(*types.ContentBlockMemberCachePoint); !ok {
		t.Fatalf("last content block type: %T (want ContentBlockMemberCachePoint)",
			msg.Content[len(msg.Content)-1])
	}
}

// TestApplyBedrockCachePoints_CapsAtFour verifies the 4-marker hard
// cap: system + tool + last-2 messages exhausts the budget, and a
// third trailing message stays unmarked. Bedrock Converse rejects
// requests that exceed the cache-point cap.
func TestApplyBedrockCachePoints_CapsAtFour(t *testing.T) {
	in, err := buildConverseInput(llm.ChatRequest{
		Messages: []llm.Message{
			{Role: "system", Content: "be brief"},
			{Role: "user", Content: "one"},
			{Role: "assistant", Content: "two"},
			{Role: "user", Content: "three"},
			{Role: "assistant", Content: "four"},
			{Role: "user", Content: "five"},
		},
		Tools: []llm.ToolDef{{
			Type: "function",
			Function: llm.ToolFunctionDef{
				Name: "read", Description: "x", Parameters: map[string]any{"type": "object"},
			},
		}},
	}, "anthropic.claude-3-5-sonnet-20241022-v2:0", 1024)
	if err != nil {
		t.Fatalf("buildConverseInput: %v", err)
	}
	applyBedrockCachePoints(in)

	cachePoints := 0
	for _, sb := range in.System {
		if _, ok := sb.(*types.SystemContentBlockMemberCachePoint); ok {
			cachePoints++
		}
	}
	if in.ToolConfig != nil {
		for _, tl := range in.ToolConfig.Tools {
			if _, ok := tl.(*types.ToolMemberCachePoint); ok {
				cachePoints++
			}
		}
	}
	for _, m := range in.Messages {
		for _, cb := range m.Content {
			if _, ok := cb.(*types.ContentBlockMemberCachePoint); ok {
				cachePoints++
			}
		}
	}
	if cachePoints != 4 {
		t.Fatalf("cache-point total: want exactly 4 (hard cap), got %d", cachePoints)
	}
}

// TestApplyBedrockCachePoints_NoSystemNoTools covers the system-less /
// tool-less case: the trailing message is still a valid cache anchor
// for multi-turn workloads, but system/tool slices must stay empty
// (Converse rejects empty-System=non-empty payloads).
func TestApplyBedrockCachePoints_NoSystemNoTools(t *testing.T) {
	in, err := buildConverseInput(llm.ChatRequest{
		Messages: []llm.Message{{Role: "user", Content: "hi"}},
	}, "anthropic.claude-3-5-sonnet-20241022-v2:0", 1024)
	if err != nil {
		t.Fatalf("buildConverseInput: %v", err)
	}
	applyBedrockCachePoints(in)
	if len(in.System) != 0 {
		t.Fatalf("System unexpectedly modified: %+v", in.System)
	}
	if in.ToolConfig != nil {
		t.Fatalf("ToolConfig unexpectedly created: %+v", in.ToolConfig)
	}
	// Trailing message should still carry one cache marker.
	if len(in.Messages) != 1 {
		t.Fatalf("Messages: want 1, got %d", len(in.Messages))
	}
	msg := in.Messages[0]
	if _, ok := msg.Content[len(msg.Content)-1].(*types.ContentBlockMemberCachePoint); !ok {
		t.Fatalf("last content block type: %T (want ContentBlockMemberCachePoint)",
			msg.Content[len(msg.Content)-1])
	}
}

// TestProviderFactory_BedrockPromptCaching confirms the prompt_caching
// flag threads through onto BedrockClient.
func TestProviderFactory_BedrockPromptCaching(t *testing.T) {
	client, err := newChatClient(config.ProviderConfig{
		Type:          "bedrock",
		Model:         "anthropic.claude-3-5-sonnet-20241022-v2:0",
		PromptCaching: true,
	})
	if err != nil {
		t.Fatalf("newChatClient: %v", err)
	}
	if !client.(*BedrockClient).PromptCaching {
		t.Fatal("PromptCaching not threaded")
	}
}
