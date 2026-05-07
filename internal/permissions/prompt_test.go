// SPDX-License-Identifier: AGPL-3.0-or-later

package permissions

import "testing"

func TestDerivePattern(t *testing.T) {
	cases := []struct {
		name string
		tool string
		args map[string]any
		want string
	}{
		{"bash multi-word: first-word generalisation",
			"bash", map[string]any{"cmd": "git status -s"}, "bash(git *)"},
		{"bash single word: exact",
			"bash", map[string]any{"cmd": "ls"}, "bash(ls)"},
		{"bash empty cmd",
			"bash", map[string]any{}, "bash(*)"},
		{"read: broad",
			"read", map[string]any{"path": "README.md"}, "read(**)"},
		{"grep: broad",
			"grep", map[string]any{"pattern": "TODO"}, "grep(**)"},
		{"write: exact path",
			"write", map[string]any{"path": "src/x.go"}, "write(src/x.go)"},
		{"edit: exact path",
			"edit", map[string]any{"path": "src/x.go"}, "edit(src/x.go)"},
		{"edit no path: fallback",
			"edit", map[string]any{}, "edit(*)"},
		{"web_fetch: exact url",
			"web_fetch", map[string]any{"url": "https://example.com"}, "web_fetch(https://example.com)"},
		{"unknown tool: fallback",
			"mcp__gitea__list_repos", map[string]any{}, "mcp__gitea__list_repos(*)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DerivePattern(tc.tool, tc.args); got != tc.want {
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
