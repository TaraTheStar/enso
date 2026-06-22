// SPDX-License-Identifier: AGPL-3.0-or-later

package bubble

import (
	"strings"
	"testing"

	"github.com/TaraTheStar/enso/internal/ui/blocks"
)

func TestDelatex(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"inline span", `If you want "X" $\rightarrow$ Vela`, `If you want "X" → Vela`},
		{"bare command", `scales by \times 2 and \alpha`, `scales by × 2 and α`},
		{"greek mix", `$\Sigma$ over $\omega$`, `Σ over ω`},
		{"display span", `result $$\sum x$$ done`, `result ∑ x done`},
		{"to alias", `a $\to$ b`, `a → b`},
		{"currency untouched", `it costs $5 and $10 total`, `it costs $5 and $10 total`},
		{"lone dollar", `the $ sign`, `the $ sign`},
		{"unknown command kept", `path \section and \n here`, `path \section and \n here`},
		{"no latex", `plain text, nothing here`, `plain text, nothing here`},
		{
			"inline code preserved",
			"use `$\\rightarrow$` literally then $\\rightarrow$ arrow",
			"use `$\\rightarrow$` literally then → arrow",
		},
		{
			"fenced code preserved",
			"text $\\alpha$\n```\n$\\beta$ stays\n```\nmore $\\gamma$",
			"text α\n```\n$\\beta$ stays\n```\nmore γ",
		},
		{
			"multiline list",
			"- A $\\rightarrow$ Vela\n- B $\\rightarrow$ Luno",
			"- A → Vela\n- B → Luno",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := delatex(tt.in); got != tt.want {
				t.Errorf("delatex(%q)\n  got  %q\n  want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestDelatexStream(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"closed span transforms", `a $\rightarrow$ b`, `a → b`},
		{"complete command midline transforms", `scaled by \times 2`, `scaled by × 2`},
		{"trailing partial command held", `arrow \rightarr`, `arrow \rightarr`},
		{"trailing complete-looking command held", `arrow \times`, `arrow \times`},
		{"trailing command resolves once delimited", `arrow \times `, `arrow × `},
		{"unclosed span held raw", `pick $\rightarr`, `pick $\rightarr`},
		{"unclosed span held even if name complete", `pick $\rightarrow`, `pick $\rightarrow`},
		{"earlier line transforms while tail streams", "done $\\alpha$\nnext \\rig", "done α\nnext \\rig"},
		{"currency stays", `costs $5 so far`, `costs $5 so far`},
		{"no latex untouched", `just words`, `just words`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := delatexStream(tt.in); got != tt.want {
				t.Errorf("delatexStream(%q)\n  got  %q\n  want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestDelatexStreamConverges checks the anti-flicker guard never corrupts the
// final result: streaming a buffer delta-by-delta and transforming each prefix
// must end at the same string as transforming the whole text at once, and the
// transformed stable prefix must only ever grow (the live cache invariant).
func TestDelatexStreamConverges(t *testing.T) {
	full := "If you want Sailing $\\rightarrow$ Vela\nIf you want Magic \\to Lyrie done"
	final := delatexStream(full)
	if strings.Contains(final, `\`) || strings.Contains(final, "$") {
		t.Fatalf("final stream output still has latex: %q", final)
	}
	var prevStable string
	for i := 1; i <= len(full); i++ {
		out := delatexStream(full[:i])
		// Stable prefix = everything up to the last newline; it must be a
		// monotonic, append-only extension across deltas.
		stable := out
		if nl := strings.LastIndexByte(out, '\n'); nl >= 0 {
			stable = out[:nl+1]
		} else {
			stable = ""
		}
		if !strings.HasPrefix(stable, prevStable) {
			t.Fatalf("stable prefix not monotonic at byte %d:\n  prev %q\n  now  %q", i, prevStable, stable)
		}
		prevStable = stable
	}
	if got := delatexStream(full); got != final {
		t.Fatalf("non-deterministic: %q vs %q", got, final)
	}
}

// TestLiveRenderLatexParity streams LaTeX-bearing text one byte at a time and
// asserts the cached live render stays byte-identical to the uncached
// renderBlock at every step — the same contract as TestLiveRenderMatchesRenderBlock,
// but exercising delatexStream's stable-line transitions and tail guard.
func TestLiveRenderLatexParity(t *testing.T) {
	const full = "If Sailing $\\rightarrow$ Vela\nIf Magic \\to Lyrie\nscale \\times 2 and \\alpha done"
	for _, w := range []int{0, 24, 40, 80} {
		var cache liveRenderCache
		b := &blocks.Assistant{}
		for i := 1; i <= len(full); i++ {
			b.Text = full[:i]
			got := cache.render(b, w)
			want := renderBlock(b, w, false)
			if got != want {
				t.Fatalf("width=%d step=%d: cached diverged\n got: %q\nwant: %q", w, i, got, want)
			}
		}
	}
}
