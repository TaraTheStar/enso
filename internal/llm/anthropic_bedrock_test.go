// SPDX-License-Identifier: AGPL-3.0-or-later

package llm

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/TaraTheStar/enso/internal/config"
)

// TestAnthropicBedrock_BuildParamsReusesAnthropicTranslator confirms
// AnthropicBedrockClient goes through the same buildAnthropicParams
// helper as AnthropicClient: system messages get hoisted, tool blocks
// translate, thinking layers on correctly. If this diverges from the
// direct Anthropic adapter, the shared helper is the wrong shape.
func TestAnthropicBedrock_BuildParamsReusesAnthropicTranslator(t *testing.T) {
	params, err := buildAnthropicParams(
		ChatRequest{
			Messages: []Message{
				{Role: "system", Content: "be brief"},
				{Role: "user", Content: "hi"},
			},
		},
		"anthropic.claude-3-5-sonnet-20241022-v2:0",
		16000,
		true,  // extended thinking
		8000,  // budget
		false, // prompt caching
	)
	if err != nil {
		t.Fatalf("buildAnthropicParams: %v", err)
	}
	data, _ := json.Marshal(params)
	js := string(data)
	if !strings.Contains(js, `"system":[{"text":"be brief"`) {
		t.Fatalf("system not hoisted: %s", js)
	}
	if !strings.Contains(js, `"thinking"`) || !strings.Contains(js, `"budget_tokens":8000`) {
		t.Fatalf("thinking not applied: %s", js)
	}
	if !strings.Contains(js, `"model":"anthropic.claude-3-5-sonnet-20241022-v2:0"`) {
		t.Fatalf("bedrock model id not preserved: %s", js)
	}
}

// TestProviderFactory_AnthropicBedrockType checks that
// type = "anthropic-bedrock" constructs an AnthropicBedrockClient with
// AWS-specific config (region, profile) threaded. Distinct from
// type = "bedrock" (Converse) — both can coexist in one config.
func TestProviderFactory_AnthropicBedrockType(t *testing.T) {
	cfg := config.ProviderConfig{
		Type:       "anthropic-bedrock",
		Model:      "anthropic.claude-3-5-haiku-20241022-v1:0",
		AWSRegion:  "us-west-2",
		AWSProfile: "dev",
		MaxTokens:  4096,
	}
	client, err := newChatClient(cfg)
	if err != nil {
		t.Fatalf("newChatClient: %v", err)
	}
	bc, ok := client.(*AnthropicBedrockClient)
	if !ok {
		t.Fatalf("want *AnthropicBedrockClient, got %T", client)
	}
	if bc.Model != cfg.Model || bc.Region != "us-west-2" || bc.Profile != "dev" {
		t.Fatalf("config not threaded: %+v", bc)
	}
	if bc.MaxTokens != 4096 {
		t.Fatalf("max_tokens not threaded: %d", bc.MaxTokens)
	}
}

// TestProviderFactory_AnthropicBedrockGuardrails confirms the three
// guardrail fields thread through onto the anthropic-bedrock adapter.
// Same `bedrock_guardrail_*` TOML keys as the Converse path so the
// user-facing surface stays consistent across both Bedrock variants.
func TestProviderFactory_AnthropicBedrockGuardrails(t *testing.T) {
	client, err := newChatClient(config.ProviderConfig{
		Type:                    "anthropic-bedrock",
		Model:                   "anthropic.claude-3-5-sonnet-20241022-v2:0",
		AWSRegion:               "us-east-1",
		BedrockGuardrailID:      "gr-xyz",
		BedrockGuardrailVersion: "1",
		BedrockGuardrailTrace:   "enabled",
	})
	if err != nil {
		t.Fatalf("newChatClient: %v", err)
	}
	abc := client.(*AnthropicBedrockClient)
	if abc.GuardrailID != "gr-xyz" || abc.GuardrailVersion != "1" || abc.GuardrailTrace != "enabled" {
		t.Fatalf("guardrail fields not threaded: %+v", abc)
	}
}

// TestProviderFactory_ConverseAndAnthropicBedrockCoexist proves that
// type = "bedrock" and type = "anthropic-bedrock" dispatch to two
// different adapters — the renames around the parked work made this
// possible. Users with both blocks configured must see two distinct
// providers, not one stomping the other.
func TestProviderFactory_ConverseAndAnthropicBedrockCoexist(t *testing.T) {
	converse, err := newChatClient(config.ProviderConfig{
		Type: "bedrock", Model: "anthropic.claude-3-5-sonnet-20241022-v2:0",
	})
	if err != nil {
		t.Fatalf("converse: %v", err)
	}
	native, err := newChatClient(config.ProviderConfig{
		Type: "anthropic-bedrock", Model: "anthropic.claude-3-5-sonnet-20241022-v2:0",
	})
	if err != nil {
		t.Fatalf("anthropic-bedrock: %v", err)
	}
	if _, ok := converse.(*BedrockClient); !ok {
		t.Fatalf("converse: want *BedrockClient, got %T", converse)
	}
	if _, ok := native.(*AnthropicBedrockClient); !ok {
		t.Fatalf("anthropic-bedrock: want *AnthropicBedrockClient, got %T", native)
	}
}

// TestProviderFactory_AnthropicBedrockPromptCaching pins the
// factory wiring on the anthropic-bedrock adapter — same TOML key.
func TestProviderFactory_AnthropicBedrockPromptCaching(t *testing.T) {
	client, err := newChatClient(config.ProviderConfig{
		Type:          "anthropic-bedrock",
		Model:         "anthropic.claude-3-5-sonnet-20241022-v2:0",
		AWSRegion:     "us-east-1",
		PromptCaching: true,
	})
	if err != nil {
		t.Fatalf("newChatClient: %v", err)
	}
	if !client.(*AnthropicBedrockClient).PromptCaching {
		t.Fatal("PromptCaching not threaded")
	}
}
