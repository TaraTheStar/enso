// SPDX-License-Identifier: AGPL-3.0-or-later

// Package status holds the user-configurable status-line template
// engine. Callers populate Context with whatever colour markup their
// renderer expects (ANSI/Lipgloss escape sequences); the template
// engine just substitutes — it never inspects markup.
package status

import (
	"fmt"
	"strings"
	"text/template"
)

// Context is the data passed to the user-supplied status-line template.
// Field names map directly to template variables — `.Provider`,
// `.Model`, etc.
type Context struct {
	Provider       string
	Model          string
	Session        string
	Mode           string
	Activity       string
	Tokens         int
	Window         int
	TokensFmt      string // pre-formatted "12k/32k" segment with colour markup applied at ≥50% / ≥80%
	TokensPerSec   int    // live streaming rate (≈ chars/4 over the open turn); 0 when not streaming
	SidebarVisible bool   // true when the right-side session sidebar is open; default template uses this to skip duplicating the tokens segment
	ConnState      string // pre-styled connection-degraded marker; empty when healthy
}

// DefaultTemplate renders only the data that's transient or repeatedly
// glanced at. Provider, model, session id, cwd, and the token bar live
// permanently in the right sidebar (when present); this template shows
// the tokens segment ONLY when the sidebar is collapsed (so Ctrl-A
// still gives you token visibility) and `t/s` only while a turn is
// actively streaming. If the sidebar is open and nothing is streaming,
// the bar is empty by design — both the duplicating segment and the
// transient one are gated.
//
// The conn-state segment leads since a degraded transport is the most
// urgent thing on the bar — and it's only present when degraded, so it
// doesn't clutter the healthy case. The trailing separator is gated on
// at least one downstream segment being non-empty.
const DefaultTemplate = "{{.ConnState}}" +
	"{{if and .ConnState (or (and (not .SidebarVisible) .TokensFmt) .TokensPerSec)}} · {{end}}" +
	"{{if not .SidebarVisible}}{{.TokensFmt}}{{if .TokensPerSec}} · {{end}}{{end}}" +
	"{{if .TokensPerSec}}{{.TokensPerSec}} t/s{{end}}"

// Compile parses `tpl` (or DefaultTemplate if empty) into a
// text/template. Errors propagate so the host can fall back gracefully.
func Compile(tpl string) (*template.Template, error) {
	if strings.TrimSpace(tpl) == "" {
		tpl = DefaultTemplate
	}
	t, err := template.New("status").Parse(tpl)
	if err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	return t, nil
}

// Render executes `tpl` against `ctx`, falling back to a minimal manual
// render if anything goes wrong (a buggy template shouldn't black-hole
// the status bar).
func Render(tpl *template.Template, ctx Context) string {
	if tpl == nil {
		return fmt.Sprintf("[%s] %s · %s · %s", ctx.Provider, ctx.Model, ctx.Session, ctx.TokensFmt)
	}
	var sb strings.Builder
	if err := tpl.Execute(&sb, ctx); err != nil {
		return fmt.Sprintf("[%s] %s · %s · %s (template err: %v)", ctx.Provider, ctx.Model, ctx.Session, ctx.TokensFmt, err)
	}
	return sb.String()
}
