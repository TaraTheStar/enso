// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"fmt"
	"strings"
	"text/template"
)

// statusContext is the data passed to the user-supplied status-line
// template. Field names map directly to template variables — `.Provider`,
// `.Model`, etc.
type statusContext struct {
	Provider       string
	Model          string
	Session        string
	Mode           string
	Activity       string
	Tokens         int
	Window         int
	TokensFmt      string // pre-formatted "12k/32k" segment, with [yellow]/[red] tcell tags applied at ≥50% / ≥80%
	TokensPerSec   int    // live streaming rate (≈ chars/4 over the open turn); 0 when not streaming
	SidebarVisible bool   // true when the right-side session sidebar is open; default template uses this to skip duplicating the tokens segment
}

// defaultStatusLine renders only the data that's transient or
// repeatedly glanced at. Provider, model, session id, cwd, and the
// token bar live permanently in the right sidebar; this template
// shows the tokens segment ONLY when the sidebar is collapsed (so
// Ctrl-A still gives you token visibility) and `t/s` only while a
// turn is actively streaming. If the sidebar is open and nothing is
// streaming, the bar is empty by design — both the duplicating
// segment and the transient one are gated.
const defaultStatusLine = "{{if not .SidebarVisible}}{{.TokensFmt}}{{if .TokensPerSec}} · {{end}}{{end}}{{if .TokensPerSec}}{{.TokensPerSec}} t/s{{end}}"

// compileStatusLine parses `tpl` (or the default if empty) into a
// text/template. Errors propagate so the host can fall back gracefully.
func compileStatusLine(tpl string) (*template.Template, error) {
	if strings.TrimSpace(tpl) == "" {
		tpl = defaultStatusLine
	}
	t, err := template.New("status").Parse(tpl)
	if err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	return t, nil
}

// renderStatusLine executes `tpl` against `ctx`, falling back to a
// minimal manual render if anything goes wrong (a buggy template
// shouldn't black-hole the status bar).
func renderStatusLine(tpl *template.Template, ctx statusContext) string {
	if tpl == nil {
		return fmt.Sprintf("[%s] %s · %s · %s", ctx.Provider, ctx.Model, ctx.Session, ctx.TokensFmt)
	}
	var sb strings.Builder
	if err := tpl.Execute(&sb, ctx); err != nil {
		return fmt.Sprintf("[%s] %s · %s · %s (template err: %v)", ctx.Provider, ctx.Model, ctx.Session, ctx.TokensFmt, err)
	}
	return sb.String()
}
