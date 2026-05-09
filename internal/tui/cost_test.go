// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import "testing"

func TestComputeCost(t *testing.T) {
	cases := []struct {
		name                   string
		inTok, outTok          int64
		inPriceM, outPriceM    float64
		want                   float64
	}{
		{"both zero prices = free", 1_000_000, 1_000_000, 0, 0, 0},
		{"input only", 1_000_000, 0, 2.50, 10.00, 2.50},
		{"output only", 0, 1_000_000, 2.50, 10.00, 10.00},
		{"both, big numbers", 2_000_000, 500_000, 2.50, 10.00, 5.00 + 5.00},
		{"sub-million tokens", 12_000, 5_000, 2.50, 10.00, 0.030 + 0.050},
	}
	for _, tc := range cases {
		got := computeCost(tc.inTok, tc.outTok, tc.inPriceM, tc.outPriceM)
		if got < tc.want-0.0001 || got > tc.want+0.0001 {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestFormatCost(t *testing.T) {
	cases := map[float64]string{
		0:        "$0.0000",
		0.00012:  "$0.0001",
		0.15:     "$0.1500",
		0.9999:   "$0.9999",
		1.00:     "$1.00",
		12.345:   "$12.35",
		-0.05:    "$0.0000", // negative clamps to zero
	}
	for in, want := range cases {
		if got := formatCost(in); got != want {
			t.Errorf("formatCost(%v)=%q, want %q", in, got, want)
		}
	}
}
