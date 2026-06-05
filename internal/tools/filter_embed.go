// SPDX-License-Identifier: AGPL-3.0-or-later

package tools

import (
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// embeddedFilters carries the default command-output filters shipped in the
// binary. They cover the highest-frequency, highest-bloat coding-agent
// commands (test runners, linters, builds, VCS, package installs). Users
// extend or override them with files under $XDG_CONFIG_HOME/enso/filters/.
//
//go:embed filters/*.toml
var embeddedFilters embed.FS

// LoadFilterSet builds the active FilterSet: embedded defaults first, then
// any *.toml under userDir overriding by filter name. A malformed embedded
// filter is a hard error (a build bug, caught by tests). A malformed or
// self-test-failing user filter is logged and skipped so a typo in a user
// file can never block agent startup. userDir == "" loads only the
// embedded defaults.
//
// Project-local filters are deliberately NOT consulted here: a filter that
// strips output could hide signal (a security warning) from the model, so —
// consistent with enso's rule that project config may only narrow, never
// inject — output-rewriting filters come only from the binary or the user's
// own config dir.
func LoadFilterSet(userDir string, logger *slog.Logger) *FilterSet {
	fset := NewFilterSet()

	entries, err := embeddedFilters.ReadDir("filters")
	if err != nil {
		// The embed directive guarantees this exists; treat a failure as
		// fatal-but-recoverable (return whatever we have).
		if logger != nil {
			logger.Warn("compress: reading embedded filters failed", "err", err)
		}
		return fset
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".toml") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names) // deterministic load order
	for _, name := range names {
		data, err := embeddedFilters.ReadFile(filepath.Join("filters", name))
		if err != nil {
			panic(fmt.Sprintf("compress: embedded filter %s unreadable: %v", name, err))
		}
		filters, err := parseFilters(data)
		if err != nil {
			panic(fmt.Sprintf("compress: embedded filter %s invalid: %v", name, err))
		}
		for _, f := range filters {
			fset.Add(f)
		}
	}

	if userDir != "" {
		loadUserFilters(fset, userDir, logger)
	}
	return fset
}

// loadUserFilters merges *.toml from dir into fset (override-by-name). Each
// user filter is self-tested before it is trusted; a failure skips just
// that filter. Missing dir is fine (most users won't have one).
func loadUserFilters(fset *FilterSet, dir string, logger *slog.Logger) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !os.IsNotExist(err) && logger != nil {
			logger.Warn("compress: reading user filters dir failed", "dir", dir, "err", err)
		}
		return
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".toml") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			if logger != nil {
				logger.Warn("compress: skipping unreadable user filter", "path", path, "err", err)
			}
			continue
		}
		filters, err := parseFilters(data)
		if err != nil {
			if logger != nil {
				logger.Warn("compress: skipping invalid user filter", "path", path, "err", err)
			}
			continue
		}
		for _, f := range filters {
			if err := f.runSelfTests(); err != nil {
				if logger != nil {
					logger.Warn("compress: skipping user filter that fails its own test", "path", path, "filter", f.Name, "err", err)
				}
				continue
			}
			fset.Add(f)
		}
	}
}

// fsReadString is a tiny helper used by tests to read an embedded filter
// file by name; kept here so the embed.FS stays unexported.
func fsReadString(name string) (string, error) {
	b, err := fs.ReadFile(embeddedFilters, filepath.Join("filters", name))
	return string(b), err
}
