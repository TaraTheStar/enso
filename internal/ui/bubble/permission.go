// SPDX-License-Identifier: AGPL-3.0-or-later

package bubble

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/TaraTheStar/enso/internal/backend/host"
	"github.com/TaraTheStar/enso/internal/config"
	"github.com/TaraTheStar/enso/internal/permissions"
)

// permPending is the bubble-side state for an in-flight permission
// prompt. The model holds at most one of these — the agent serialises
// permission requests, blocking on req.Respond until we send a
// Decision. While pending, all input keys are intercepted so the user
// can't accidentally type into the input buffer instead of answering.
type permPending struct {
	req     *permissions.PromptRequest
	checker *permissions.Checker
	cwd     string
	// sess is the seam to the worker's REAL enforcing checker. On the
	// default local path the checker enforcing tool gating lives in the
	// worker process, so "always"/"turn" grants RPC through here
	// (mirroring /yolo); checker above is only the host display mirror.
	// nil in true attach mode (no wire path to the daemon's checker).
	sess *host.Session
}

// startPermPrompt is called from handleBusEvent when an
// EventPermissionRequest arrives. It returns the Cmd that prints the
// inline prompt to scrollback. The caller stores the pending state on
// the model so subsequent keypresses can resolve it.
func startPermPrompt(req *permissions.PromptRequest) tea.Cmd {
	prompt := renderPermPrompt(req)
	return tea.Println(prompt)
}

// permPendingHint renders the single-line status-bar reminder shown
// while a permission prompt is unresolved. The full prompt is in
// scrollback (via tea.Println at request time), but heavy streaming
// can push it off the top of the visible window; this hint stays
// pinned to the status line so the user always sees that a decision
// is owed and which tool is asking.
func permPendingHint(req *permissions.PromptRequest) string {
	if req == nil {
		return ""
	}
	summary := req.ToolName
	switch {
	case req.ArgString != "":
		summary += "(" + req.ArgString + ")"
	case len(req.Args) > 0:
		summary += "(" + summarizeArgs(req.Args) + ")"
	default:
		summary += "()"
	}
	const maxLen = 60
	// Rune-aware: ArgString / summarizeArgs can contain non-ASCII
	// (paths, prose) and byte-slicing at maxLen-1 could cut a rune in
	// half and produce invalid UTF-8 in the status line.
	if r := []rune(summary); len(r) > maxLen {
		summary = string(r[:maxLen-1]) + "…"
	}
	return noticeStyle.Render("▸ awaiting: ") + summary +
		noticeStyle.Render("   [y/n/a/t]")
}

// renderPermPrompt builds the inline prompt block. Diff (when present)
// is shown indented and dim above the question line so the user can
// scan the change before deciding.
func renderPermPrompt(req *permissions.PromptRequest) string {
	var sb strings.Builder
	header := noticeStyle.Render("? ") +
		"Allow " + req.ToolName
	if req.ArgString != "" {
		header += "(" + req.ArgString + ")"
	} else if req.Args != nil && len(req.Args) > 0 {
		header += "(" + summarizeArgs(req.Args) + ")"
	} else {
		header += "()"
	}
	if req.AgentRole != "" {
		header += statusStyle.Render(fmt.Sprintf("  [%s]", req.AgentRole))
	}
	header += "?"
	sb.WriteString(header)

	if req.Diff != "" {
		sb.WriteByte('\n')
		// Indent diff lines so they don't look like top-level prose.
		lines := strings.Split(strings.TrimRight(req.Diff, "\n"), "\n")
		for i, ln := range lines {
			if i > 0 {
				sb.WriteByte('\n')
			}
			sb.WriteString(statusStyle.Render("  " + ln))
		}
	}
	sb.WriteByte('\n')
	// [y]es is the default — Enter commits it. Mark it with a cursor
	// glyph and bold (userStyle); other choices are dim (noticeStyle)
	// so the default is visually unambiguous. Red on [n]o would falsely
	// imply it's the destructive default; deny is the safe outcome so
	// it doesn't warrant a warning colour.
	choices := []string{
		userStyle.Render("▸ [y]es"),
		noticeStyle.Render("[n]o"),
		noticeStyle.Render("[a]lways"),
		noticeStyle.Render("[t]urn"),
	}
	sb.WriteString(statusStyle.Render("  ") + strings.Join(choices, statusStyle.Render("  ")))
	sb.WriteString(statusStyle.Render("   · enter = yes"))
	return sb.String()
}

