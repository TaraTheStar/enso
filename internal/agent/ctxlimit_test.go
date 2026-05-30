// SPDX-License-Identifier: AGPL-3.0-or-later

package agent

import (
	"testing"

	"github.com/TaraTheStar/enso/internal/llm"
)

// TestEffectiveContextWindow covers the learned-limit override: a limit
// discovered from a server's context-overflow rejection takes precedence
// over the configured context_window (so compaction targets reality even
// when the config is unset or wrong), and it's keyed per provider so a
// /model swap doesn't carry one model's limit onto another.
func TestEffectiveContextWindow(t *testing.T) {
	a := &Agent{}
	pA := &llm.Provider{Name: "qwen", ContextWindow: 0}      // unset config
	pB := &llm.Provider{Name: "other", ContextWindow: 32768} // configured

	// Before learning anything: fall back to the configured value.
	if got := a.effectiveContextWindow(pA); got != 0 {
		t.Errorf("unset provider before learning = %d, want 0", got)
	}
	if got := a.effectiveContextWindow(pB); got != 32768 {
		t.Errorf("configured provider = %d, want 32768", got)
	}

	// Learning a limit for pA drives its effective window without
	// touching pB.
	a.learnContextLimit("qwen", 262144)
	if got := a.effectiveContextWindow(pA); got != 262144 {
		t.Errorf("learned provider = %d, want 262144", got)
	}
	if got := a.effectiveContextWindow(pB); got != 32768 {
		t.Errorf("other provider should be unaffected, got %d", got)
	}

	// A learned limit overrides even a (wrong, too-large) configured value.
	pB.ContextWindow = 1_000_000
	a.learnContextLimit("other", 262144)
	if got := a.effectiveContextWindow(pB); got != 262144 {
		t.Errorf("learned limit should override configured, got %d", got)
	}

	// Non-positive learned values are ignored.
	a.learnContextLimit("zero", 0)
	pZ := &llm.Provider{Name: "zero", ContextWindow: 4096}
	if got := a.effectiveContextWindow(pZ); got != 4096 {
		t.Errorf("ignoring zero learn = %d, want 4096", got)
	}

	if got := a.effectiveContextWindow(nil); got != 0 {
		t.Errorf("nil provider = %d, want 0", got)
	}
}

// TestInputBudget verifies the compaction trigger reserves the output cap
// (max_tokens) plus a margin against the window, so input + output can't
// overflow the real ceiling — the bug that let a 64K max_tokens consume
// the entire fraction-of-window headroom.
func TestInputBudget(t *testing.T) {
	a := &Agent{}
	cases := []struct {
		name      string
		window    int
		maxTokens int
		want      int
	}{
		// 262144 − 65536 − (262144/16=16384) = 180224
		{"full 256K window with 64K output", 262144, 65536, 180224},
		// 131072 − 65536 − (131072/16=8192) = 57344
		{"128K window with 64K output", 131072, 65536, 57344},
		// 65536 − 16384 − (65536/16=4096) = 45056
		{"64K window default output", 65536, 16384, 45056},
		// 131072 − 120000 − 8192 = 2880 < floor(32768) → 32768
		{"pathological huge output clamps to window/4 floor", 131072, 120000, 32768},
		// margin floored at 2048: 16384 − 4096 − 2048 = 10240
		{"small window margin floor", 16384, 4096, 10240},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &llm.Provider{Name: "p", ContextWindow: tc.window, MaxTokens: tc.maxTokens}
			if got := a.inputBudget(p, tc.window); got != tc.want {
				t.Errorf("inputBudget(window=%d, maxTokens=%d) = %d, want %d", tc.window, tc.maxTokens, got, tc.want)
			}
			// The invariant that matters: budget + output ≤ window (so a
			// prompt at budget plus the reply fits), except where the floor
			// deliberately overrides for a pathological max_tokens.
			if got := a.inputBudget(p, tc.window); got != tc.window/4 && got+tc.maxTokens > tc.window {
				t.Errorf("budget %d + maxTokens %d = %d overflows window %d", got, tc.maxTokens, got+tc.maxTokens, tc.window)
			}
		})
	}
}
