// SPDX-License-Identifier: AGPL-3.0-or-later

package provider

import (
	"testing"

	llm "github.com/TaraTheStar/azoth/llm"
)

// Per-provider usage-translation tests. The wire-level SDK paths are
// implementation details of the upstream SDKs; what we own — and what
// must stay correct under SDK upgrades — is the translation from the
// adapter's reported field set into MessageUsage.

func TestAnthropicUsageFrom_SummingTotal(t *testing.T) {
	// Anthropic InputTokens is fresh-only; cache reads/writes are
	// separate. Total = sum of all four.
	got := anthropicUsageFrom(100, 50, 20, 5)
	want := llm.MessageUsage{
		InputTokens:      100,
		OutputTokens:     50,
		CacheReadTokens:  20,
		CacheWriteTokens: 5,
		TotalTokens:      175,
	}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestAnthropicUsageFrom_ZeroIsEmpty(t *testing.T) {
	got := anthropicUsageFrom(0, 0, 0, 0)
	if !got.Empty() {
		t.Errorf("all-zero translation should be Empty, got %+v", got)
	}
}

func TestBedrockUsageFrom_SummingTotal(t *testing.T) {
	// Bedrock Converse mirrors Anthropic accounting.
	got := bedrockUsageFrom(120, 60, 25, 7)
	want := llm.MessageUsage{
		InputTokens:      120,
		OutputTokens:     60,
		CacheReadTokens:  25,
		CacheWriteTokens: 7,
		TotalTokens:      212,
	}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestBedrockUsageFrom_ZeroIsEmpty(t *testing.T) {
	got := bedrockUsageFrom(0, 0, 0, 0)
	if !got.Empty() {
		t.Errorf("all-zero translation should be Empty, got %+v", got)
	}
}

func TestVertexUsageFrom_TotalIsAuthoritative(t *testing.T) {
	// Gemini reports TotalTokenCount separately; we use it verbatim
	// rather than re-summing. CachedContentTokenCount is a sub-line
	// of PromptTokenCount, not additive.
	got := vertexUsageFrom(150, 75, 30, 225)
	want := llm.MessageUsage{
		InputTokens:     150, // prompt
		OutputTokens:    75,  // candidates
		CacheReadTokens: 30,  // cached (sub-line of prompt)
		TotalTokens:     225, // authoritative from Gemini
	}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestVertexUsageFrom_TotalIsNotRecomputed(t *testing.T) {
	// Verify TotalTokens uses the API-reported value even when it
	// disagrees with our local sum — Gemini's number is authoritative
	// and includes things we don't model (e.g. tool-use prompt tokens).
	got := vertexUsageFrom(100, 50, 0, 999)
	if got.TotalTokens != 999 {
		t.Errorf("TotalTokens = %d, want 999 (API-reported, not recomputed)",
			got.TotalTokens)
	}
}
