// SPDX-License-Identifier: AGPL-3.0-or-later

package workflow

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"
	"unicode"

	"github.com/adrg/frontmatter"

	"github.com/TaraTheStar/enso/internal/paths"
)

// Workflow is a parsed declarative agent workflow.
type Workflow struct {
	Name      string
	Roles     map[string]Role
	Edges     []Edge
	RoleOrder []string // topological order; populated by topoSort()
}

// Role describes one agent in the workflow.
type Role struct {
	Name           string
	Model          string   // optional override; "" = inherit parent
	AllowedTools   []string // optional restriction
	PromptTemplate *template.Template
}

// Edge is a directed dependency: From's output is fed to To's prompt context.
// Cond is an optional `if` guard; when non-nil the edge only "fires" (counts
// toward To's readiness) if the predicate evaluates truthy at runtime. A nil
// Cond is an unconditional edge.
type Edge struct {
	From string
	To   string
	Cond *Predicate
}

// frontmatter shape

type fmRoles map[string]struct {
	Model string   `yaml:"model"`
	Tools []string `yaml:"tools"`
}

type fmDoc struct {
	Roles fmRoles  `yaml:"roles"`
	Edges []string `yaml:"edges"`
}

// LoadFile reads and parses a workflow from disk. Returns (*Workflow, nil)
// on success. The workflow is pre-validated and topo-sorted so the runner can
// iterate `wf.RoleOrder` without further work.
func LoadFile(path string) (*Workflow, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read workflow: %w", err)
	}
	return Parse(filepath.Base(path), data)
}

// LoadByName resolves a workflow by name. Search order:
//  1. ./.enso/workflows/<name>.md (project)
//  2. $XDG_CONFIG_HOME/enso/workflows/<name>.md (user)
func LoadByName(cwd, name string) (*Workflow, error) {
	candidates := []string{}
	if cwd != "" {
		candidates = append(candidates, filepath.Join(cwd, ".enso", "workflows", name+".md"))
	}
	if dir, err := paths.ConfigDir(); err == nil {
		candidates = append(candidates, filepath.Join(dir, "workflows", name+".md"))
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return LoadFile(p)
		}
	}
	return nil, fmt.Errorf("workflow %q not found in %v", name, candidates)
}

// Parse parses an in-memory workflow definition.
func Parse(displayName string, data []byte) (*Workflow, error) {
	var doc fmDoc
	body, err := frontmatter.Parse(bytes.NewReader(data), &doc)
	if err != nil {
		return nil, fmt.Errorf("frontmatter: %w", err)
	}
	if len(doc.Roles) == 0 {
		return nil, fmt.Errorf("workflow %s: no roles defined", displayName)
	}

	sections, err := splitSections(string(body))
	if err != nil {
		return nil, fmt.Errorf("workflow %s: %w", displayName, err)
	}

	wf := &Workflow{
		Name:  strings.TrimSuffix(displayName, ".md"),
		Roles: map[string]Role{},
	}
	for name, meta := range doc.Roles {
		section, ok := sections[name]
		if !ok {
			return nil, fmt.Errorf("workflow %s: role %q has no `## %s` section in body", displayName, name, name)
		}
		tmpl, err := newTemplate(name, section)
		if err != nil {
			return nil, fmt.Errorf("workflow %s: role %q template: %w", displayName, name, err)
		}
		wf.Roles[name] = Role{
			Name:           name,
			Model:          meta.Model,
			AllowedTools:   meta.Tools,
			PromptTemplate: tmpl,
		}
	}

	for _, e := range doc.Edges {
		edge, err := parseEdge(e)
		if err != nil {
			return nil, fmt.Errorf("workflow %s: edge %q: %w", displayName, e, err)
		}
		if _, ok := wf.Roles[edge.From]; !ok {
			return nil, fmt.Errorf("workflow %s: edge %q references unknown role %q", displayName, e, edge.From)
		}
		if _, ok := wf.Roles[edge.To]; !ok {
			return nil, fmt.Errorf("workflow %s: edge %q references unknown role %q", displayName, e, edge.To)
		}
		wf.Edges = append(wf.Edges, edge)
	}

	order, err := topoSort(wf.Roles, wf.Edges)
	if err != nil {
		return nil, fmt.Errorf("workflow %s: %w", displayName, err)
	}
	wf.RoleOrder = order
	return wf, nil
}

var sectionHeader = regexp.MustCompile(`(?m)^##\s+(\w+)\s*$`)

