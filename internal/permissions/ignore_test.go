// SPDX-License-Identifier: AGPL-3.0-or-later

package permissions

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestLoadIgnoreFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".ensoignore")
	body := `# comment
.env

# another comment
secrets/**
*.pem
   trailing-space-is-trimmed
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := LoadIgnoreFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{".env", "secrets/**", "*.pem", "trailing-space-is-trimmed"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestLoadIgnoreFileMissing(t *testing.T) {
	got, err := LoadIgnoreFile(filepath.Join(t.TempDir(), "nope"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("missing file should yield empty slice, got %v", got)
	}
}

func TestIgnoreToDenyPatterns(t *testing.T) {
	got := IgnoreToDenyPatterns([]string{".env"}, "")
	if len(got) != 5 {
		t.Fatalf("expected one rule per file tool (5), got %d: %v", len(got), got)
	}
	wantTools := map[string]bool{"read": false, "write": false, "edit": false, "grep": false, "glob": false}
	for _, p := range got {
		// shape: tool(.env)
		idx := -1
		for i, c := range p {
			if c == '(' {
				idx = i
				break
			}
		}
		if idx < 0 {
			t.Errorf("malformed: %q", p)
			continue
		}
		wantTools[p[:idx]] = true
	}
	for tool, ok := range wantTools {
		if !ok {
			t.Errorf("missing rule for tool %q", tool)
		}
	}
}

// TestIgnoreToDenyPatterns_AnchorsRelativeToCwd locks in the H3 fix on
// the pattern side: relative ignore entries are anchored to cwd for the
// path tools so they match the canonical absolute paths the checker
// compares against; absolute and `**`-rooted entries pass through; the
// glob rule always keeps the pattern verbatim (glob's arg is itself a
// pattern, never a resolved path).
func TestIgnoreToDenyPatterns_AnchorsRelativeToCwd(t *testing.T) {
	got := IgnoreToDenyPatterns([]string{".env", "/abs/**", "**/key.pem"}, "/repo")
	want := map[string]bool{
		"read(/repo/.env)": false, "write(/repo/.env)": false,
		"edit(/repo/.env)": false, "grep(/repo/.env)": false,
		"glob(.env)": false,
		// Absolute entries are already anchored.
		"read(/abs/**)": false, "glob(/abs/**)": false,
		// `**/...` means "anywhere" — never narrowed to cwd.
		"read(**/key.pem)": false, "glob(**/key.pem)": false,
	}
	for _, p := range got {
		if _, ok := want[p]; ok {
			want[p] = true
		}
	}
	for p, seen := range want {
		if !seen {
			t.Errorf("missing expected rule %q in %v", p, got)
		}
	}
}

// TestIgnoreEnforcedAsDeny wires LoadIgnoreFile + IgnoreToDenyPatterns
// through the live Allowlist to confirm `.ensoignore` content actually
// blocks file-touching tools.
func TestIgnoreEnforcedAsDeny(t *testing.T) {
	patterns := []string{".env", "secrets/**"}
	denies := IgnoreToDenyPatterns(patterns, "")
	al := NewAllowlist(nil, nil, denies)

	cases := []struct {
		tool, arg string
		match     bool
		kind      Kind
	}{
		{"read", ".env", true, KindDeny},
		{"edit", ".env", true, KindDeny},
		{"write", "secrets/key.pem", true, KindDeny},
		{"grep", "secrets/foo/bar", true, KindDeny},
		{"read", "src/main.go", false, KindAllow},
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
