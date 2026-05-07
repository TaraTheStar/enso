// SPDX-License-Identifier: AGPL-3.0-or-later

package workflow

import (
	"strings"
	"testing"
)

func TestParse_LinearWorkflow(t *testing.T) {
	src := []byte(`---
roles:
  planner:
    tools: [read]
  coder:
    tools: [read, edit]
  reviewer:
    tools: [read]
edges:
  - planner -> coder
  - coder -> reviewer
---

## planner

Plan: {{ .Args }}

## coder

{{ .planner.output }}

## reviewer

{{ .coder.output }}
`)
	wf, err := Parse("build-feature.md", src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if wf.Name != "build-feature" {
		t.Errorf("name = %q, want build-feature", wf.Name)
	}
	if len(wf.Roles) != 3 {
		t.Errorf("got %d roles, want 3", len(wf.Roles))
	}
	want := []string{"planner", "coder", "reviewer"}
	if len(wf.RoleOrder) != len(want) {
		t.Fatalf("RoleOrder length = %d, want %d", len(wf.RoleOrder), len(want))
	}
	for i, n := range want {
		if wf.RoleOrder[i] != n {
			t.Errorf("RoleOrder[%d] = %q, want %q", i, wf.RoleOrder[i], n)
		}
	}
	// Tools restriction parsed correctly.
	if got := wf.Roles["coder"].AllowedTools; len(got) != 2 || got[0] != "read" || got[1] != "edit" {
		t.Errorf("coder.AllowedTools = %v, want [read edit]", got)
	}
}

func TestParse_ParallelBranches(t *testing.T) {
	src := []byte(`---
roles:
  fanout:
    tools: []
  branchA:
    tools: []
  branchB:
    tools: []
  joiner:
    tools: []
edges:
  - fanout -> branchA
  - fanout -> branchB
  - branchA -> joiner
  - branchB -> joiner
---

## fanout
fan
## branchA
A
## branchB
B
## joiner
J: {{ .branchA.output }} + {{ .branchB.output }}
`)
	wf, err := Parse("diamond.md", src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// Topology: fanout must come first; joiner must come last; branches
	// are siblings with stable ordering (alphabetical via sortStrings).
	if wf.RoleOrder[0] != "fanout" {
		t.Errorf("RoleOrder[0] = %q, want fanout", wf.RoleOrder[0])
	}
	if wf.RoleOrder[len(wf.RoleOrder)-1] != "joiner" {
		t.Errorf("last role = %q, want joiner", wf.RoleOrder[len(wf.RoleOrder)-1])
	}
	mid := wf.RoleOrder[1:3]
	if !((mid[0] == "branchA" && mid[1] == "branchB") || (mid[0] == "branchB" && mid[1] == "branchA")) {
		t.Errorf("middle = %v, want branchA/branchB siblings", mid)
	}
}

func TestParse_CycleDetected(t *testing.T) {
	src := []byte(`---
roles:
  a:
  b:
  c:
edges:
  - a -> b
  - b -> c
  - c -> a
---

## a
A
## b
B
## c
C
`)
	_, err := Parse("cycle.md", src)
	if err == nil {
		t.Fatal("expected cycle error, got nil")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Errorf("err = %v, expected to mention 'cycle'", err)
	}
}

func TestParse_EdgeReferencesUnknownRole(t *testing.T) {
	src := []byte(`---
roles:
  a:
edges:
  - a -> ghost
---

## a
A
`)
	_, err := Parse("badedge.md", src)
	if err == nil {
		t.Fatal("expected unknown-role error")
	}
	if !strings.Contains(err.Error(), "unknown role") {
		t.Errorf("err = %v, want 'unknown role'", err)
	}
}

func TestParse_EmptyRoles(t *testing.T) {
	src := []byte(`---
roles: {}
edges: []
---

## anything
no
`)
	_, err := Parse("empty.md", src)
	if err == nil || !strings.Contains(err.Error(), "no roles") {
		t.Errorf("got %v, want 'no roles' error", err)
	}
}

func TestParse_MissingBodySection(t *testing.T) {
	src := []byte(`---
roles:
  planner:
  coder:
edges:
  - planner -> coder
---

## planner
Plan: {{ .Args }}
`)
	_, err := Parse("missingbody.md", src)
	if err == nil {
		t.Fatal("expected error for missing `## coder` section")
	}
	if !strings.Contains(err.Error(), "coder") || !strings.Contains(err.Error(), "section") {
		t.Errorf("err = %v, want mention of coder + section", err)
	}
}

func TestParse_BadEdgeSyntax(t *testing.T) {
	src := []byte(`---
roles:
  a:
  b:
edges:
  - "a => b"
---

## a
A
## b
B
`)
	_, err := Parse("badedge.md", src)
	if err == nil {
		t.Fatal("expected edge-syntax error")
	}
}

func TestParseEdge(t *testing.T) {
	cases := []struct {
		in        string
		from, to  string
		wantError bool
	}{
		{"a -> b", "a", "b", false},
		{"  planner   ->   coder  ", "planner", "coder", false},
		{"a -> b -> c", "", "", true}, // multi-arrow not supported
		{"a => b", "", "", true},
		{"a", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			e, err := parseEdge(tc.in)
			if tc.wantError {
				if err == nil {
					t.Errorf("expected error for %q, got %+v", tc.in, e)
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}
			if e.From != tc.from || e.To != tc.to {
				t.Errorf("got %+v, want from=%q to=%q", e, tc.from, tc.to)
			}
		})
	}
}

func TestSplitSections(t *testing.T) {
	body := `intro junk should be ignored

## alpha

alpha body line 1
alpha body line 2

## beta

beta body
`
	sections, err := splitSections(body)
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	if got := sections["alpha"]; !strings.Contains(got, "alpha body line 1") {
		t.Errorf("alpha section = %q", got)
	}
	if got := sections["beta"]; !strings.Contains(got, "beta body") {
		t.Errorf("beta section = %q", got)
	}
	if len(sections) != 2 {
		t.Errorf("got %d sections, want 2", len(sections))
	}
}
