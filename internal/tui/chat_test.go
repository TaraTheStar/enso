// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import "testing"

func TestPartialTagSuffix(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		// Real partial tag prefixes
		{"foo<", 1},
		{"foo<t", 2},
		{"foo<th", 3},
		{"foo<think", 6},
		{"foo</thi", 5},
		{"foo</think", 7}, // 7 of 8 — full tag still incomplete by `>`
		// Full tag — no buffering needed
		{"foo<think>", 0},
		{"foo</think>", 0},
		// No partial
		{"foo bar", 0},
		{"hello", 0},
		// Edge: just `<` at end
		{"<", 1},
		// Edge: empty string
		{"", 0},
		// Bytes that look tag-ish but aren't a prefix
		{"foo<x", 0},
	}
	for _, tc := range cases {
		if got := partialTagSuffix(tc.in); got != tc.want {
			t.Errorf("partialTagSuffix(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}
