// SPDX-License-Identifier: AGPL-3.0-or-later

package bubble

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/TaraTheStar/enso/internal/permissions"
)

// egressPending is the bubble-side state for an in-flight interactive
// egress prompt. Like permPending the model holds at most one — the
// host InteractiveBroker blocks on req.Respond — and while it is set
// all input keys route to the y/t/n resolver instead of the buffer.
type egressPending struct {
	req *permissions.EgressPrompt
}

// startEgressPrompt prints the inline egress prompt to scrollback. The
// next y/t/n keystroke (handleKey) resolves it via req.Respond.
func startEgressPrompt(req *permissions.EgressPrompt) tea.Cmd {
	return tea.Println(renderEgressPrompt(req))
}

// egressPendingHint is the status-line analogue of permPendingHint:
// a single pinned reminder shown while an egress prompt is unanswered,
// in case the full Println'd prompt has scrolled out of view.
func egressPendingHint(req *permissions.EgressPrompt) string {
	if req == nil {
		return ""
	}
	target := req.Target
	const maxLen = 50
	// Rune-aware (see permPendingHint): targets are usually ASCII
	// host:port but an IDN punycode'd back into Unicode would otherwise
	// risk a mid-rune cut.
	if r := []rune(target); len(r) > maxLen {
		target = string(r[:maxLen-1]) + "…"
	}
	return noticeStyle.Render("▸ awaiting egress: ") + target +
		noticeStyle.Render("   [y/t/n]")
}

// renderEgressPrompt mirrors renderPermPrompt: a single notice line
// naming the blocked target, the best-effort reason dimmed beneath it,
// then the y/t/n choices. "this task" is the only durable scope (there
// is no per-call egress allowlist file — the static list is config,
// edited up front).
func renderEgressPrompt(req *permissions.EgressPrompt) string {
	var sb strings.Builder
	sb.WriteString(noticeStyle.Render("? ") + "Allow network egress to " + req.Target + "?")
	if req.Reason != "" {
		sb.WriteByte('\n')
		sb.WriteString(statusStyle.Render("  " + req.Reason))
	}
	sb.WriteByte('\n')
	// [y]es is the default — Enter commits it. Same scheme as the
	// perm prompt: bold + cursor on default, dim on the others, no
	// red on [n]o (deny is safe, not destructive).
	choices := []string{
		userStyle.Render("▸ [y]es once"),
		noticeStyle.Render("[t] this task"),
		noticeStyle.Render("[n]o"),
	}
	sb.WriteString(statusStyle.Render("  ") + strings.Join(choices, statusStyle.Render("  ")))
	sb.WriteString(statusStyle.Render("   · enter = yes"))
	return sb.String()
}

// resolveEgress maps a keystroke to an EgressDecision. decided=false
// means the key was not one of the choices and the prompt stays up
// (same UX as resolvePerm — stray keys don't accidentally answer).
func resolveEgress(key string) (decision permissions.EgressDecision, decided bool) {
	switch strings.ToLower(key) {
	case "y", "enter":
		return permissions.EgressAllowOnce, true
	case "t":
		return permissions.EgressAllowTask, true
	case "n", "esc":
		return permissions.EgressDeny, true
	}
	return permissions.EgressDeny, false
}
