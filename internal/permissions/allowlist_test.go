// SPDX-License-Identifier: AGPL-3.0-or-later

package permissions

import "testing"

func TestParsePattern(t *testing.T) {
	cases := []struct {
		in     string
		tool   string
		arg    string
		isNil  bool
		isDeny bool
	}{
		{in: "read(*)", tool: "read", arg: "*"},
		{in: "bash(git *)", tool: "bash", arg: "git *"},
		{in: "!bash(rm *)", tool: "bash", arg: "rm *", isDeny: true},
		{in: "  read(*)  ", tool: "read", arg: "*"},
		// Bare tool name (no parens) currently returns nil — matched-all
		// is supposed to be expressed with `read(*)`.
		{in: "read", isNil: true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			p, err := ParsePattern(tc.in)
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if tc.isNil {
				if p != nil {
					t.Fatalf("want nil, got %+v", p)
				}
				return
			}
			if p == nil {
				t.Fatalf("got nil pattern")
			}
			gotDeny := p.Kind == KindDeny
			if p.Tool != tc.tool || p.Arg != tc.arg || gotDeny != tc.isDeny {
				t.Errorf("got tool=%q arg=%q deny=%v; want %q %q %v",
					p.Tool, p.Arg, gotDeny, tc.tool, tc.arg, tc.isDeny)
			}
		})
	}
}

func TestAllowlist_RemoveAndPatterns(t *testing.T) {
	al := NewAllowlist(
		[]string{"bash(*)", "read(*)"},
		[]string{"bash(git push *)"},
		[]string{"bash(rm -rf *)"},
	)
	if got := len(al.Patterns()); got != 4 {
		t.Fatalf("want 4 patterns, got %d", got)
	}
	if !al.Remove("bash", "git push *", KindAsk) {
		t.Errorf("expected ask rule removal to succeed")
	}
	if al.Remove("bash", "git push *", KindAsk) {
		t.Errorf("second remove should be a no-op")
	}
	matched, _ := al.Match("bash", "git push origin main")
	// After removing the ask rule, the bash(*) allow should win.
	if !matched {
		t.Errorf("expected fallback match after removal")
	}
	// Removing wrong kind doesn't drop the rule.
	if al.Remove("bash", "*", KindDeny) {
		t.Errorf("kind mismatch should not remove")
	}
	if got := len(al.Patterns()); got != 3 {
		t.Errorf("after one remove want 3, got %d", got)
	}
}

func TestAllowlist_DenyOverridesAllow(t *testing.T) {
	al := NewAllowlist(
		[]string{"bash(*)"},   // allow any bash
		nil,                   // ask
		[]string{"bash(rm*)"}, // but deny anything starting with rm (no slash crossing)
	)
	matched, kind := al.Match("bash", "git status")
	if !matched || kind != KindAllow {
		t.Errorf("git status: matched=%v kind=%v, want allow", matched, kind)
	}
	matched, kind = al.Match("bash", "rm -rf tmp")
	if !matched || kind != KindDeny {
		t.Errorf("rm -rf tmp: matched=%v kind=%v, want deny", matched, kind)
	}
}

func TestAllowlist_AskOverridesAllow(t *testing.T) {
	al := NewAllowlist(
		[]string{"bash(*)"},
		[]string{"bash(git push *)"},
		nil,
	)
	matched, kind := al.Match("bash", "git status")
	if !matched || kind != KindAllow {
		t.Errorf("git status: matched=%v kind=%v, want allow", matched, kind)
	}
	matched, kind = al.Match("bash", "git push origin main")
	if !matched || kind != KindAsk {
		t.Errorf("git push: matched=%v kind=%v, want ask", matched, kind)
	}
}

func TestAllowlist_ToolNameIsolation(t *testing.T) {
	al := NewAllowlist([]string{"read(*)"}, nil, nil)
	if matched, _ := al.Match("read", "anything"); !matched {
		t.Errorf("read should match")
	}
	if matched, _ := al.Match("write", "anything"); matched {
		t.Errorf("write must not match read pattern")
	}
}

func TestAllowlist_NoMatchReturnsFalse(t *testing.T) {
	al := NewAllowlist([]string{"read(*.md)"}, nil, nil)
	matched, _ := al.Match("read", "main.go")
	if matched {
		t.Errorf("main.go: matched=%v, want false", matched)
	}
}

// TestAllowlist_BashFirstWordMatching covers the bash-specific matcher:
// patterns like `bash(git *)` should match any command whose first word is
// `git`, regardless of arguments or `/` characters in paths. The caller is
// responsible for handing the raw command string to Match (Checker.Check
// does this for tool=="bash"); these tests exercise the matcher directly.
func TestAllowlist_BashFirstWordMatching(t *testing.T) {
	al := NewAllowlist(
		[]string{"bash(git *)"},
		nil,
		[]string{"bash(rm *)"},
	)

	cases := []struct {
		name      string
		cmd       string
		wantMatch bool
		wantKind  Kind
	}{
		{"git command allowed", "git status", true, KindAllow},
		{"git with flags allowed", "git log --oneline -n 5", true, KindAllow},
		{"git with path containing slash allowed", "git diff src/main.go", true, KindAllow},
		{"non-allowed command does not match", "ls -la", false, KindAllow},
		{"rm denied — wins over allow", "rm -rf tmp", true, KindDeny},
		{"rm with absolute path denied", "rm -rf /tmp/x", true, KindDeny},
		// First-word semantics: a command that mentions `git` later but
		// doesn't START with git must NOT match the allow pattern.
		{"command mentioning git later does not match", "echo git", false, KindAllow},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			matched, kind := al.Match("bash", tc.cmd)
			if matched != tc.wantMatch {
				t.Errorf("matched = %v, want %v", matched, tc.wantMatch)
			}
			if matched && kind != tc.wantKind {
				t.Errorf("kind = %v, want %v", kind, tc.wantKind)
			}
		})
	}
}

