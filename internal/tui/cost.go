// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import "fmt"

// computeCost returns total dollars spent given cumulative token
// counts and per-million-token prices. Both prices being zero is the
// "free / local model" case — the caller should gate the cost render
// on `p.InputPrice > 0 || p.OutputPrice > 0` rather than checking the
// computed value, since a real call with zero prices and many tokens
// would still produce 0.0.
func computeCost(inTokens, outTokens int64, inPricePerM, outPricePerM float64) float64 {
	return (float64(inTokens) * inPricePerM / 1_000_000) +
		(float64(outTokens) * outPricePerM / 1_000_000)
}

// formatCost renders a dollar amount with adaptive precision: ≥ $1
// gets two decimals (typical paid sessions), under that gets four so
// fractional-cent spends ($0.0042) are still readable. Always
// $-prefixed; no thousands separators since most enso sessions
// won't run into the multi-dollar range.
func formatCost(dollars float64) string {
	if dollars < 0 {
		dollars = 0
	}
	if dollars >= 1 {
		return fmt.Sprintf("$%.2f", dollars)
	}
	return fmt.Sprintf("$%.4f", dollars)
}
