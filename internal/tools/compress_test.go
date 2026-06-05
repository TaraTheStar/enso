// SPDX-License-Identifier: AGPL-3.0-or-later

package tools

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestCompressDiffLockfile(t *testing.T) {
	in := `diff --git a/go.sum b/go.sum
index 111..222 100644
--- a/go.sum
+++ b/go.sum
@@ -1,2 +1,3 @@
 github.com/a/b v1.0.0 h1:abc=
-github.com/c/d v1.0.0 h1:old=
+github.com/c/d v1.1.0 h1:new=
+github.com/e/f v2.0.0 h1:zzz=
diff --git a/main.go b/main.go
index 333..444 100644
--- a/main.go
+++ b/main.go
@@ -1,1 +1,1 @@
-var x = 1
+var x = 2`
	out, changed := compressDiff(in)
	if !changed {
		t.Fatal("expected diff to be compressed")
	}
	if strings.Contains(out, "h1:new=") {
		t.Fatalf("lockfile hunk body should be elided:\n%s", out)
	}
	if !strings.Contains(out, "lockfile diff elided: go.sum") {
		t.Fatalf("missing lockfile summary:\n%s", out)
	}
	// main.go is a real change and must survive verbatim.
	if !strings.Contains(out, "+var x = 2") {
		t.Fatalf("real code change dropped:\n%s", out)
	}
}

func TestCompressDiffWhitespaceHunk(t *testing.T) {
	in := `diff --git a/x.go b/x.go
index 1..2 100644
--- a/x.go
+++ b/x.go
@@ -1,2 +1,2 @@
-func foo() {
+func  foo()  {
@@ -10,1 +10,1 @@
-real := 1
+real := 2`
	out, changed := compressDiff(in)
	if !changed {
		t.Fatal("expected whitespace hunk to be elided")
	}
	if !strings.Contains(out, "whitespace-only hunk elided") {
		t.Fatalf("whitespace hunk not elided:\n%s", out)
	}
	if !strings.Contains(out, "+real := 2") {
		t.Fatalf("substantive hunk dropped:\n%s", out)
	}
}

func TestCompressLogTemplateCollapse(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 20; i++ {
		fmt.Fprintf(&b, "2026-06-05 12:00:%02d INFO request handled in %dms\n", i, i*3)
	}
	b.WriteString("ERROR something broke\n")
	out, changed := compressLog(b.String())
	if !changed {
		t.Fatal("expected repetitive log to collapse")
	}
	if !strings.Contains(out, "more lines matching this pattern elided") {
		t.Fatalf("no collapse annotation:\n%s", out)
	}
	if !strings.Contains(out, "ERROR something broke") {
		t.Fatalf("distinct line dropped:\n%s", out)
	}
	if estTokens(out) >= estTokens(b.String()) {
		t.Fatal("collapse did not reduce size")
	}
}

func TestCompressLogLeavesShortOutput(t *testing.T) {
	in := "line a\nline b\nline c\n"
	if _, changed := compressLog(in); changed {
		t.Fatal("short, non-repetitive output should be untouched")
	}
}

func TestCompressJSONArraySampling(t *testing.T) {
	type item struct {
		ID int `json:"id"`
	}
	items := make([]item, 100)
	for i := range items {
		items[i] = item{ID: i}
	}
	raw, _ := json.Marshal(items)
	out, changed := compressJSONArray(string(raw))
	if !changed {
		t.Fatal("expected large JSON array to be sampled")
	}
	if !strings.Contains(out, "array elements elided") {
		t.Fatalf("missing elision marker:\n%s", out)
	}
	if estTokens(out) >= estTokens(string(raw)) {
		t.Fatal("sampling did not reduce size")
	}
}

func TestCompressJSONArraySmallUntouched(t *testing.T) {
	out, changed := compressJSONArray(`[1,2,3]`)
	if changed || out != `[1,2,3]` {
		t.Fatal("small array should be untouched")
	}
}

func TestCompressOutputTokenAwareGuard(t *testing.T) {
	fs := NewFilterSet()
	// A filter that would replace tiny output with a longer "summary":
	// the token-aware guard must reject it (never inflate).
	f := &Filter{Name: "x", MatchCommand: "noop", OnEmpty: strings.Repeat("padding ", 50), StripLinesMatching: []string{"."}}
	if err := f.compile(); err != nil {
		t.Fatal(err)
	}
	fs.Add(f)
	raw := "tiny"
	out, saved := compressOutput(fs, "noop", raw)
	if out != raw || saved != 0 {
		t.Fatalf("guard should keep raw when compression inflates: out=%q saved=%d", out, saved)
	}
}

func TestCompressOutputFilterWins(t *testing.T) {
	fs := LoadFilterSet("", nil)
	raw := "=== RUN   TestX\n--- PASS: TestX (0.00s)\nok  \tgithub.com/x\t0.01s\n"
	out, saved := compressOutput(fs, "go test ./...", raw)
	if saved <= 0 {
		t.Fatalf("expected savings from go-test filter, got %d", saved)
	}
	if strings.Contains(out, "=== RUN") {
		t.Fatalf("filter did not apply:\n%s", out)
	}
}
