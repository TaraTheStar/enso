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

// resolvePerm dispatches a key into a permission decision. decided=true
// means the prompt is resolved and should be cleared from m.permPending;
// the returned Cmd both sends the Decision on req.Respond and prints any
// side-effect notice.
//
// CRITICAL: the Cmd does all the slow work — the up-to-5s
// AddAllow/AddTurnAllow worker RPC and the config-file write — so it
// MUST run off the Bubble Tea event loop (Cmds do, by design). Calling
// any of this inline in Update would freeze the UI for the RPC's
// duration. Enforcement is applied BEFORE the Decision is sent so the
// next call in this session is gated by the new rule, not a race.
func resolvePerm(p *permPending, key string) (decided bool, cmd tea.Cmd) {
	switch strings.ToLower(key) {
	case "y", "enter":
		return true, decisionCmd(p.req, permissions.Allow, nil)
	case "n", "esc":
		return true, decisionCmd(p.req, permissions.Deny, nil)
	case "a":
		return true, rememberCmd(p, false)
	case "t":
		return true, rememberCmd(p, true)
	}
	return false, nil
}

// sendDecision unblocks the agent goroutine parked on req.Respond. Safe
// when Respond is nil (unit tests construct bare PromptRequests).
func sendDecision(req *permissions.PromptRequest, d permissions.Decision) {
	if req != nil && req.Respond != nil {
		req.Respond <- d
	}
}

// decisionCmd returns a Cmd that sends d on req.Respond and then runs
// the optional follow-up notice Cmd. Used for the plain y/n path.
func decisionCmd(req *permissions.PromptRequest, d permissions.Decision, notice tea.Cmd) tea.Cmd {
	return func() tea.Msg {
		sendDecision(req, d)
		if notice != nil {
			return notice()
		}
		return nil
	}
}

// rememberCmd builds the Cmd for the "always" (turn=false) and "turn"
// (turn=true) grants. It applies the grant to the enforcing checker —
// worker-side via sess on the default local path, the in-process checker
// otherwise — sends the Allow decision once enforcement is in place, and
// (for "always") persists the pattern to project config. Every blocking
// step runs here, inside the Cmd, never on the event loop.
func rememberCmd(p *permPending, turn bool) tea.Cmd {
	return func() tea.Msg {
		// True attach mode (no sess, no checker): no wire path to
		// enforcement — fall back to plain Allow with a notice so the
		// user knows the grant didn't take.
		if p.sess == nil && p.checker == nil {
			sendDecision(p.req, permissions.Allow)
			return tea.Println(noticeStyle.Render("(allowed once; remember/turn unavailable in attach mode)"))()
		}
		pattern := permissions.DerivePattern(p.req.ToolName, p.req.Args, p.cwd)
		verb := "remember"
		if turn {
			verb = "allow-turn"
		}

		// Enforcement first: apply to the worker's real checker before
		// the decision is sent.
		if p.sess != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			var err error
			if turn {
				err = p.sess.AddTurnAllow(ctx, pattern)
			} else {
				err = p.sess.AddAllow(ctx, pattern)
			}
			cancel()
			if err != nil {
				sendDecision(p.req, permissions.Allow)
				return tea.Println(errorStyle.Render(fmt.Sprintf("%s %s: %v", verb, pattern, err)))()
			}
		}

		if turn {
			// Mirror onto the host checker only when it is itself the
			// enforcing checker (in-process path, sess == nil). The host
			// display mirror is never reset host-side — only the worker
			// agent loop calls ResetTurnAllows at each user message — so
			// mirroring a transient turn grant onto it would leave stale
			// entries that over-report in /perms.
			if p.sess == nil && p.checker != nil {
				if err := p.checker.AddTurnAllow(pattern); err != nil {
					sendDecision(p.req, permissions.Allow)
					return tea.Println(errorStyle.Render(fmt.Sprintf("%s %s: %v", verb, pattern, err)))()
				}
			}
		} else if p.checker != nil {
			if err := p.checker.AddAllow(pattern); err != nil {
				sendDecision(p.req, permissions.Allow)
				return tea.Println(errorStyle.Render(fmt.Sprintf("%s %s: %v", verb, pattern, err)))()
			}
		}

		// Enforcement is in place — release the agent.
		sendDecision(p.req, permissions.Allow)

		if turn {
			return tea.Println(statusStyle.Render(fmt.Sprintf("→ allowing %s for this turn", pattern)))()
		}
		path := config.ProjectLocalPath(p.cwd)
		if err := config.AppendAllow(path, pattern); err != nil {
			return tea.Println(noticeStyle.Render(fmt.Sprintf("remembered %s in this session, but couldn't persist: %v", pattern, err)))()
		}
		return tea.Println(statusStyle.Render(fmt.Sprintf("→ remembered %s · %s", pattern, path)))()
	}
}
