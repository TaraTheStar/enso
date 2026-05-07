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
func IgnoreToDenyPatterns(ignore []string) []string {
	tools := []string{"read", "write", "edit", "grep", "glob"}
	out := make([]string, 0, len(ignore)*len(tools))
	for _, pat := range ignore {
		for _, t := range tools {
			out = append(out, t+"("+pat+")")
		}
	}
	return out
}
