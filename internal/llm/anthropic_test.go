// SPDX-License-Identifier: AGPL-3.0-or-later

package llm

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/TaraTheStar/enso/internal/config"
	"github.com/anthropics/anthropic-sdk-go"
)

// TestProviderFactory_AnthropicType confirms type = "anthropic"
// dispatches to AnthropicClient with the api.anthropic.com fields
// threaded. Endpoint is repurposed as BaseURL — useful when proxying
// through a corporate gateway.
func TestProviderFactory_AnthropicType(t *testing.T) {
	cfg := config.ProviderConfig{
		Type:                   "anthropic",
		Model:                  "claude-sonnet-4-5",
		APIKey:                 "sk-ant-test",
		Endpoint:               "https://anthropic.example.proxy/",
		MaxTokens:              16000,
		ExtendedThinking:       true,
		ExtendedThinkingBudget: 8000,
	}
	client, err := newChatClient(cfg)
	if err != nil {
		t.Fatalf("newChatClient: %v", err)
	}
	ac, ok := client.(*AnthropicClient)
	if !ok {
		t.Fatalf("want *AnthropicClient, got %T", client)
	}
	if ac.APIKey != cfg.APIKey || ac.Model != cfg.Model {
		t.Fatalf("config not threaded: %+v", ac)
	}
	if ac.BaseURL != cfg.Endpoint {
		t.Fatalf("BaseURL should pull from cfg.Endpoint: %q", ac.BaseURL)
	}
	if !ac.ExtendedThinking || ac.ExtendedThinkingBudget != 8000 {
		t.Fatalf("thinking config not threaded: %+v", ac)
	}
}

// TestToAnthropicParams_System pulls every role="system" message out of
// the conversation and into the top-level System field. Sending system
// turns as message-array entries makes Anthropic 400 the request.
func TestToAnthropicParams_System(t *testing.T) {
	req := ChatRequest{
		Messages: []Message{
			{Role: "system", Content: "you are helpful"},
			{Role: "system", Content: "be concise"},
			{Role: "user", Content: "hi"},
		},
	}
	p, err := toAnthropicParams(req, "claude-3-5-sonnet", 4096)
	if err != nil {
		t.Fatalf("toAnthropicParams: %v", err)
	}
	if len(p.System) != 2 {
		t.Fatalf("system blocks: want 2 got %d", len(p.System))
	}
	if p.System[0].Text != "you are helpful" || p.System[1].Text != "be concise" {
		t.Fatalf("system text mismatch: %+v", p.System)
	}
	if len(p.Messages) != 1 || p.Messages[0].Role != anthropic.MessageParamRoleUser {
		t.Fatalf("expected single user message, got %+v", p.Messages)
	}
}

// TestToAnthropicParams_ToolResultRoundtrip checks that an OpenAI-shape
// tool-result turn (role="tool", ToolCallID, Content) becomes a user
// message carrying a tool_result block, which is how Anthropic models
// tool outputs.
func TestToAnthropicParams_ToolResultRoundtrip(t *testing.T) {
	req := ChatRequest{
		Messages: []Message{
			{Role: "user", Content: "list files"},
			{
				Role: "assistant",
				ToolCalls: []ToolCall{{
					ID:   "call_1",
					Type: "function",
					Function: struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					}{Name: "ls", Arguments: `{"path":"/tmp"}`},
				}},
			},
			{Role: "tool", ToolCallID: "call_1", Content: "a.txt\nb.txt"},
		},
	}
	p, err := toAnthropicParams(req, "claude-3-5-sonnet", 4096)
	if err != nil {
		t.Fatalf("toAnthropicParams: %v", err)
	}
	if len(p.Messages) != 3 {
		t.Fatalf("messages: want 3 got %d", len(p.Messages))
	}
	// Round-trip through JSON so we exercise the same marshal path the
	// SDK uses on the wire — that's where union-discriminator bugs would
	// surface.
	data, err := json.Marshal(p.Messages[1])
	if err != nil {
		t.Fatalf("marshal assistant: %v", err)
	}
	if !strings.Contains(string(data), `"tool_use"`) || !strings.Contains(string(data), `"call_1"`) {
		t.Fatalf("assistant tool_use missing: %s", data)
	}
	data, err = json.Marshal(p.Messages[2])
	if err != nil {
		t.Fatalf("marshal tool result: %v", err)
	}
	if !strings.Contains(string(data), `"tool_result"`) || !strings.Contains(string(data), `"call_1"`) {
		t.Fatalf("tool_result block missing: %s", data)
	}
}

