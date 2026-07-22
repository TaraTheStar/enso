// SPDX-License-Identifier: AGPL-3.0-or-later

package provider

import (
	"testing"

	"github.com/TaraTheStar/azoth/llm/anthropic"
	"github.com/TaraTheStar/azoth/llm/bedrock"
	"github.com/TaraTheStar/enso/internal/config"
)

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
	bc, ok := client.(*anthropic.BedrockClient)
	if !ok {
		t.Fatalf("want *anthropic.BedrockClient, got %T", client)
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
	abc := client.(*anthropic.BedrockClient)
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
	if _, ok := converse.(*bedrock.Client); !ok {
		t.Fatalf("converse: want *bedrock.Client, got %T", converse)
	}
	if _, ok := native.(*anthropic.BedrockClient); !ok {
		t.Fatalf("anthropic-bedrock: want *anthropic.BedrockClient, got %T", native)
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
	if !client.(*anthropic.BedrockClient).PromptCaching {
		t.Fatal("PromptCaching not threaded")
	}
}
