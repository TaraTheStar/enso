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
	got := IgnoreToDenyPatterns([]string{".env"})
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

// TestIgnoreEnforcedAsDeny wires LoadIgnoreFile + IgnoreToDenyPatterns
// through the live Allowlist to confirm `.ensoignore` content actually
// blocks file-touching tools.
func TestIgnoreEnforcedAsDeny(t *testing.T) {
	patterns := []string{".env", "secrets/**"}
	denies := IgnoreToDenyPatterns(patterns)
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
