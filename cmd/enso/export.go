// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"fmt"
	"io"
	"os"

	"github.com/TaraTheStar/enso/internal/session"
)

// runExport reads a session by id from $XDG_DATA_HOME/enso/enso.db and
// writes a markdown transcript to stdout (or --out path). The actual
// rendering lives in session.WriteMarkdown so the TUI's /export slash
// command can share it.
func runExport(id, outPath string) error {
	store, err := session.Open()
	if err != nil {
		return fmt.Errorf("open session store: %w", err)
	}
	defer store.Close()

	var w io.Writer = os.Stdout
	if outPath != "" {
		f, err := os.Create(outPath)
		if err != nil {
			return fmt.Errorf("create %s: %w", outPath, err)
		}
		defer f.Close()
		w = f
	}
	return session.WriteMarkdownByID(w, store, id)
}
