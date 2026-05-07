// SPDX-License-Identifier: AGPL-3.0-or-later

// Package picker implements the file walker + fuzzy ranker used by the
// TUI's @-file picker overlay. Walking is best-effort: we skip the obvious
// noise directories (`.git`, `node_modules`, ...) but don't try to parse
// `.gitignore` — keeping it simple keeps the picker fast on large repos and
// avoids surprising users when their gitignore syntax is more elaborate
// than what a v1 matcher would handle.
package picker

import (
	"io/fs"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// hardSkipDirs are directory basenames the walker skips unconditionally.
// Any directory whose basename starts with "." is also skipped.
var hardSkipDirs = map[string]struct{}{
	"node_modules": {},
	"vendor":       {},
	"target":       {},
	"dist":         {},
	"build":        {},
	"__pycache__":  {},
	"bin":          {},
	"obj":          {},
}

// maxFiles caps the walker's output so we don't burn memory or freeze the
// UI on hand-built monorepos. Beyond this, the user has bigger problems
// than picker UX.
const maxFiles = 5000

// Walk returns file paths visible from `root`, alphabetised. Hidden and
// well-known build/output directories are skipped. Paths are emitted as
// rel-to-root for files inside `root`.
func Walk(root string) ([]string, error) {
	return WalkAll(root, nil, nil)
}

// WalkAll walks `root` plus each directory in `extras`. `ignore` is a
// list of doublestar globs (typically loaded from `.ensoignore`); any
// candidate file whose emitted path matches one is dropped. Files
// inside `root` are emitted as relative paths (e.g.
// `cmd/enso/main.go`); files in extras are emitted as their absolute
// paths so the model can read them outside the project. Total output
// is capped by maxFiles across every root combined.
func WalkAll(root string, extras []string, ignore []string) ([]string, error) {
	out, err := walkOne(filepath.Clean(root), false, ignore)
	if err != nil {
		return nil, err
	}
	if len(out) >= maxFiles {
		sort.Strings(out)
		return out, nil
	}
	for _, ex := range extras {
		ex = filepath.Clean(ex)
		if ex == "" || ex == "." {
			continue
		}
		more, err := walkOne(ex, true, ignore)
		if err != nil {
			continue
		}
		out = append(out, more...)
		if len(out) >= maxFiles {
			break
		}
	}
	sort.Strings(out)
	return out, nil
}

// matchesIgnore reports whether `path` matches any pattern in
// `patterns` using doublestar.PathMatch — the same matcher
// `permissions` uses for path-style globs, so the picker and the
// permissions checker agree on what's hidden.
func matchesIgnore(path string, patterns []string) bool {
	if len(patterns) == 0 {
		return false
	}
	base := filepath.Base(path)
	for _, p := range patterns {
		if m, _ := doublestar.PathMatch(p, path); m {
			return true
		}
		if m, _ := doublestar.PathMatch(p, base); m {
			return true
		}
	}
	return false
}

// walkOne walks a single root. When `absolute` is true, files are
// emitted as absolute paths (used for `additional_directories`).
// Otherwise paths are relative to `root`.
func walkOne(root string, absolute bool, ignore []string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if path == root {
			return nil
		}
		base := filepath.Base(path)
		if d.IsDir() {
			if _, skip := hardSkipDirs[base]; skip {
				return filepath.SkipDir
			}
			if strings.HasPrefix(base, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		var emit string
		if absolute {
			abs, err := filepath.Abs(path)
			if err != nil {
				return nil
			}
			emit = filepath.ToSlash(abs)
		} else {
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return nil
			}
			emit = filepath.ToSlash(rel)
		}
		if matchesIgnore(emit, ignore) {
			return nil
		}
		files = append(files, emit)
		if len(files) >= maxFiles {
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}

// Rank scores `files` against `query` and returns the top results
// (capped at `limit`), highest score first. Empty query → all files in
// directory order. Ranking, in priority order:
//
//  1. exact basename match (case-insensitive)
//  2. basename starts with query
//  3. basename contains query
//  4. path contains query
//
// Within a tier, shorter paths win — files closer to the project root
// typically matter more than ones buried in a fixture tree.
func Rank(files []string, query string, limit int) []string {
	if limit <= 0 {
		limit = 20
	}
	if query == "" {
		if len(files) > limit {
			return append([]string{}, files[:limit]...)
		}
		return append([]string{}, files...)
	}
	q := strings.ToLower(query)

	type scored struct {
		path  string
		tier  int // higher = better
		depth int
	}
	var hits []scored
	for _, f := range files {
		base := strings.ToLower(filepath.Base(f))
		full := strings.ToLower(f)
		tier := 0
		switch {
		case base == q:
			tier = 4
		case strings.HasPrefix(base, q):
			tier = 3
		case strings.Contains(base, q):
			tier = 2
		case strings.Contains(full, q):
			tier = 1
		default:
			continue
		}
		hits = append(hits, scored{path: f, tier: tier, depth: strings.Count(f, "/")})
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].tier != hits[j].tier {
			return hits[i].tier > hits[j].tier
		}
		if hits[i].depth != hits[j].depth {
			return hits[i].depth < hits[j].depth
		}
		return hits[i].path < hits[j].path
	})
	if len(hits) > limit {
		hits = hits[:limit]
	}
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.path
	}
	return out
}
