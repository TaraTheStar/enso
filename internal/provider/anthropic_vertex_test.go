// SPDX-License-Identifier: AGPL-3.0-or-later

package provider

import (
	"testing"

	"github.com/TaraTheStar/azoth/llm/anthropic"
	"github.com/TaraTheStar/azoth/llm/vertex"
	"github.com/TaraTheStar/enso/internal/config"
)

func TestProviderFactory_AnthropicVertexType(t *testing.T) {
	cfg := config.ProviderConfig{
		Type:        "anthropic-vertex",
		Model:       "claude-3-5-sonnet-v2@20241022",
		GCPProject:  "acme-prod",
		GCPLocation: "us-east5",
		MaxTokens:   4096,
	}
	client, err := newChatClient(cfg)
	if err != nil {
		t.Fatalf("newChatClient: %v", err)
	}
	vc, ok := client.(*anthropic.VertexClient)
	if !ok {
		t.Fatalf("want *anthropic.VertexClient, got %T", client)
	}
	if vc.Model != cfg.Model || vc.Project != "acme-prod" || vc.Region != "us-east5" {
		t.Fatalf("config not threaded: %+v", vc)
	}
	if vc.MaxTokens != 4096 {
		t.Fatalf("max_tokens not threaded: %d", vc.MaxTokens)
	}
}

// TestProviderFactory_GenerateContentAndAnthropicVertexCoexist proves
// the two Vertex adapters (Gemini via generateContent and Claude via
// :rawPredict) dispatch to two different structs — type strings keep
// them apart, both can be configured in one TOML.
func TestProviderFactory_GenerateContentAndAnthropicVertexCoexist(t *testing.T) {
	gemini, err := newChatClient(config.ProviderConfig{
		Type: "vertex", Model: "gemini-2.5-pro", GCPProject: "p",
	})
	if err != nil {
		t.Fatalf("vertex: %v", err)
	}
	claude, err := newChatClient(config.ProviderConfig{
		Type: "anthropic-vertex", Model: "claude-3-5-sonnet-v2@20241022",
		GCPProject: "p", GCPLocation: "us-east5",
	})
	if err != nil {
		t.Fatalf("anthropic-vertex: %v", err)
	}
	if _, ok := gemini.(*vertex.Client); !ok {
		t.Fatalf("vertex: want *vertex.Client, got %T", gemini)
	}
	if _, ok := claude.(*anthropic.VertexClient); !ok {
		t.Fatalf("anthropic-vertex: want *anthropic.VertexClient, got %T", claude)
	}
}

func TestProviderFactory_AnthropicVertexPromptCaching(t *testing.T) {
	client, err := newChatClient(config.ProviderConfig{
		Type:          "anthropic-vertex",
		Model:         "claude-3-5-sonnet-v2@20241022",
		GCPProject:    "p",
		GCPLocation:   "us-east5",
		PromptCaching: true,
	})
	if err != nil {
		t.Fatalf("newChatClient: %v", err)
	}
	if !client.(*anthropic.VertexClient).PromptCaching {
		t.Fatal("PromptCaching not threaded")
	}
}
