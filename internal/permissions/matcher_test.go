// SPDX-License-Identifier: AGPL-3.0-or-later

package permissions

import "testing"

func TestMatchTool(t *testing.T) {
	cases := []struct {
		pattern, tool string
		want          bool
	}{
		{"read", "read", true},
		{"read", "write", false},
		{"*", "anything", true},
		{"mcp__*", "mcp__gitea__list_repos", true},
		{"mcp__*", "read", false},
	}
	for _, tc := range cases {
		t.Run(tc.pattern+"/"+tc.tool, func(t *testing.T) {
			if got := MatchTool(tc.pattern, tc.tool); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestMatchPath(t *testing.T) {
	cases := []struct {
		pattern, path string
		want          bool
	}{
		{"/repo/**", "/repo/internal/x.go", true},
		{"/repo/**", "/other/x.go", false},
		// Bare extension patterns DO NOT match absolute paths — that
		// fallback would let `read(*.md)` exfiltrate /etc/passwd.md.
		{"*.go", "/repo/internal/x.go", false},
		{"*.md", "/repo/README.md", false},
		{"*.go", "/repo/x.txt", false},
		// Authors get the same intent via doublestar's `**`.
		{"**/*.go", "/repo/internal/x.go", true},
		{"**/*.md", "/repo/README.md", true},
		{"**/*.go", "/repo/x.txt", false},
		// Project-scoped — the safer way: rooted prefix + recursive glob.
		{"/repo/**/*.md", "/repo/sub/README.md", true},
		{"/repo/**/*.md", "/etc/README.md", false},
		// Single-segment patterns still work for single-segment paths.
		{"*.go", "x.go", true},
	}
	for _, tc := range cases {
		t.Run(tc.pattern+"/"+tc.path, func(t *testing.T) {
			if got := MatchPath(tc.pattern, tc.path); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestMatchCommand(t *testing.T) {
	cases := []struct {
		pattern, cmd string
		want         bool
	}{
		// Pattern with first-word + glob: only the first command word is checked
		// against the pattern's first word.
		{"git *", "git status", true},
		{"git *", "git log --oneline", true},
		{"git *", "rm -rf /", false},
		{"rm *", "rm file.txt", true},
		{"rm *", "git rm file.txt", false}, // first word is git, not rm
		// Bare pattern (no space): match against the command's first
		// word, with glob `*`/`?` semantics that cross `/` since shell
		// commands aren't path-structured.
		{"*", "anything goes", true},
		{"echo*", "echo hello", true},  // first word "echo" matches "echo*" (* can be empty)
		{"echo*", "echofoo bar", true}, // first word "echofoo" matches "echo*"
		{"ls?", "lsa", true},           // ? matches single char
		{"echo hello", "echo hello", true},
		// Multi-word patterns match the whole command, * crosses everything.
		{"git push *", "git push origin main", true},
		{"git push *", "git status", false},
		{"git push *", "git push origin feat/x", true}, // * crosses /
	}
	for _, tc := range cases {
		t.Run(tc.pattern+"/"+tc.cmd, func(t *testing.T) {
			if got := MatchCommand(tc.pattern, tc.cmd); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}
