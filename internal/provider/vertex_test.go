// SPDX-License-Identifier: AGPL-3.0-or-later

package provider

import (
	"testing"

	"github.com/TaraTheStar/azoth/llm/vertex"
	"github.com/TaraTheStar/enso/internal/config"
)

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
	vc, ok := client.(*vertex.VertexClient)
	if !ok {
		t.Fatalf("want *vertex.VertexClient, got %T", client)
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
	vc := client.(*vertex.VertexClient)
	if !vc.ExtendedThinking || vc.ExtendedThinkingBudget != 12000 {
		t.Fatalf("thinking config not threaded: %+v", vc)
	}
}

// TestBuildVertexRequest_MalformedArgsReturnsError keeps the build
// strict about its inputs — if the agent ever produces a tool_calls
// entry with non-JSON arguments, we want to fail loudly at translate
// time, not stream a confusing error from Vertex.
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
	vc := client.(*vertex.VertexClient)
	if len(vc.Safety) != 2 {
		t.Fatalf("Safety not threaded: %+v", vc.Safety)
	}
}

// TestVertexUserParts_ImageInline confirms a user-role Message with
// inline image bytes lands as a Vertex Part with InlineData (not
// FileData, since the bytes are inline). MIME passes through verbatim.
