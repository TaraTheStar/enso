// SPDX-License-Identifier: AGPL-3.0-or-later

package slash

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestExampleSkillLoadable parses the shipped example skill via the
// real loader. Catches accidental frontmatter regressions when the
// example is updated.
func TestExampleSkillLoadable(t *testing.T) {
	src, err := os.ReadFile("../../examples/skills/explain-this.md")
	if err != nil {
		t.Skipf("examples not available: %v", err)
	}
	dir := t.TempDir()
	sub := filepath.Join(dir, ".enso", "skills")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "explain-this.md"), src, 0o644); err != nil {
		t.Fatal(err)
	}
	skills, err := LoadSkills(dir)
	if err != nil {
		t.Fatalf("LoadSkills: %v", err)
	}
	if len(skills) == 0 {
		t.Fatal("no skills loaded")
	}
	var got *Skill
	for _, s := range skills {
		if s.Name() == "explain-this" {
			got = s
			break
		}
	}
	if got == nil {
		t.Fatal("explain-this skill not found")
	}
	if !strings.Contains(got.Description(), "summariser") {
		t.Errorf("description not parsed: %q", got.Description())
	}
	tools := got.AllowedTools()
	for _, want := range []string{"read", "grep", "lsp_hover"} {
		found := false
		for _, tn := range tools {
			if tn == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected allowed-tool %q in %v", want, tools)
		}
	}
}
