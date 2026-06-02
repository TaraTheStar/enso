// SPDX-License-Identifier: AGPL-3.0-or-later

package permissions

import "testing"

func TestDerivePattern(t *testing.T) {
	cases := []struct {
		name string
		tool string
		args map[string]any
		cwd  string
		want string
	}{
		{"bash multi-word: first-word generalisation",
			"bash", map[string]any{"cmd": "git status -s"}, "/repo", "bash(git *)"},
		{"bash single word: exact",
			"bash", map[string]any{"cmd": "ls"}, "/repo", "bash(ls)"},
		{"bash empty cmd",
			"bash", map[string]any{}, "/repo", "bash(*)"},
		// S8: read/grep are scoped to cwd, never `**`.
		{"read inside cwd: project-scoped subtree",
			"read", map[string]any{"path": "/repo/src/main.go"}, "/repo", "read(/repo/**)"},
		{"read of cwd itself: project-scoped subtree",
			"read", map[string]any{"path": "/repo"}, "/repo", "read(/repo/**)"},
		{"read outside cwd: exact cleaned path (no whole-fs grant)",
			"read", map[string]any{"path": "/etc/passwd"}, "/repo", "read(/etc/passwd)"},
		{"read with .. traversal: cleaned before scoping",
			"read", map[string]any{"path": "/repo/../etc/passwd"}, "/repo", "read(/etc/passwd)"},
		{"read empty cwd: exact",
			"read", map[string]any{"path": "/repo/x.go"}, "", "read(/repo/x.go)"},
		{"grep inside cwd: project-scoped subtree",
			"grep", map[string]any{"path": "/repo/pkg"}, "/repo", "grep(/repo/**)"},
		{"glob: exact pattern, never broadened",
			"glob", map[string]any{"pattern": "**/*.go"}, "/repo", "glob(**/*.go)"},
		{"write: exact path",
			"write", map[string]any{"path": "src/x.go"}, "/repo", "write(src/x.go)"},
		{"edit: exact path",
			"edit", map[string]any{"path": "src/x.go"}, "/repo", "edit(src/x.go)"},
		{"edit no path: fallback",
			"edit", map[string]any{}, "/repo", "edit(*)"},
		{"web_fetch: exact url",
			"web_fetch", map[string]any{"url": "https://example.com"}, "/repo", "web_fetch(https://example.com)"},
		{"unknown tool: fallback",
			"mcp__gitea__list_repos", map[string]any{}, "/repo", "mcp__gitea__list_repos(*)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DerivePattern(tc.tool, tc.args, tc.cwd); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestChecker_AddAllow_LiveMatch(t *testing.T) {
	c := NewChecker(nil, nil, nil, "deny") // strict default — only allow what's explicitly added
	if err := c.AddAllow("bash(git *)"); err != nil {
		t.Fatalf("add: %v", err)
	}
	d, err := c.Check("bash", map[string]any{"cmd": "git status"}, nil)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if d != Allow {
		t.Errorf("got %v, want Allow", d)
	}
	// Unrelated command still falls through to mode (deny).
	d, _ = c.Check("bash", map[string]any{"cmd": "rm -rf"}, nil)
	if d != Deny {
		t.Errorf("rm: got %v, want Deny", d)
	}
}

func TestChecker_AddAllow_RejectsBadPattern(t *testing.T) {
	c := NewChecker(nil, nil, nil, "prompt")
	if err := c.AddAllow("not a pattern"); err == nil {
		t.Errorf("want error for bare tool name without parens")
	}
}
