// SPDX-License-Identifier: AGPL-3.0-or-later

package permissions

import (
	"path/filepath"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// MatchTool checks if a tool name matches a pattern (supports * and **).
func MatchTool(pattern, tool string) bool {
	m, _ := doublestar.Match(pattern, tool)
	return m
}

// MatchPath returns true iff `path` (an absolute path passed by the
// tool) matches the doublestar pattern. There is no basename fallback —
// a bare `*.go` pattern intentionally does NOT match `/abs/foo.go`.
//
// Without that anchor, a rule like `read(*.md)` would silently allow
// reading `/etc/anything.md`, turning naturally-written file-extension
// rules into cwd-escape grants. Authors who want "any .go file" should
// write `**/*.go`; "any .go in this project" is `<cwd>/**/*.go` (or
// `./**/*.go` if the rule is in a project-tier config).
//
// Both pattern and path are filepath.Clean'd before matching, for two
// reasons: (1) a `..` traversal in the path can't escape a scoped pattern
// lexically — e.g. `read(/repo/**)` must NOT match `/repo/../etc/passwd`,
// which cleans to `/etc/passwd`; (2) cleaning both keeps relative-form
// patterns matching (`edit(./src/**)` against `./src/x.go` — both lose
// the `./`). Clean only resolves `.`/`..`, duplicate slashes, and
// trailing slashes; it leaves glob metacharacters (`*`, `**`, `?`)
// untouched, so doublestar semantics are preserved. (Symlink resolution
// is out of scope here — that containment check lives at the
// tool/workspace layer; see S10.)
func MatchPath(pattern, path string) bool {
	m, _ := doublestar.PathMatch(filepath.Clean(pattern), filepath.Clean(path))
	return m
}

// MatchCommand checks if a bash command matches a pattern. Supports
// multi-word patterns (e.g. `git push *`) by treating `*` as
// "any characters including spaces and slashes" — doublestar refuses to
// cross `/`, which is wrong for shell commands. Single-word patterns
// (e.g. `git`) are matched against the command's first word.
//
// This function is purely lexical. The shell-metacharacter gate that
// stops `bash(git *)` from auto-allowing `git status; rm -rf ~` lives
// in Allowlist.Match — it applies only to allow rules. See
// bashHasUnchainedMetachars in allowlist.go.
//
// Allowlist.Match also extends bash deny rules with a per-segment
// re-check (bashDenySegments) plus argv[0] normalization
// (normalizeBashSegment) and command-substitution extraction
// (bashSubstitutions), so `bash(rm -rf *)` deny fires on
// `do_evil; /bin/\rm -rf /` and `$(rm -rf /)` alike. Deny is
// best-effort by design (see Allowlist.Match) — for adversarial inputs
// an isolating backend (`[backend] type = "podman"` or `"lima"`) is the
// real boundary.
func MatchCommand(pattern, cmd string) bool {
	pattern = strings.TrimSpace(pattern)
	if pattern == "*" || pattern == "" {
		return true
	}
	if !strings.ContainsAny(pattern, " ") {
		// Single token: match against the command's first word.
		first := cmd
		if idx := strings.IndexByte(cmd, ' '); idx > 0 {
			first = cmd[:idx]
		}
		return globMatch(pattern, first)
	}
	return globMatch(pattern, cmd)
}

// globMatch is a literal-and-* wildcard matcher: `*` matches any run of
// characters (including spaces and `/`); `?` matches any single
// character. No bracket classes — keeps the implementation a few lines
// and matches what users actually write in permission patterns.
func globMatch(pattern, s string) bool {
	// Split on '*' and require each fragment to appear in order, with
	// the first as a prefix and the last as a suffix.
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return wildEq(parts[0], s)
	}
	if !wildPrefix(parts[0], s) {
		return false
	}
	s = s[len(parts[0]):]
	last := parts[len(parts)-1]
	for i := 1; i < len(parts)-1; i++ {
		idx := wildIndex(s, parts[i])
		if idx < 0 {
			return false
		}
		s = s[idx+len(parts[i]):]
	}
	return wildSuffix(last, s)
}

// wildEq compares pattern (with possible `?` wildcards) to s exactly.
func wildEq(pattern, s string) bool {
	if len(pattern) != len(s) {
		return false
	}
	for i := 0; i < len(pattern); i++ {
		if pattern[i] != '?' && pattern[i] != s[i] {
			return false
		}
	}
	return true
}

func wildPrefix(pattern, s string) bool {
	if len(pattern) > len(s) {
		return false
	}
	return wildEq(pattern, s[:len(pattern)])
}

func wildSuffix(pattern, s string) bool {
	if len(pattern) > len(s) {
		return false
	}
	return wildEq(pattern, s[len(s)-len(pattern):])
}

// wildIndex returns the first index of pattern in s, treating `?` as a
// single-character wildcard.
func wildIndex(s, pattern string) int {
	if len(pattern) == 0 {
		return 0
	}
	if !strings.ContainsRune(pattern, '?') {
		return strings.Index(s, pattern)
	}
	for i := 0; i+len(pattern) <= len(s); i++ {
		if wildEq(pattern, s[i:i+len(pattern)]) {
			return i
		}
	}
	return -1
}
