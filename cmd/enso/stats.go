// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"fmt"
	"io"
	"os"
	"time"

	"github.com/TaraTheStar/enso/internal/session"
)

// runStats prints a per-store summary of activity. With --days N, only
// sessions updated within the last N days are counted.
func runStats(days int) error {
	store, err := session.Open()
	if err != nil {
		return fmt.Errorf("open session store: %w", err)
	}
	defer store.Close()

	var since time.Time
	if days > 0 {
		since = time.Now().AddDate(0, 0, -days)
	}

	st, err := session.ComputeStats(store, since)
	if err != nil {
		return err
	}
	return writeStats(os.Stdout, st, since)
}

func writeStats(w io.Writer, st session.Stats, since time.Time) error {
	if st.SessionCount == 0 {
		if since.IsZero() {
			_, err := fmt.Fprintln(w, "no sessions yet")
			return err
		}
		_, err := fmt.Fprintf(w, "no sessions since %s\n", since.Format("2006-01-02"))
		return err
	}

	bw := newWriter(w)

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
	bw.printf("Approx tokens (4-char heuristic): ~%s\n", commas(st.ApproxTotalTokens))

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

// commas formats an int with thousands separators, e.g. 12345 -> "12,345".
func commas(n int) string {
	if n < 0 {
		return "-" + commas(-n)
	}
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	return commas(n/1000) + fmt.Sprintf(",%03d", n%1000)
}
