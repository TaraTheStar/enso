// SPDX-License-Identifier: AGPL-3.0-or-later

package permissions

import (
	"bufio"
	"errors"
	"os"
	"strings"
)

// LoadIgnoreFile reads a `.ensoignore` (or any gitignore-style file) and
// returns its non-empty, non-comment entries. Lines starting with `#` are
// comments; trailing whitespace is trimmed; blank lines are skipped.
//
// We do NOT implement gitignore's full grammar (no `!` negation, no
// trailing-slash directory semantics) — patterns are passed to the
// permission matcher as-is, which means leading `/` and `**` work the
// way doublestar interprets them. For most users that's enough; complex
// gitignore files can be replicated with explicit `[permissions] deny`
// rules.
func LoadIgnoreFile(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var patterns []string
	scan := bufio.NewScanner(f)
	for scan.Scan() {
		line := strings.TrimSpace(scan.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		patterns = append(patterns, line)
	}
	return patterns, scan.Err()
}

// IgnoreToDenyPatterns expands each ignore pattern into one deny rule
// per file-touching tool: `read(<pattern>)`, `write(<pattern>)`,
// `edit(<pattern>)`, `grep(<pattern>)`, `glob(<pattern>)`. The
// agent loop applies these in addition to anything in
// `[permissions] deny`.
//
// For the path tools, relative patterns are anchored to cwd (see
// anchorPattern) so they match the canonical absolute path the checker
// compares against — a relative `.env` entry must deny `/repo/.env`
// however the model spells the path. The glob rule keeps the pattern
// verbatim: glob's argument is itself a pattern matched lexically, not
// a resolved path.
func IgnoreToDenyPatterns(ignore []string, cwd string) []string {
	pathTools := []string{"read", "write", "edit", "grep"}
	out := make([]string, 0, len(ignore)*(len(pathTools)+1))
	for _, pat := range ignore {
		for _, t := range pathTools {
			out = append(out, t+"("+anchorPattern(pat, cwd)+")")
		}
		out = append(out, "glob("+pat+")")
	}
	return out
}