// summarizeArgs renders a permissions PromptRequest.Args map for
// inline display. Sorted, key=value, summarised values.
func summarizeArgs(args map[string]any) string {
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	// Tiny sort.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j-1] > keys[j]; j-- {
			keys[j-1], keys[j] = keys[j], keys[j-1]
		}
	}
	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%s", k, summarizeArg(args[k])))
	}
	return strings.Join(parts, ", ")
}

// resolvePerm dispatches a key into a permission decision. Returns the
// Decision (or noDecision sentinel) plus a Cmd to print any side-effect
// notice (the "remembered" line). decided=true means the prompt is
// resolved and should be cleared from m.permPending.
func resolvePerm(p *permPending, key string) (decision permissions.Decision, decided bool, cmd tea.Cmd) {
	switch strings.ToLower(key) {
	case "y", "enter":
		return permissions.Allow, true, nil
	case "n", "esc":
		return permissions.Deny, true, nil
	case "a":
		// "Remember" — apply a matching allow pattern to the enforcing
		// checker (worker-side via sess on the default local path; the
		// in-process checker otherwise), persist it to project config,
		// and mirror it onto the host display checker, then allow this
		// call. Only in true attach mode (no sess, no checker) is there
		// no wire path to enforcement — fall back to plain Allow with a
		// notice so the user knows the grant didn't take.
		if p.sess == nil && p.checker == nil {
			cmd = tea.Println(noticeStyle.Render("(allowed once; remember/turn unavailable in attach mode)"))
			return permissions.Allow, true, cmd
		}
		pattern := permissions.DerivePattern(p.req.ToolName, p.req.Args, p.cwd)
		// Enforcement first: apply to the worker's real checker before
		// the decision is sent so the next call in this session is
		// gated by the new rule, not a race.
		if p.sess != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := p.sess.AddAllow(ctx, pattern)
			cancel()
			if err != nil {
				cmd = tea.Println(errorStyle.Render(fmt.Sprintf("remember %s: %v", pattern, err)))
				return permissions.Allow, true, cmd
			}
		}
		if p.checker != nil {
			if err := p.checker.AddAllow(pattern); err != nil {
				cmd = tea.Println(errorStyle.Render(fmt.Sprintf("remember %s: %v", pattern, err)))
				return permissions.Allow, true, cmd
			}
		}
		path := config.ProjectLocalPath(p.cwd)
		if err := config.AppendAllow(path, pattern); err != nil {
			cmd = tea.Println(noticeStyle.Render(fmt.Sprintf("remembered %s in this session, but couldn't persist: %v", pattern, err)))
			return permissions.Allow, true, cmd
		}
		cmd = tea.Println(statusStyle.Render(fmt.Sprintf("→ remembered %s · %s", pattern, path)))
		return permissions.Allow, true, cmd
	case "t":
		// Turn-scoped grant: matches future calls in this turn but
		// doesn't persist. Same enforcement surfaces as "always".
		if p.sess == nil && p.checker == nil {
			cmd = tea.Println(noticeStyle.Render("(allowed once; remember/turn unavailable in attach mode)"))
			return permissions.Allow, true, cmd
		}
		pattern := permissions.DerivePattern(p.req.ToolName, p.req.Args, p.cwd)
		if p.sess != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := p.sess.AddTurnAllow(ctx, pattern)
			cancel()
			if err != nil {
				cmd = tea.Println(errorStyle.Render(fmt.Sprintf("allow-turn %s: %v", pattern, err)))
				return permissions.Allow, true, cmd
			}
		}
		// Mirror onto the host checker only when it is itself the
		// enforcing checker (in-process path, sess == nil). The host
		// display mirror is never reset host-side — only the worker
		// agent loop calls ResetTurnAllows at each user message — so
		// mirroring a transient turn grant onto it would leave stale
		// entries that over-report in /perms.
		if p.sess == nil && p.checker != nil {
			if err := p.checker.AddTurnAllow(pattern); err != nil {
				cmd = tea.Println(errorStyle.Render(fmt.Sprintf("allow-turn %s: %v", pattern, err)))
				return permissions.Allow, true, cmd
			}
		}
		cmd = tea.Println(statusStyle.Render(fmt.Sprintf("→ allowing %s for this turn", pattern)))
		return permissions.Allow, true, cmd
	}
	return permissions.Deny, false, nil
}
