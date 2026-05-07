// SPDX-License-Identifier: AGPL-3.0-or-later

package agents

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExampleAgentLoadable(t *testing.T) {
	src, err := os.ReadFile("../../examples/agents/reviewer.md")
	if err != nil {
		t.Skipf("examples not available: %v", err)
	}
	dir := t.TempDir()
	sub := filepath.Join(dir, ".enso", "agents")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "reviewer.md"), src, 0o644); err != nil {
		t.Fatal(err)
	}
	spec, err := Find(dir, "reviewer")
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if spec == nil {
		t.Fatal("nil spec")
	}
	if !strings.Contains(spec.PromptAppend, "code reviewer") {
		t.Errorf("body not loaded: %q", spec.PromptAppend)
	}
	for _, want := range []string{"read", "grep", "lsp_hover"} {
		found := false
		for _, t := range spec.AllowedTools {
			if t == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected allowed-tool %q in %v", want, spec.AllowedTools)
		}
	}
}
