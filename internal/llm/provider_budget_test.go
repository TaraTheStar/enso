// SPDX-License-Identifier: AGPL-3.0-or-later

package llm

import "testing"

func TestComputeInputBudget(t *testing.T) {
	cases := []struct {
		name                             string
		window, maxTokens, compactBudget int
		want                             int
	}{
		// Decoupled budget is returned directly — context_window stays the
		// real window (honest denominator) while compaction fires here.
		{"decoupled", 262144, 24576, 67584, 67584},
		// A budget >= window is clamped to 3/4 window so a fat-fingered
		// value can't silently disable compaction.
		{"over-window clamps", 100000, 0, 100000, 75000},
		// Legacy: window - maxTokens - margin (margin = window/16, min 2048).
		{"legacy formula", 98304, 24576, 0, 67584},
		// Legacy floor: an over-large maxTokens can't drop the budget below
		// window/4.
		{"legacy floor", 40000, 38000, 0, 10000},
		{"unconfigured window", 0, 0, 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ComputeInputBudget(c.window, c.maxTokens, c.compactBudget); got != c.want {
				t.Errorf("ComputeInputBudget(%d,%d,%d) = %d, want %d",
					c.window, c.maxTokens, c.compactBudget, got, c.want)
			}
		})
	}
}
