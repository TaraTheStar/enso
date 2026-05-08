// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"fmt"
	"os"
	"time"

	"github.com/TaraTheStar/enso/internal/session"
)

// runStats prints a per-store summary of activity. With --days N, only
// sessions updated within the last N days are counted. The actual
// rendering lives in session.WriteStatsText so the TUI's /stats slash
// command can share it.
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
	return session.WriteStatsText(os.Stdout, st, since)
}
