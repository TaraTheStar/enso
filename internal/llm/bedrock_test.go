// SPDX-License-Identifier: AGPL-3.0-or-later

package llm

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/TaraTheStar/enso/internal/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
)

// TestBuildConverseInput_SystemHoisted confirms role="system" messages
// move to the top-level System field rather than appearing in Messages
// — Converse rejects system entries in the Messages array.
func TestBuildConverseInput_SystemHoisted(t *testing.T) {
	in, err := buildConverseInput(ChatRequest{
		Messages: []Message{
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
	in, err := buildConverseInput(ChatRequest{
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
	in, err := buildConverseInput(ChatRequest{
		Messages: []Message{
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
	in, err := buildConverseInput(ChatRequest{
		Messages: []Message{{Role: "user", Content: "go"}},
		Tools: []ToolDef{{
			Type: "function",
			Function: ToolFunctionDef{
				Name:        "lookup",
				Description: "find a thing",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"q": map[string]interface{}{"type": "string"},
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
	in, err := buildConverseInput(ChatRequest{
		Messages: []Message{{Role: "user", Content: "hi"}},
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
	_, err := buildConverseInput(ChatRequest{
		Messages: []Message{{Role: "user", Content: "hi"}},
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
	in, err := buildConverseInput(ChatRequest{
		Messages:    []Message{{Role: "user", Content: "think"}},
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
			in, err := buildConverseInput(ChatRequest{
				Messages: []Message{{Role: "user", Content: "x"}},
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
	if _, ok := client.(*OpenAIClient); !ok {
		t.Fatalf("want *OpenAIClient (empty type), got %T", client)
	}
}
