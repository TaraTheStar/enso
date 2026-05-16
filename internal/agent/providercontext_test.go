// SPDX-License-Identifier: AGPL-3.0-or-later

package agent

import (
	"testing"

	"github.com/TaraTheStar/enso/internal/llm"
)

func mkProv(name, model string, include bool) *llm.Provider {
	return &llm.Provider{Name: name, Model: model, IncludeProviders: include}
}

func TestProviderContext_SuppressedCases(t *testing.T) {
	// Fewer than two providers → nil regardless of the flag.
	one := map[string]*llm.Provider{"a": mkProv("a", "ma", true)}
	if pc := providerContext(one, "a"); pc != nil {
		t.Errorf("single provider should yield nil, got %+v", pc)
	}

	// Two providers but the active one opted out → nil.
	off := map[string]*llm.Provider{
		"a": mkProv("a", "ma", false),
		"b": mkProv("b", "mb", false),
	}
	if pc := providerContext(off, "a"); pc != nil {
		t.Errorf("opt-out should yield nil, got %+v", pc)
	}
}

func TestProviderContext_BuildsInfoSet(t *testing.T) {
	provs := map[string]*llm.Provider{
		"a": {Name: "a", Model: "ma", ContextWindow: 8000, Description: "fast", IncludeProviders: true},
		"b": {Name: "b", Model: "mb", IncludeProviders: true},
	}
	pc := providerContext(provs, "a")
	if pc == nil {
		t.Fatal("expected non-nil context")
	}
	if pc.Active != "a" {
		t.Errorf("Active = %q, want a", pc.Active)
	}
	if len(pc.Providers) != 2 {
		t.Fatalf("want 2 providers, got %d", len(pc.Providers))
	}
	var sawA bool
	for _, p := range pc.Providers {
		if p.Name == "a" {
			sawA = true
			if p.Model != "ma" || p.ContextWindow != 8000 || p.Description != "fast" {
				t.Errorf("provider a not copied through: %+v", p)
			}
			if p.Pool != "" {
				t.Errorf("Pool should be empty until step 4, got %q", p.Pool)
			}
		}
	}
	if !sawA {
		t.Error("provider a missing from context")
	}
}
