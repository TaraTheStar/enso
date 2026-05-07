// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"strings"
	"testing"
)

func TestFmtTokens_NoWindow(t *testing.T) {
	got := fmtTokens(1234, 0)
	if got != "1.2k" {
		t.Errorf("got %q, want 1.2k", got)
	}
}

func TestFmtTokens_ColorThresholds(t *testing.T) {
	cases := []struct {
		used, win int
		wantColor string // "" / "[yellow]" / "[red]"
	}{
		{1000, 32000, ""},          // ~3% — default
		{15000, 32000, ""},         // 47% — default (just under 50%)
		{16000, 32000, "[yellow]"}, // 50%
		{20000, 32000, "[yellow]"}, // 62%
		{25600, 32000, "[red]"},    // 80%
		{30000, 32000, "[red]"},    // 94%
	}
	for _, tc := range cases {
		got := fmtTokens(tc.used, tc.win)
		if tc.wantColor == "" {
			if strings.Contains(got, "[yellow]") || strings.Contains(got, "[red]") {
				t.Errorf("used=%d win=%d: unexpected colour in %q", tc.used, tc.win, got)
			}
		} else if !strings.Contains(got, tc.wantColor) {
			t.Errorf("used=%d win=%d: missing %s in %q", tc.used, tc.win, tc.wantColor, got)
		}
	}
}

func TestCompactTokenCount(t *testing.T) {
	cases := []struct {
		n    int
		want string
	}{
		{0, "0"},
		{500, "500"},
		{999, "999"},
		{1000, "1.0k"},
		{3247, "3.2k"},
		{32000, "32.0k"},
		{1_500_000, "1.5M"},
	}
	for _, tc := range cases {
		if got := compactTokenCount(tc.n); got != tc.want {
			t.Errorf("compactTokenCount(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}