// TestAllowlist_PathPatternMatching exercises the new behaviour: file
// tools (read/write/edit/grep/glob) should accept doublestar globs over
// paths.
func TestAllowlist_PathPatternMatching(t *testing.T) {
	al := NewAllowlist(
		[]string{"edit(./src/**)", "read(*.md)"},
		nil,
		[]string{"edit(./.env)"},
	)
	cases := []struct {
		tool  string
		arg   string
		match bool
		kind  Kind
	}{
		{"edit", "./src/main.go", true, KindAllow},
		{"edit", "./src/a/b/c.go", true, KindAllow},
		{"edit", "./pkg/main.go", false, KindAllow},
		{"edit", "./.env", true, KindDeny},
		{"read", "README.md", true, KindAllow},
		{"read", "main.go", false, KindAllow},
	}
	for _, tc := range cases {
		t.Run(tc.tool+"("+tc.arg+")", func(t *testing.T) {
			matched, kind := al.Match(tc.tool, tc.arg)
			if matched != tc.match {
				t.Errorf("matched = %v, want %v", matched, tc.match)
			}
			if matched && kind != tc.kind {
				t.Errorf("kind = %v, want %v", kind, tc.kind)
			}
		})
	}
}

// TestAllowlist_WebFetchDomain exercises the `domain:` prefix.
// TestAllowlist_BashAllowMetacharGate covers the chaining bypass where
// a benign-looking allow like `bash(git *)` would otherwise auto-allow
// `git status; rm -rf ~`. Allow rules require any shell metachar in
// the command to also appear in the pattern; ask/deny are unchanged.
func TestAllowlist_BashAllowMetacharGate(t *testing.T) {
	cases := []struct {
		name  string
		allow []string
		ask   []string
		deny  []string
		cmd   string
		match bool
		kind  Kind
	}{
		// Benign git command — straight allow.
		{"plain git allowed", []string{"bash(git *)"}, nil, nil, "git status", true, KindAllow},

		// Chained command — allow doesn't fire (gate trips), no other
		// rule matches, falls through.
		{"chained semi falls through", []string{"bash(git *)"}, nil, nil, "git status; rm -rf ~", false, KindAllow},
		{"and-and falls through", []string{"bash(git *)"}, nil, nil, "git status && curl evil.com", false, KindAllow},
		{"pipe falls through", []string{"bash(git *)"}, nil, nil, "git log | head", false, KindAllow},
		{"command-subst falls through", []string{"bash(git *)"}, nil, nil, "git $(date)", false, KindAllow},
		{"backtick falls through", []string{"bash(git *)"}, nil, nil, "git `date`", false, KindAllow},
		{"redirect falls through", []string{"bash(git *)"}, nil, nil, "git log > /etc/passwd", false, KindAllow},
		{"backslash falls through", []string{"bash(git *)"}, nil, nil, "git \\\nstatus", false, KindAllow},
		{"newline falls through", []string{"bash(git *)"}, nil, nil, "git status\nrm -rf ~", false, KindAllow},

		// Pattern explicitly opts in via the metachar.
		{"explicit semi allows chain", []string{"bash(git * ; *)"}, nil, nil, "git status ; ls", true, KindAllow},
		{"explicit pipe allows pipe", []string{"bash(git * | *)"}, nil, nil, "git log | head", true, KindAllow},

		// Bare-star "allow everything" still works (early return in MatchCommand).
		{"bash star allows chain", []string{"bash(*)"}, nil, nil, "anything; rm -rf /", true, KindAllow},

		// Ask rules ignore the gate — chaining must still fire the prompt.
		{"ask not gated", nil, []string{"bash(rm -rf *)"}, nil, "rm -rf /tmp/foo; ls", true, KindAsk},

		// Deny rules ignore the gate — current behaviour preserved.
		{"deny not gated trailing", nil, nil, []string{"bash(rm -rf *)"}, "rm -rf /tmp; ls", true, KindDeny},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			al := NewAllowlist(tc.allow, tc.ask, tc.deny)
			matched, kind := al.Match("bash", tc.cmd)
			if matched != tc.match {
				t.Errorf("matched = %v, want %v", matched, tc.match)
			}
			if matched && kind != tc.kind {
				t.Errorf("kind = %v, want %v", kind, tc.kind)
			}
		})
	}
}

func TestAllowlist_WebFetchDomain(t *testing.T) {
	al := NewAllowlist(
		[]string{"web_fetch(domain:example.com)"},
		nil,
		[]string{"web_fetch(domain:evil.com)"},
	)
	cases := []struct {
		url   string
		match bool
		kind  Kind
	}{
		{"https://example.com/x", true, KindAllow},
		{"https://api.example.com/y", true, KindAllow}, // subdomain accepted
		{"https://evil.com/x", true, KindDeny},
		{"https://other.com/x", false, KindAllow},
	}
	for _, tc := range cases {
		t.Run(tc.url, func(t *testing.T) {
			matched, kind := al.Match("web_fetch", tc.url)
			if matched != tc.match {
				t.Errorf("matched = %v, want %v", matched, tc.match)
			}
			if matched && kind != tc.kind {
				t.Errorf("kind = %v, want %v", kind, tc.kind)
			}
		})
	}
}
