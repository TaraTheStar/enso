// SPDX-License-Identifier: AGPL-3.0-or-later

package bubble

import "testing"

// TestCompactionTokens pins the EventCompacted payload key names. The agent
// publishes "before_tokens"/"after_tokens" (compact.go); a prior version of
// this parser read "before"/"after" and silently rendered every compaction
// as "0 → 0". Cover both the in-process int payload and the daemon JSON
// path where numbers arrive as float64.
func TestCompactionTokens(t *testing.T) {
	t.Run("int payload (local backend)", func(t *testing.T) {
		before, after := compactionTokens(map[string]any{
			"before_tokens": 1200,
			"after_tokens":  450,
		})
		if before != 1200 || after != 450 {
			t.Fatalf("got %d → %d, want 1200 → 450", before, after)
		}
	})

	t.Run("float64 payload (wire/daemon JSON)", func(t *testing.T) {
		before, after := compactionTokens(map[string]any{
			"before_tokens": float64(1200),
			"after_tokens":  float64(450),
		})
		if before != 1200 || after != 450 {
			t.Fatalf("got %d → %d, want 1200 → 450", before, after)
		}
	})

	t.Run("non-map payload", func(t *testing.T) {
		if before, after := compactionTokens("nope"); before != 0 || after != 0 {
			t.Fatalf("got %d → %d, want 0 → 0", before, after)
		}
	})
}
