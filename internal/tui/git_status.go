// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// gitChange is a single entry from `git status --porcelain` — a two-char
// status code plus the path. We keep it as the raw two characters so the
// renderer can pick a color from either the index or work-tree slot
// (modified-but-staged still shows up correctly).
type gitChange struct {
	Status string // exactly two bytes: index slot, work-tree slot
	Path   string
}

// fetchGitChanges runs `git status --porcelain` against cwd with a tight
// timeout. Returns nil when:
//   - cwd isn't a git work tree (the most common non-error case)
//   - git is missing from PATH
//   - the call times out (slow filesystem, huge repo)
//
// On success returns the parsed entries (or an empty slice for a clean
// tree). The caller renders nothing for nil and renders nothing for an
// empty slice — both states mean "no changes section."
//
// Path safety: we don't pass `-z`, so paths with embedded newlines would
// be mis-parsed. Git quotes paths with control chars or spaces by default
// (config core.quotePath), so the cosmetic damage is bounded; this trades
// a sliver of correctness for a much simpler parser.
func fetchGitChanges(cwd string) []gitChange {
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "status", "--porcelain")
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	trimmed := strings.TrimRight(string(out), "\n")
	if trimmed == "" {
		return []gitChange{}
	}
	lines := strings.Split(trimmed, "\n")
	changes := make([]gitChange, 0, len(lines))
	for _, line := range lines {
		// Each line is `XY PATH` (two status chars, space, path). Lines
		// shorter than 4 bytes can't be valid; skip defensively rather
		// than indexing past the end.
		if len(line) < 4 {
			continue
		}
		changes = append(changes, gitChange{
			Status: line[:2],
			Path:   line[3:],
		})
	}
	return changes
}