// TestToAnthropicSchema_LiftsRequired exercises the schema translator's
// Required + Properties extraction. The OpenAI tool schema arrives as a
// generic map[string]interface{} so the type-assertion fallbacks matter.
func TestToAnthropicSchema_LiftsRequired(t *testing.T) {
	params := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"path": map[string]interface{}{"type": "string"},
		},
		"required":             []interface{}{"path"},
		"additionalProperties": false,
	}
	schema, err := toAnthropicSchema(params)
	if err != nil {
		t.Fatalf("toAnthropicSchema: %v", err)
	}
	if len(schema.Required) != 1 || schema.Required[0] != "path" {
		t.Fatalf("required: %+v", schema.Required)
	}
	if schema.Properties == nil {
		t.Fatal("properties not set")
	}
	if schema.ExtraFields["additionalProperties"] != false {
		t.Fatalf("extras lost: %+v", schema.ExtraFields)
	}
}

// TestAnthropicBuildParams_ExtendedThinking exercises the layered
// thinking config: when enabled the request must include the thinking
// block, force temperature=1, and drop top_p / top_k. Anthropic rejects
// the request if any of these are off.
func TestAnthropicBuildParams_ExtendedThinking(t *testing.T) {
	c := &AnthropicClient{
		Model:                  "claude-sonnet-4-5",
		ExtendedThinking:       true,
		ExtendedThinkingBudget: 8000,
	}
	params, err := c.buildParams(ChatRequest{
		Messages:    []Message{{Role: "user", Content: "hi"}},
		Temperature: 0.7,
		TopP:        0.95,
	}, 16000)
	if err != nil {
		t.Fatalf("buildParams: %v", err)
	}
	data, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	js := string(data)
	if !strings.Contains(js, `"thinking"`) || !strings.Contains(js, `"enabled"`) {
		t.Fatalf("thinking block missing: %s", js)
	}
	if !strings.Contains(js, `"budget_tokens":8000`) {
		t.Fatalf("budget not threaded through: %s", js)
	}
	if !strings.Contains(js, `"temperature":1`) {
		t.Fatalf("temperature not pinned to 1: %s", js)
	}
	if strings.Contains(js, `"top_p"`) || strings.Contains(js, `"top_k"`) {
		t.Fatalf("top_p / top_k must be cleared with thinking on: %s", js)
	}
}

// TestAnthropicBuildParams_ThinkingBudgetClamps covers the two edges
// that would 400 the request at the API: a budget below 1024 (Anthropic
// minimum) and a budget at or above max_tokens. Both get clamped silently.
func TestAnthropicBuildParams_ThinkingBudgetClamps(t *testing.T) {
	cases := []struct {
		name      string
		budget    int64
		maxTokens int64
		wantBudg  int64
	}{
		{"below min", 500, 16000, 1024},
		{"at max", 16000, 16000, 15999},
		{"above max", 50000, 16000, 15999},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &AnthropicClient{
				Model:                  "claude-sonnet-4-5",
				ExtendedThinking:       true,
				ExtendedThinkingBudget: tc.budget,
			}
			params, err := c.buildParams(ChatRequest{
				Messages: []Message{{Role: "user", Content: "hi"}},
			}, tc.maxTokens)
			if err != nil {
				t.Fatalf("buildParams: %v", err)
			}
			data, _ := json.Marshal(params)
			want := `"budget_tokens":` + strconv.FormatInt(tc.wantBudg, 10)
			if !strings.Contains(string(data), want) {
				t.Fatalf("want %s, got: %s", want, data)
			}
		})
	}
}

