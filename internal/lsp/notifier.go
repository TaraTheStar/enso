// SPDX-License-Identifier: AGPL-3.0-or-later

package lsp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// NotifierOptions tunes the post-write diagnostics path. Zero values
// pick sensible defaults — Wait defaults to 500ms (typical incremental
// reanalysis on gopls / tsserver / pyright lands inside that), Dedup
// defaults to 100ms (collects any follow-up publication from servers
// that emit an interim empty batch first), MaxLines defaults to 10
// (long lists are noise; the model should fix the first few and re-edit).
type NotifierOptions struct {
	Wait        time.Duration
	Dedup       time.Duration
	MinSeverity int
	MaxLines    int
}

func (o NotifierOptions) wait() time.Duration {
	if o.Wait > 0 {
		return o.Wait
	}
	return 500 * time.Millisecond
}

func (o NotifierOptions) dedup() time.Duration {
	if o.Dedup > 0 {
		return o.Dedup
	}
	return 100 * time.Millisecond
}

func (o NotifierOptions) minSeverity() int {
	if o.MinSeverity > 0 {
		return o.MinSeverity
	}
	return SeverityError
}

func (o NotifierOptions) maxLines() int {
	if o.MaxLines > 0 {
		return o.MaxLines
	}
	return 10
}

// Notifier wraps a *Manager with the post-write LSP push the write/
// edit tools call into. Construct with NewNotifier and stash on
// tools.AgentContext.LSPNotifier; nil there disables the path.
type Notifier struct {
	Manager *Manager
	Cwd     string // for path rendering ("foo/bar.go" not "/abs/foo/bar.go")
	Opts    NotifierOptions
}

// NewNotifier returns a Notifier that funnels writes through mgr.
// cwd is used only for rendering relative paths in diagnostics.
func NewNotifier(mgr *Manager, cwd string, opts NotifierOptions) *Notifier {
	return &Notifier{Manager: mgr, Cwd: cwd, Opts: opts}
}

// NotifyWrite implements tools.LSPNotifier. Ensures the right language
// server is up for absPath, refreshes its view of the file with the
// just-written on-disk contents, waits for the next publishDiagnostics,
// and renders an LLM-friendly block. Returns "" on any non-fatal
// problem (no LSP for this extension, server crashed, no diagnostics
// at or above MinSeverity).
//
// The bounded wait runs against either ctx or the configured Wait
// budget, whichever fires first — caller-supplied ctx can shorten but
// not extend the budget.
func (n *Notifier) NotifyWrite(ctx context.Context, absPath string) string {
	if n == nil || n.Manager == nil {
		return ""
	}

	// Resolve and start the server. The manager's ClientFor returns
	// (nil, "", nil) when no LSP is configured for this extension —
	// not an error, just nothing to do.
	client, _, err := n.Manager.ClientFor(ctx, absPath)
	if err != nil || client == nil {
		return ""
	}
	uri := pathToURI(absPath)
	languageID := n.Manager.LanguageID(absPath)

	// Refresh the server's view of the file. Re-sending DidOpen with
	// the new content makes most servers reset their buffer; DidChange
	// (future) replaces this with the proper incremental flow.
	text, readErr := os.ReadFile(absPath)
	if readErr != nil {
		return ""
	}
	if err := refreshBuffer(client, uri, languageID, string(text)); err != nil {
		return ""
	}

	// Bounded wait for the next publication.
	waitCtx, cancel := context.WithTimeout(ctx, n.Opts.wait())
	defer cancel()
	diags := client.WaitForDiagnostics(waitCtx, uri, n.Opts.dedup())

	// Filter + render.
	filtered := filterBySeverity(diags, n.Opts.minSeverity())
	if len(filtered) == 0 {
		return ""
	}
	return renderDiagnostics(n.relPath(absPath), filtered, n.Opts.maxLines())
}

func (n *Notifier) relPath(abs string) string {
	if n.Cwd == "" {
		return abs
	}
	if rel, err := filepath.Rel(n.Cwd, abs); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return abs
}

// filterBySeverity drops entries below threshold (LSP uses smaller-
// number-is-higher-severity: 1=Error, 2=Warning, 3=Info, 4=Hint).
// Entries with Severity == 0 (server omitted the field) are treated
// as Error — safer to over-surface than under.
func filterBySeverity(diags []Diagnostic, min int) []Diagnostic {
	if len(diags) == 0 {
		return nil
	}
	out := make([]Diagnostic, 0, len(diags))
	for _, d := range diags {
		sev := d.Severity
		if sev == 0 {
			sev = SeverityError
		}
		if sev <= min {
			out = append(out, d)
		}
	}
	return out
}

// renderDiagnostics formats a slice of Diagnostic into the block the
// model sees. Sorted by line then character so the output is stable
// across reruns and matches how a human would scan the file.
func renderDiagnostics(displayPath string, diags []Diagnostic, maxLines int) string {
	sort.SliceStable(diags, func(i, j int) bool {
		if diags[i].Range.Start.Line != diags[j].Range.Start.Line {
			return diags[i].Range.Start.Line < diags[j].Range.Start.Line
		}
		return diags[i].Range.Start.Character < diags[j].Range.Start.Character
	})

	var b strings.Builder
	fmt.Fprintf(&b, "\n\n[LSP diagnostics for %s]\n", displayPath)

	n := len(diags)
	shown := min(n, maxLines)
	for i := range shown {
		d := diags[i]
		// LSP positions are zero-indexed; 1-index for the human (and
		// the model — every editor reports 1-indexed line numbers).
		line := d.Range.Start.Line + 1
		col := d.Range.Start.Character + 1
		fmt.Fprintf(&b, "%s:%d:%d: %s: %s\n",
			displayPath, line, col, severityWord(d.Severity), strings.TrimSpace(d.Message))
	}
	if n > shown {
		fmt.Fprintf(&b, "(%d more not shown)\n", n-shown)
	}
	return strings.TrimRight(b.String(), "\n")
}

func severityWord(s int) string {
	switch s {
	case SeverityError, 0:
		return "error"
	case SeverityWarning:
		return "warning"
	case SeverityInformation:
		return "info"
	case SeverityHint:
		return "hint"
	}
	return "diagnostic"
}

// refreshBuffer informs the server about the just-saved file. If the
// document is already open, fire didChange with the new whole-document
// text followed by didSave — both reanalysis triggers most servers
// honour (didChange for buffer state, didSave for save-on-change
// linters). If the document is new to the server, didOpen carries the
// full text and the save is implicit in the open's version=1 state.
func refreshBuffer(client *Client, uri, languageID, text string) error {
	if client == nil {
		return nil
	}
	if client.IsOpen(uri) {
		if err := client.DidChange(uri, text); err != nil {
			return err
		}
		return client.DidSave(uri)
	}
	return client.DidOpen(uri, languageID, text)
}
