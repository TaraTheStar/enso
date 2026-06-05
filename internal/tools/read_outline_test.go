// SPDX-License-Identifier: AGPL-3.0-or-later

package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOutlineGo(t *testing.T) {
	src := `package foo

import (
	"fmt"
	"strings"
)

// Bar does a thing.
type Bar struct {
	Name string
	id   int
}

const Answer = 42

func (b *Bar) Hello(name string) (string, error) {
	x := strings.ToUpper(name)
	return fmt.Sprintf("hi %s", x), nil
}

func helper() { println("body") }
`
	out, ok := outlineGo(src)
	if !ok {
		t.Fatal("expected go source to parse")
	}
	for _, want := range []string{"package foo", "type Bar struct", "func (b *Bar) Hello(name string) (string, error)", "const Answer = 42", "func helper()"} {
		if !strings.Contains(out, want) {
			t.Errorf("outline missing %q:\n%s", want, out)
		}
	}
	// Function bodies must be gone.
	if strings.Contains(out, "strings.ToUpper") || strings.Contains(out, `println("body")`) {
		t.Errorf("outline leaked a function body:\n%s", out)
	}
}

func TestOutlineGoParseFailureFallsBack(t *testing.T) {
	// Not valid Go — outlineFile should still return something via the
	// heuristic rather than failing.
	out, ok := outlineFile("broken.go", "this is not go {{{")
	if !ok || out == "" {
		t.Fatalf("expected heuristic fallback, ok=%v out=%q", ok, out)
	}
}

func TestOutlineHeuristic(t *testing.T) {
	py := `class Foo:
    def method(self):
        x = 1
        return x

def top_level():
    pass
`
	out := outlineHeuristic(py)
	if !strings.Contains(out, "class Foo:") || !strings.Contains(out, "def method(self):") {
		t.Errorf("heuristic dropped a definition:\n%s", out)
	}
	if strings.Contains(out, "x = 1") {
		t.Errorf("heuristic kept a body line:\n%s", out)
	}
}

func TestReadToolOutlineMode(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.go")
	if err := os.WriteFile(p, []byte("package x\n\nfunc Foo() { y := 1; _ = y }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ac := &AgentContext{Cwd: dir, ReadSet: map[string]bool{}}
	res, err := ReadTool{}.Run(context.Background(),
		map[string]any{"path": p, "mode": "outline"}, ac)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(res.LLMOutput, "func Foo()") {
		t.Errorf("outline mode missing signature:\n%s", res.LLMOutput)
	}
	if strings.Contains(res.LLMOutput, "y := 1") {
		t.Errorf("outline mode leaked body:\n%s", res.LLMOutput)
	}
}
