// SPDX-License-Identifier: AGPL-3.0-or-later

package bubble

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// stripANSI drops styling so we can assert on the glyphs/percentage text
// without depending on the active colour profile.
func stripANSI(s string) string { return ansi.Strip(s) }

// TestCompactingIndicatorContent: the bar always shows the full cell
// width plus the "compacting… N%" label, and the pct is clamped to a
// sane 0–100.
func TestCompactingIndicatorContent(t *testing.T) {
	for _, pct := range []int{-5, 0, 1, 42, 99, 100, 250} {
		got := stripANSI(compactingIndicator(pct))

		// Label present and clamped.
		want := pct
		switch {
		case want < 0:
			want = 0
		case want > 100:
			want = 100
		}
		if !strings.Contains(got, "compacting…") {
			t.Fatalf("pct=%d: missing label in %q", pct, got)
		}
		if !strings.Contains(got, itoa(want)+"%") {
			t.Fatalf("pct=%d: want %d%% in %q", pct, want, got)
		}

		// Exactly compactBarCells glyph cells precede the space+label.
		bar := got[:strings.Index(got, " compacting…")]
		if n := len([]rune(bar)); n != compactBarCells {
			t.Fatalf("pct=%d: bar has %d cells, want %d (%q)", pct, n, compactBarCells, bar)
		}
	}
}

// TestCompactingIndicatorFillMonotonic: more progress never renders
// fewer filled (non-track) cells. Track cells are the dim floor glyph
// compactFillGlyphs[0]; filled cells use the brighter ⣤/⣶/⣿.
func TestCompactingIndicatorFillMonotonic(t *testing.T) {
	prev := -1
	for pct := 0; pct <= 100; pct += 5 {
		bar := stripANSI(compactingIndicator(pct))
		bar = bar[:strings.Index(bar, " compacting…")]
		filled := 0
		for _, r := range bar {
			if r == '⣤' || r == '⣶' || r == '⣿' {
				filled++
			}
		}
		if filled < prev {
			t.Fatalf("pct=%d filled=%d dropped below prev=%d", pct, filled, prev)
		}
		prev = filled
	}
	if prev == 0 {
		t.Fatalf("bar never filled at 100%%")
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
