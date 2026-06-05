// SPDX-License-Identifier: AGPL-3.0-or-later

package workflow

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
	"text/template"

	"github.com/TaraTheStar/enso/internal/bus"
)

// funcMap is the shared template helper set for both role bodies and edge
// predicates, so `contains`/`matches` work in either. The stdlib builtins
// (eq/ne/and/or/not) are always present — do not re-add them here.
func funcMap() template.FuncMap {
	return template.FuncMap{
		// contains is a case-insensitive substring test; haystack first,
		// needle second: {{ contains .review.output "lgtm" }}.
		"contains": func(haystack, needle string) bool {
			return strings.Contains(strings.ToLower(haystack), strings.ToLower(needle))
		},
		// matches is a regexp test; subject first, pattern second:
		// {{ matches .review.output "LG.M" }}. A bad pattern errors at EVAL
		// time (the literal is only compiled when matches runs), where
		// evalCond demotes it to falsey+warn rather than aborting the run.
		"matches": func(subject, pattern string) (bool, error) {
			re, err := regexp.Compile(pattern)
			if err != nil {
				return false, err
			}
			return re.MatchString(subject), nil
		},
	}
}

// newTemplate builds a template wired with funcMap. Used for role bodies
// (so they get contains/matches too) and edge predicates.
func newTemplate(name, body string) (*template.Template, error) {
	return template.New(name).Funcs(funcMap()).Parse(body)
}

// Predicate is a compiled edge guard. Compilation happens at parse time so
// template-syntax and unknown-func errors surface in `/workflow validate`
// rather than mid-run.
type Predicate struct {
	Src  string             // original source incl. quotes — for validate + errors
	tmpl *template.Template // compiled via newTemplate
}

// newPredicate compiles a predicate body. src is the original quoted form,
// kept verbatim for diagnostics.
func newPredicate(src, body string) (*Predicate, error) {
	t, err := newTemplate("pred", body)
	if err != nil {
		return nil, fmt.Errorf("predicate %s: %w", src, err)
	}
	return &Predicate{Src: src, tmpl: t}, nil
}

// Eval renders the predicate against the already-built data map and returns
// its truthiness. The caller owns snapshotting so concurrent role writes are
// serialised before this is reached.
func (p *Predicate) Eval(data map[string]any) (bool, error) {
	var buf bytes.Buffer
	if err := p.tmpl.Execute(&buf, data); err != nil {
		return false, fmt.Errorf("predicate %s: %w", p.Src, err)
	}
	return truthy(buf.String()), nil
}

// truthy reports whether a rendered predicate string counts as true. It is
// falsey iff the trimmed, lowercased render is one of the listed tokens.
// "<no value>" is included so a bare `{{ .role.missingfield }}` predicate is
// falsey (lenient) rather than truthy on the literal placeholder.
func truthy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "false", "0", "no", "<no value>":
		return false
	}
	return true
}

// evalCond resolves an edge guard. A nil predicate (unconditional edge) is
// always true. A runtime Eval error — in practice only a bad literal regexp
// in matches, which errors at eval not parse — is demoted to falsey with a
// neutral bus notice, so the gate simply doesn't fire instead of aborting.
func evalCond(p *Predicate, data map[string]any, b *bus.Bus) bool {
	if p == nil {
		return true
	}
	ok, err := p.Eval(data)
	if err != nil {
		if b != nil {
			b.Publish(bus.Event{
				Type:    bus.EventNotice,
				Payload: fmt.Sprintf("workflow: %v — treating as not satisfied", err),
			})
		}
		return false
	}
	return ok
}
