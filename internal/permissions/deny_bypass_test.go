// SPDX-License-Identifier: AGPL-3.0-or-later

package permissions

import (
	"reflect"
	"testing"
)

func TestBashDeny_BlocksTopLevelChaining(t *testing.T) {
	// Each entry exercises a separator-class bypass against a
	// `bash(rm -rf *)` deny rule. With the full-command-only matcher
	// these were all bypasses; the segment-aware extension now blocks
	// each one. If any of these regress, we're back to the documented
	// AGENTS.md security gap.
	cases := []struct {
		name string
		cmd  string
	}{
		{"semicolon trailing", "echo hi; rm -rf /tmp/foo"},
		{"semicolon leading", "do_evil; rm -rf /"},
		{"and-and", "cd / && rm -rf *"},
		{"or-or", "cd missing || rm -rf *"},
		{"pipe", "ls | rm -rf *"},
		{"background", "sleep 1 & rm -rf /"},
		{"newline", "echo hi\nrm -rf /tmp"},
		{"multiple chains", "a; b && c; rm -rf x"},
		{"trailing semicolon then deny", "ls;rm -rf /"},
	}
	al := NewAllowlist(nil, nil, []string{"bash(rm -rf *)"})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			matched, kind := al.Match("bash", tc.cmd)
			if !matched || kind != KindDeny {
				t.Errorf("cmd=%q matched=%v kind=%v, want deny", tc.cmd, matched, kind)
			}
		})
	}
}

func TestBashDeny_DoesNotMatchUnrelatedCommands(t *testing.T) {
	// Negative cases: chained commands that don't include the denied
	// pattern in any segment must NOT trigger the deny. Without these
	// tests, an over-eager segment matcher would deny everything that
	// happens to chain through `;` or `&&`.
	al := NewAllowlist(nil, nil, []string{"bash(rm -rf *)"})
	cases := []string{
		"echo hello",
		"ls -la; cat foo",
		"git status && git diff",
		"npm install || npm ci",
	}
	for _, cmd := range cases {
		matched, kind := al.Match("bash", cmd)
		if matched && kind == KindDeny {
			t.Errorf("cmd=%q false-positive deny", cmd)
		}
	}
}

func TestBashDeny_QuotedSeparatorsDoNotSplit(t *testing.T) {
	// Inside single or double quotes, `;` and `&&` are literal text,
	// not separators. The split must honour that — otherwise users
	// can't write `echo "hello; world"` in a deny-watched session.
	al := NewAllowlist(nil, nil, []string{"bash(rm -rf *)"})
	cases := []struct {
		cmd      string
		wantDeny bool
	}{
		// Quoted text containing the deny pattern should NOT match —
		// `echo` is what runs, not `rm -rf`.
		{`echo 'rm -rf /tmp'`, false},
		{`echo "rm -rf /home"`, false},
		// Real chain after a quoted noisy string still gets caught.
		{`echo "; not a separator"; rm -rf /`, true},
	}
	for _, tc := range cases {
		matched, kind := al.Match("bash", tc.cmd)
		gotDeny := matched && kind == KindDeny
		if gotDeny != tc.wantDeny {
			t.Errorf("cmd=%q gotDeny=%v want %v", tc.cmd, gotDeny, tc.wantDeny)
		}
	}
}

func TestBashDeny_KnownResidualGap_DocumentedNotFixed(t *testing.T) {
	// Locks in the documented gap from AGENTS.md / TODO #21: the
	// segment splitter does NOT recurse into command substitution or
	// backticks. These are real bypasses; a security-conscious user
	// must rely on an isolating backend (`[backend] type = "podman"`
	// or `"lima"`) for protection. If a
	// future change extends the matcher to handle these, this test
	// SHOULD fail and be updated — that's a deliberate signpost.
	al := NewAllowlist(nil, nil, []string{"bash(rm -rf *)"})
	cases := []string{
		`echo $(rm -rf /tmp/foo)`,
		"echo `rm -rf /tmp/foo`",
		`eval "rm -rf /tmp/foo"`,
	}
	for _, cmd := range cases {
		matched, kind := al.Match("bash", cmd)
		if matched && kind == KindDeny {
			t.Errorf(
				"cmd=%q now matches deny — segment splitter has been extended? "+
					"If intentional, update this test and AGENTS.md.",
				cmd,
			)
		}
	}
}

func TestBashDeny_SegmentRespectsExistingMetacharGate_Allow(t *testing.T) {
	// Symmetry check: extending deny segment-matching mustn't
	// accidentally weaken the allow-rule metachar gate. An allow rule
	// for `bash(git *)` must still NOT auto-allow `git status; ls`
	// because the user didn't put `;` in the pattern.
	al := NewAllowlist([]string{"bash(git *)"}, nil, nil)
	matched, kind := al.Match("bash", "git status; ls")
	if matched && kind == KindAllow {
		t.Errorf("allow gate regressed: bash(git *) matched chained command")
	}
}

func TestBashSplitTopLevel(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"echo hi", []string{"echo hi"}},
		{"a;b", []string{"a", "b"}},
		{"a && b", []string{"a", "b"}},
		{"a || b", []string{"a", "b"}},
		{"a | b", []string{"a", "b"}},
		{"a & b", []string{"a", "b"}},
		{"a;b;c", []string{"a", "b", "c"}},
		{"a\nb", []string{"a", "b"}},
		{"  ;  ; foo  ;", []string{"foo"}},              // empty segments dropped
		{`echo "a; b"`, []string{`echo "a; b"`}},        // double-quoted separator stays
		{`echo 'x && y'`, []string{`echo 'x && y'`}},    // single-quoted ditto
		{"a && b; c | d", []string{"a", "b", "c", "d"}}, // mixed chain
		{"a&&b||c", []string{"a", "b", "c"}},            // no spaces around chains
	}
	for _, tc := range cases {
		got := bashSplitTopLevel(tc.in)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("bashSplitTopLevel(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
