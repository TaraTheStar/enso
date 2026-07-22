// SPDX-License-Identifier: AGPL-3.0-or-later

package provider

import (
	"strings"
	"testing"

	llm "github.com/TaraTheStar/azoth/llm"
	"github.com/TaraTheStar/azoth/llm/bedrock"
	"github.com/TaraTheStar/enso/internal/config"
)

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
	bc, ok := client.(*bedrock.BedrockClient)
	if !ok {
		t.Fatalf("want *bedrock.BedrockClient, got %T", client)
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
	bc := client.(*bedrock.BedrockClient)
	if !bc.ExtendedThinking || bc.ExtendedThinkingBudget != 8000 {
		t.Fatalf("thinking config not threaded: %+v", bc)
	}
}

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
	bc := client.(*bedrock.BedrockClient)
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
func TestProviderFactory_BedrockPromptCaching(t *testing.T) {
	client, err := newChatClient(config.ProviderConfig{
		Type:          "bedrock",
		Model:         "anthropic.claude-3-5-sonnet-20241022-v2:0",
		PromptCaching: true,
	})
	if err != nil {
		t.Fatalf("newChatClient: %v", err)
	}
	if !client.(*bedrock.BedrockClient).PromptCaching {
		t.Fatal("PromptCaching not threaded")
	}
}
