// SPDX-License-Identifier: AGPL-3.0-or-later

package llm

import (
	"context"
	"strings"
	"testing"

	"github.com/TaraTheStar/enso/internal/config"
)

// TestProviderFactory_AnthropicVertexType verifies type = "anthropic-vertex"
// constructs an AnthropicVertexClient with GCP fields threaded.
// Distinct from type = "vertex" (Gemini-only generateContent).
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
	vc, ok := client.(*AnthropicVertexClient)
	if !ok {
		t.Fatalf("want *AnthropicVertexClient, got %T", client)
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
	if _, ok := gemini.(*VertexClient); !ok {
		t.Fatalf("vertex: want *VertexClient, got %T", gemini)
	}
	if _, ok := claude.(*AnthropicVertexClient); !ok {
		t.Fatalf("anthropic-vertex: want *AnthropicVertexClient, got %T", claude)
	}
}

// TestAnthropicVertexClient_MissingRegionErrors exercises the up-front
// validation we add to avoid the SDK's WithGoogleAuth panic when region
// is empty. The error needs to be a clean Go error the caller can see,
// not a runtime panic surfacing from a goroutine.
func TestAnthropicVertexClient_MissingRegionErrors(t *testing.T) {
	c := &AnthropicVertexClient{Model: "claude-3-5-sonnet-v2@20241022", Project: "p"}
	_, err := c.Chat(context.Background(), ChatRequest{
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("want error for missing region")
	}
	if !strings.Contains(err.Error(), "region") {
		t.Fatalf("error should name region: %v", err)
	}
}

// TestAnthropicVertexClient_MissingProjectErrors mirrors the region
// check — project is also required, also pre-validated.
func TestAnthropicVertexClient_MissingProjectErrors(t *testing.T) {
	c := &AnthropicVertexClient{Model: "claude-3-5-sonnet-v2@20241022", Region: "us-east5"}
	_, err := c.Chat(context.Background(), ChatRequest{
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("want error for missing project")
	}
	if !strings.Contains(err.Error(), "project") {
		t.Fatalf("error should name project: %v", err)
	}
}
