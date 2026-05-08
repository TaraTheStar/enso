// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"fmt"
	"io"
	"time"
)

// WriteStatsText renders Stats as the plain-text summary used by both
// `enso stats` (CLI) and `/stats` (TUI slash command). `since` is the
// inclusive lower bound the stats were computed against; pass the
// zero time to indicate "all sessions".
//
// Lifted from cmd/enso/stats.go with no behaviour change.
func WriteStatsText(w io.Writer, st Stats, since time.Time) error {
	if st.SessionCount == 0 {
		if since.IsZero() {
			_, err := fmt.Fprintln(w, "no sessions yet")
			return err
		}
		_, err := fmt.Fprintf(w, "no sessions since %s\n", since.Format("2006-01-02"))
		return err
	}

	bw := &mdWriter{w: w}

	bw.printf("Sessions:    %d", st.SessionCount)
	if !st.OldestUpdatedAt.IsZero() {
		bw.printf("  (%s — %s)",
			st.OldestUpdatedAt.Format("2006-01-02"),
			st.NewestUpdatedAt.Format("2006-01-02"))
	}
	bw.printf("\n")
	if st.InterruptedCount > 0 {
		bw.printf("Interrupted: %d\n", st.InterruptedCount)
	}
	bw.printf("Approx tokens (4-char heuristic): ~%s\n", formatThousands(st.ApproxTotalTokens))

	bw.printf("\nMessages by role:\n")
	for _, role := range []string{"user", "assistant", "tool", "system"} {
		if n := st.MessagesByRole[role]; n > 0 {
			bw.printf("  %-10s %d\n", role, n)
		}
	}

	if len(st.SessionsByModel) > 0 {
		bw.printf("\nModels:\n")
		for _, name := range st.SortedModels() {
			bw.printf("  %-30s %d\n", name, st.SessionsByModel[name])
		}
	}

	if len(st.ToolCallsByName) > 0 {
		bw.printf("\nTool calls:\n")
		for _, name := range st.SortedToolNames() {
			t := st.ToolCallsByName[name]
			line := fmt.Sprintf("ok %d", t.OK)
			if t.Error > 0 {
				line += fmt.Sprintf(", error %d", t.Error)
			}
			if t.Denied > 0 {
				line += fmt.Sprintf(", denied %d", t.Denied)
			}
			bw.printf("  %-20s %5d  (%s)\n", name, t.Total, line)
		}
	}
	return bw.err
}

// formatThousands formats an int with US thousands separators
// (12345 → "12,345"). Used by the stats renderer; lifted alongside
// the renderer itself so the CLI and TUI agree on output format.
func formatThousands(n int) string {
	if n < 0 {
		return "-" + formatThousands(-n)
	}
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	return formatThousands(n/1000) + fmt.Sprintf(",%03d", n%1000)
}
