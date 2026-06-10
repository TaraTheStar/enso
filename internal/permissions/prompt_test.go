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

// TestChecker_DenyMatchesCanonicalPath locks in the H3 fix: deny rules
// are matched against the resolved absolute path the tool will actually
// open (join cwd + Abs + Clean — mirroring tools.resolveRestricted),
// not the raw model-supplied string. Pre-fix, a relative `.ensoignore`
// entry like `.env` did not deny `read(/repo/.env)`, and an absolute
// `read(/repo/secrets/**)` deny was bypassed by passing `secrets/key`.
func TestChecker_DenyMatchesCanonicalPath(t *testing.T) {
	const cwd = "/repo"

	t.Run("relative ignore-derived deny vs absolute path arg", func(t *testing.T) {
		denies := IgnoreToDenyPatterns([]string{".env"}, cwd)
		c := NewChecker(nil, nil, denies, "allow")
		c.SetCwd(cwd)
		for _, path := range []string{"/repo/.env", ".env", "./sub/../.env", "/repo/sub/../.env"} {
			if d, _ := c.Check("read", map[string]any{"path": path}, nil); d != Deny {
				t.Errorf("read(%q): got %v, want Deny", path, d)
			}
		}
		// Unrelated files are unaffected.
		if d, _ := c.Check("read", map[string]any{"path": "/repo/main.go"}, nil); d != Allow {
			t.Errorf("unrelated file: got %v, want Allow", d)
		}
	})

	t.Run("relative config deny rule anchored at match time", func(t *testing.T) {
		c := NewChecker(nil, nil, []string{"edit(.env)", "write(./secrets/**)"}, "allow")
		c.SetCwd(cwd)
		if d, _ := c.Check("edit", map[string]any{"path": "/repo/.env"}, nil); d != Deny {
			t.Errorf("edit abs: got %v, want Deny", d)
		}
		if d, _ := c.Check("write", map[string]any{"path": "/repo/secrets/key.pem"}, nil); d != Deny {
			t.Errorf("write abs under relative glob deny: got %v, want Deny", d)
		}
	})

	t.Run("absolute deny rule vs relative path arg", func(t *testing.T) {
		c := NewChecker(nil, nil, []string{"read(/repo/secrets/**)"}, "allow")
		c.SetCwd(cwd)
		for _, path := range []string{"secrets/key", "./secrets/key", "sub/../secrets/key", "/repo/secrets/key"} {
			if d, _ := c.Check("read", map[string]any{"path": path}, nil); d != Deny {
				t.Errorf("read(%q): got %v, want Deny", path, d)
			}
		}
	})

	t.Run("traversal cannot escape into a denied scope unnoticed", func(t *testing.T) {
		// The arg is canonicalized before matching, so spelling
		// /repo/.env via `..` segments still trips the deny.
		c := NewChecker(nil, nil, []string{"read(/repo/.env)"}, "allow")
		c.SetCwd(cwd)
		if d, _ := c.Check("read", map[string]any{"path": "/repo/sub/../.env"}, nil); d != Deny {
			t.Errorf("traversal spelling: got %v, want Deny", d)
		}
	})

	t.Run("anywhere patterns keep matching everywhere", func(t *testing.T) {
		// `**`-rooted patterns are documented as filesystem-wide and
		// must not be silently narrowed to cwd by the anchoring.
		c := NewChecker([]string{"read(**)"}, nil, []string{"write(**/id_rsa)"}, "prompt")
		c.SetCwd(cwd)
		if d, _ := c.Check("read", map[string]any{"path": "/etc/hosts"}, nil); d != Allow {
			t.Errorf("read(**) outside cwd: got %v, want Allow", d)
		}
		if d, _ := c.Check("write", map[string]any{"path": "/home/x/.ssh/id_rsa"}, nil); d != Deny {
			t.Errorf("write(**/id_rsa) outside cwd: got %v, want Deny", d)
		}
	})
}

// TestBuildArgString_Deterministic locks in the M3 fix: tools without a
// dedicated extractArg case (MCP tools, memory_save, ...) are matched
// against a k=v string built from a map, which must be rendered in
// sorted key order — map iteration is randomized, and allow/ask/deny
// globs over it must not match nondeterministically.
func TestBuildArgString_Deterministic(t *testing.T) {
	args := map[string]any{"zeta": 1, "alpha": "x", "mid": true}
	const want = "alpha=x mid=true zeta=1"
	for range 50 {
		if got := buildArgString(args); got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	}
}