// TestAssistantBlocks_EmptyArgs covers the model emitting a tool call
// with no arguments — agent code parses Arguments with json.Unmarshal so
// the translator must fill in "{}", and the assistant block must still
// carry the tool_use block.
func TestAssistantBlocks_EmptyArgs(t *testing.T) {
	m := Message{Role: "assistant", ToolCalls: []ToolCall{{ID: "x", Type: "function"}}}
	m.ToolCalls[0].Function.Name = "now"
	blocks, err := assistantBlocks(m)
	if err != nil {
		t.Fatalf("assistantBlocks: %v", err)
	}
	if len(blocks) != 1 {
		t.Fatalf("blocks: want 1 got %d", len(blocks))
	}
	data, err := json.Marshal(blocks[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(data), `"tool_use"`) {
		t.Fatalf("missing tool_use: %s", data)
	}
}

// TestSplitSystem_UserImagePart verifies a user-role Message with an
// image Part produces an Anthropic ImageBlock alongside any text. The
// base64 wrapper ships verbatim on the wire (Anthropic SDK takes the
// pre-encoded string, not raw bytes).
func TestSplitSystem_UserImagePart(t *testing.T) {
	imgBytes := []byte{0x89, 0x50, 0x4e, 0x47}
	_, msgs, err := splitSystem([]Message{
		{
			Role:    "user",
			Content: "what is this?",
			Parts:   []MessagePart{NewImagePart("image/png", imgBytes)},
		},
	})
	if err != nil {
		t.Fatalf("splitSystem: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("messages: want 1 got %d", len(msgs))
	}
	if len(msgs[0].Content) != 2 {
		t.Fatalf("content blocks: want 2 (text + image), got %d", len(msgs[0].Content))
	}
	// Round-trip through JSON to confirm the on-the-wire shape.
	data, err := json.Marshal(msgs[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	js := string(data)
	if !strings.Contains(js, `"type":"image"`) || !strings.Contains(js, `"media_type":"image/png"`) {
		t.Fatalf("image block missing: %s", js)
	}
	if !strings.Contains(js, `"data":"iVBORw=="`) {
		// "iVBORw==" is the base64 of 0x89 0x50 0x4e 0x47.
		t.Fatalf("base64 data: %s", js)
	}
}

// TestToolResultBlock_WithImage covers the tool-result-with-image
// path: the read tool on a PNG produces a tool_result block whose
// Content carries an image, not just text. Pinned because three
// vendors model this slightly differently and we need the Anthropic
// one to actually round-trip.
func TestToolResultBlock_WithImage(t *testing.T) {
	imgBytes := []byte("gifdata")
	blk, err := toolResultBlock(Message{
		Role:       "tool",
		ToolCallID: "call_42",
		Content:    "[image: foo.gif]",
		Parts:      []MessagePart{NewImagePart("image/gif", imgBytes)},
	})
	if err != nil {
		t.Fatalf("toolResultBlock: %v", err)
	}
	data, err := json.Marshal(blk)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	js := string(data)
	if !strings.Contains(js, `"tool_use_id":"call_42"`) {
		t.Fatalf("tool_use_id missing: %s", js)
	}
	if !strings.Contains(js, `"type":"image"`) {
		t.Fatalf("image content block missing: %s", js)
	}
	if !strings.Contains(js, `"media_type":"image/gif"`) {
		t.Fatalf("media_type missing: %s", js)
	}
	// Synthesized text summary still rides along.
	if !strings.Contains(js, `"text":"[image: foo.gif]"`) {
		t.Fatalf("text summary missing: %s", js)
	}
}

// TestAssistantBlocks_LegacyPathUnchanged guards the back-compat
// contract: an assistant message with no Parts behaves exactly as
// before this refactor.
func TestAssistantBlocks_LegacyPathUnchanged(t *testing.T) {
	blocks, err := assistantBlocks(Message{Role: "assistant", Content: "hello"})
	if err != nil {
		t.Fatalf("assistantBlocks: %v", err)
	}
	if len(blocks) != 1 {
		t.Fatalf("blocks: want 1 got %d", len(blocks))
	}
	data, _ := json.Marshal(blocks[0])
	if !strings.Contains(string(data), `"text":"hello"`) {
		t.Fatalf("text block missing: %s", data)
	}
}

// TestAnthropicContentBlock_ImageWithoutDataErrors pins the fail-loud
// contract: an image part with no Data and no URI is a caller bug, not
// a silent drop.
func TestAnthropicContentBlock_ImageWithoutDataErrors(t *testing.T) {
	_, err := anthropicContentBlock(MessagePart{Type: "image", MIMEType: "image/png"})
	if err == nil {
		t.Fatal("want error for image part with no data/uri")
	}
}

// TestAnthropicBuildParams_PromptCaching confirms cache_control:
// ephemeral markers land on the last system block, the last tool, and
// the trailing conversation message. JSON shape pinning — the SDK's
// omitzero machinery is finicky, and a silently-empty CacheControl
// wouldn't reach the wire.
func TestAnthropicBuildParams_PromptCaching(t *testing.T) {
	c := &AnthropicClient{Model: "claude-sonnet-4-5", PromptCaching: true}
	params, err := c.buildParams(ChatRequest{
		Messages: []Message{
			{Role: "system", Content: "you are concise"},
			{Role: "user", Content: "hi"},
		},
		Tools: []ToolDef{{
			Type: "function",
			Function: ToolFunctionDef{
				Name:        "read",
				Description: "read a file",
				Parameters:  map[string]interface{}{"type": "object"},
			},
		}},
	}, 8192)
	if err != nil {
		t.Fatalf("buildParams: %v", err)
	}
	data, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	js := string(data)
	// System, tools, and the lone user message all carry markers.
	// SDK marshals CacheControlEphemeralParam as {"type":"ephemeral"}.
	if strings.Count(js, `"cache_control"`) != 3 {
		t.Fatalf("want 3 cache_control markers (system + tool + last msg), got: %s", js)
	}
	if strings.Count(js, `"type":"ephemeral"`) != 3 {
		t.Fatalf("want 3 ephemeral types, got: %s", js)
	}
}

// TestAnthropicBuildParams_PromptCachingCapsAtFour confirms the hard
// 4-marker limit is respected: system + tool + last-2 messages = 4,
// and a third trailing message stays unmarked. Anthropic rejects
// requests with more than 4 cache_control markers, so the count must
// never exceed it even when there are plenty of messages to mark.
func TestAnthropicBuildParams_PromptCachingCapsAtFour(t *testing.T) {
	c := &AnthropicClient{Model: "claude-sonnet-4-5", PromptCaching: true}
	params, err := c.buildParams(ChatRequest{
		Messages: []Message{
			{Role: "system", Content: "be brief"},
			{Role: "user", Content: "one"},
			{Role: "assistant", Content: "two"},
			{Role: "user", Content: "three"},
			{Role: "assistant", Content: "four"},
			{Role: "user", Content: "five"},
		},
		Tools: []ToolDef{{
			Type: "function",
			Function: ToolFunctionDef{
				Name: "read", Description: "x", Parameters: map[string]interface{}{"type": "object"},
			},
		}},
	}, 8192)
	if err != nil {
		t.Fatalf("buildParams: %v", err)
	}
	data, _ := json.Marshal(params)
	js := string(data)
	if got := strings.Count(js, `"cache_control"`); got != 4 {
		t.Fatalf("want exactly 4 markers (4-marker hard cap), got %d: %s", got, js)
	}
}

// TestAnthropicBuildParams_PromptCachingDisabled is the back-compat
// pin: with the flag off, NO cache_control markers appear. This is
// the byte-identical legacy shape every existing user gets.
func TestAnthropicBuildParams_PromptCachingDisabled(t *testing.T) {
	c := &AnthropicClient{Model: "claude-sonnet-4-5", PromptCaching: false}
	params, err := c.buildParams(ChatRequest{
		Messages: []Message{
			{Role: "system", Content: "you are concise"},
			{Role: "user", Content: "hi"},
		},
		Tools: []ToolDef{{Type: "function", Function: ToolFunctionDef{Name: "read", Description: "x", Parameters: map[string]interface{}{"type": "object"}}}},
	}, 8192)
	if err != nil {
		t.Fatalf("buildParams: %v", err)
	}
	data, _ := json.Marshal(params)
	if strings.Contains(string(data), `"cache_control"`) {
		t.Fatalf("cache_control must not appear when disabled: %s", data)
	}
}

// TestAnthropicPromptCaching_NoSystemNoTools covers the system-less /
// tool-less path: the trailing message is still a valid cache anchor
// for multi-turn workloads. Important is that empty system + empty
// tools doesn't panic on slice access.
func TestAnthropicPromptCaching_NoSystemNoTools(t *testing.T) {
	c := &AnthropicClient{Model: "claude-sonnet-4-5", PromptCaching: true}
	params, err := c.buildParams(ChatRequest{
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, 8192)
	if err != nil {
		t.Fatalf("buildParams: %v", err)
	}
	data, _ := json.Marshal(params)
	js := string(data)
	if got := strings.Count(js, `"cache_control"`); got != 1 {
		t.Fatalf("want 1 trailing-message marker without system/tools, got %d: %s", got, js)
	}
}

// TestProviderFactory_AnthropicPromptCaching confirms the
// prompt_caching toml key threads onto AnthropicClient.PromptCaching.
func TestProviderFactory_AnthropicPromptCaching(t *testing.T) {
	client, err := newChatClient(config.ProviderConfig{
		Type:          "anthropic",
		Model:         "claude-sonnet-4-5",
		APIKey:        "k",
		PromptCaching: true,
	})
	if err != nil {
		t.Fatalf("newChatClient: %v", err)
	}
	if !client.(*AnthropicClient).PromptCaching {
		t.Fatal("PromptCaching not threaded")
	}
}
