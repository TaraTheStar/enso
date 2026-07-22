// SPDX-License-Identifier: AGPL-3.0-or-later

package provider

import (
	"testing"

	"github.com/TaraTheStar/azoth/llm/anthropic"
	"github.com/TaraTheStar/enso/internal/config"
)

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
	ac, ok := client.(*anthropic.Client)
	if !ok {
		t.Fatalf("want *anthropic.Client, got %T", client)
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
	if !client.(*anthropic.Client).PromptCaching {
		t.Fatal("PromptCaching not threaded")
	}
}