// splitSections returns map[role-name]body for every `## <name>` section.
func splitSections(body string) (map[string]string, error) {
	matches := sectionHeader.FindAllStringIndex(body, -1)
	if len(matches) == 0 {
		return nil, fmt.Errorf("no `## <role>` sections found")
	}
	out := map[string]string{}
	for i, m := range matches {
		nameMatch := sectionHeader.FindStringSubmatch(body[m[0]:m[1]])
		name := nameMatch[1]
		start := m[1]
		end := len(body)
		if i+1 < len(matches) {
			end = matches[i+1][0]
		}
		out[name] = strings.TrimSpace(body[start:end])
	}
	return out, nil
}

// parseEdge parses a single edge spec: `from -> to [if '<predicate>']`.
// The guard parser is structured so a future `loop` arm can slot in beside
// `if`; for now `loop` returns a clear "not supported" error.
func parseEdge(s string) (Edge, error) {
	parts := strings.Split(s, "->")
	if len(parts) != 2 { // preserves the existing "exactly one arrow" rule
		return Edge{}, fmt.Errorf("expected `from -> to [if <pred>]`")
	}
	from, fromRest := splitFirstWord(parts[0])
	if from == "" {
		return Edge{}, fmt.Errorf("missing edge source")
	}
	if fromRest != "" {
		return Edge{}, fmt.Errorf("edge source must be a single role name, got %q", strings.TrimSpace(parts[0]))
	}
	to, rest := splitFirstWord(parts[1])
	if to == "" {
		return Edge{}, fmt.Errorf("missing edge target")
	}
	cond, err := parseGuard(rest)
	if err != nil {
		return Edge{}, err
	}
	return Edge{From: from, To: to, Cond: cond}, nil
}

// parseGuard parses the optional trailing guard after the edge target. An
// empty remainder means an unconditional edge (nil predicate).
func parseGuard(rest string) (*Predicate, error) {
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return nil, nil
	}
	kw, after := splitFirstWord(rest)
	switch kw {
	case "if":
		pred, leftover, err := extractPredicate(after)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(leftover) != "" {
			return nil, fmt.Errorf("unexpected text after if-predicate: %q", leftover)
		}
		return pred, nil
	case "loop":
		return nil, fmt.Errorf("`loop` edges (bounded loops) are not supported yet")
	default:
		return nil, fmt.Errorf("expected `if` guard, got %q", kw)
	}
}

// extractPredicate reads a single-quoted predicate at the start of s and
// compiles it. LIMITATION: the predicate body must not contain a literal
// single quote — inside a Go template use double quotes or backticks.
func extractPredicate(s string) (*Predicate, string, error) {
	s = strings.TrimSpace(s)
	if s == "" || s[0] != '\'' {
		return nil, "", fmt.Errorf("expected single-quoted predicate, got %q", s)
	}
	j := strings.IndexByte(s[1:], '\'')
	if j < 0 {
		return nil, "", fmt.Errorf("unterminated predicate (missing closing quote): %q", s)
	}
	body := s[1 : 1+j]
	rest := s[1+j+1:]
	p, err := newPredicate("'"+body+"'", body) // compiles now; errors surface in /workflow validate
	if err != nil {
		return nil, "", err
	}
	return p, rest, nil
}

// splitFirstWord returns the first whitespace-delimited token and the trimmed
// remainder.
func splitFirstWord(s string) (word, rest string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", ""
	}
	i := strings.IndexFunc(s, unicode.IsSpace)
	if i < 0 {
		return s, ""
	}
	return s[:i], strings.TrimSpace(s[i:])
}

// topoSort returns roles in dependency order. Roles with no incoming edge
// come first; cycles are reported as an error.
func topoSort(roles map[string]Role, edges []Edge) ([]string, error) {
	indeg := map[string]int{}
	out := map[string][]string{}
	for name := range roles {
		indeg[name] = 0
	}
	for _, e := range edges {
		out[e.From] = append(out[e.From], e.To)
		indeg[e.To]++
	}

	var queue []string
	for name, d := range indeg {
		if d == 0 {
			queue = append(queue, name)
		}
	}
	// Stable order: sort the initial queue.
	sortStrings(queue)

	var ordered []string
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		ordered = append(ordered, n)
		nexts := out[n]
		sortStrings(nexts)
		for _, m := range nexts {
			indeg[m]--
			if indeg[m] == 0 {
				queue = append(queue, m)
			}
		}
	}
	if len(ordered) != len(roles) {
		return nil, fmt.Errorf("cycle detected (or unreachable role)")
	}
	return ordered, nil
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
