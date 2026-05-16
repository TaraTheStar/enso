// SPDX-License-Identifier: AGPL-3.0-or-later

package instructions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderProviderSection_NilOrSingleIsEmpty(t *testing.T) {
	if got := renderProviderSection(nil); got != "" {
		t.Errorf("nil context should render empty, got %q", got)
	}
	one := &ProviderContext{Active: "a", Providers: []ProviderInfo{{Name: "a"}}}
	if got := renderProviderSection(one); got != "" {
		t.Errorf("single provider should render empty, got %q", got)
	}
}

func TestRenderProviderSection_ActiveAndOthers(t *testing.T) {
	pc := &ProviderContext{
		Active: "qwen-fast",
		Providers: []ProviderInfo{
			{Name: "qwen-fast", Model: "qwen3.6-27B", ContextWindow: 65536},
			{Name: "deep", Model: "minimax-m2.7", ContextWindow: 131072, Description: "hard SWE"},
			{Name: "local", Model: "local"}, // model == name → model omitted
		},
	}
	got := renderProviderSection(pc)

	if !strings.Contains(got, "## Available models") {
		t.Fatalf("missing header:\n%s", got)
	}
	if !strings.Contains(got, "You are running as `qwen-fast` (model: qwen3.6-27B, context: 65k).") {
		t.Errorf("active line wrong:\n%s", got)
	}
	// Active provider must not also appear in the "others" bullet list.
	if strings.Count(got, "`qwen-fast`") != 1 {
		t.Errorf("active provider should appear exactly once:\n%s", got)
	}
	if !strings.Contains(got, "- `deep` (model: minimax-m2.7, context: 131k) — hard SWE") {
		t.Errorf("other-provider line wrong:\n%s", got)
	}
	// model == name and no context → bare name, no empty parens.
	if !strings.Contains(got, "- `local`\n") && !strings.HasSuffix(got, "- `local`") {
		t.Errorf("bare provider should have no attrs parens:\n%s", got)
	}
	if strings.Contains(got, "()") {
		t.Errorf("empty attrs parens leaked:\n%s", got)
	}
	if strings.Contains(got, "pool:") {
		t.Errorf("no Pool set → pool: must be omitted:\n%s", got)
	}
	if strings.Contains(got, "sharing a pool") {
		t.Errorf("no shared pool → guidance paragraph must be absent:\n%s", got)
	}
}

func TestRenderProviderSection_SharedPoolAndSwapCost(t *testing.T) {
	pc := &ProviderContext{
		Active: "fast",
		Providers: []ProviderInfo{
			{Name: "fast", Model: "mf", Pool: "latchkey-3090", SwapCost: "high"},
			{Name: "deep", Model: "md", Pool: "latchkey-3090", SwapCost: "high"},
		},
	}
	got := renderProviderSection(pc)

	if !strings.Contains(got, "You are running as `fast` (model: mf, pool: latchkey-3090, swap-cost: high).") {
		t.Errorf("active line missing pool/swap-cost:\n%s", got)
	}
	if !strings.Contains(got, "- `deep` (model: md, pool: latchkey-3090, swap-cost: high)") {
		t.Errorf("other line missing pool/swap-cost:\n%s", got)
	}
	if !strings.Contains(got, "sharing a pool run on the same hardware") {
		t.Errorf("shared-pool guidance paragraph missing:\n%s", got)
	}
}

func TestRenderProviderSection_DistinctPoolsNoGuidance(t *testing.T) {
	pc := &ProviderContext{
		Active: "a",
		Providers: []ProviderInfo{
			{Name: "a", Model: "ma", Pool: "auto-h1-1"},
			{Name: "b", Model: "mb", Pool: "auto-h2-2"},
		},
	}
	got := renderProviderSection(pc)

	if !strings.Contains(got, "pool: auto-h1-1") || !strings.Contains(got, "pool: auto-h2-2") {
		t.Errorf("distinct pools should still be labelled:\n%s", got)
	}
	if strings.Contains(got, "sharing a pool") {
		t.Errorf("all-distinct pools → no guidance paragraph:\n%s", got)
	}
	if strings.Contains(got, "swap-cost:") {
		t.Errorf("no swap_cost set → must be omitted:\n%s", got)
	}
}

// The provider section sits between the embedded default and the user
// ENSO.md layer, and a user replace:true discards BOTH default and the
// provider section.
func TestBuildSystemPromptLayered_ProviderSlotAndReplace(t *testing.T) {
	pc := &ProviderContext{
		Active: "a",
		Providers: []ProviderInfo{
			{Name: "a", Model: "ma"}, {Name: "b", Model: "mb"},
		},
	}

	tmp := t.TempDir()
	setUserHome(t, tmp+"/no-home")
	cwd := filepath.Join(tmp, "proj")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}

	layers, err := BuildSystemPromptLayered(cwd, pc)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(layers) < 2 || layers[0].Name != "default" || layers[1].Name != "providers" {
		t.Fatalf("expected default then providers, got %+v", names(layers))
	}

	// User ENSO.md with replace:true must discard default AND providers.
	userENSO := filepath.Join(tmp, ".config", "enso", "ENSO.md")
	if err := os.MkdirAll(filepath.Dir(userENSO), 0o755); err != nil {
		t.Fatal(err)
	}
	setUserHome(t, tmp)
	if err := os.WriteFile(userENSO, []byte("---\nreplace: true\n---\nONLY THIS\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	prompt, err := BuildSystemPrompt(cwd, pc)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if strings.Contains(prompt, "## Available models") {
		t.Errorf("replace:true should discard the provider section:\n%s", prompt)
	}
	if !strings.Contains(prompt, "ONLY THIS") {
		t.Errorf("replacement body missing:\n%s", prompt)
	}
}
