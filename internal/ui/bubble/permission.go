// SPDX-License-Identifier: AGPL-3.0-or-later

package bubble

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

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
}

// startPermPrompt is called from handleBusEvent when an
// EventPermissionRequest arrives. It returns the Cmd that prints the
// inline prompt to scrollback. The caller stores the pending state on
// the model so subsequent keypresses can resolve it.
func startPermPrompt(req *permissions.PromptRequest) tea.Cmd {
	prompt := renderPermPrompt(req)
	return tea.Println(prompt)
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
	choices := []string{
		userStyle.Render("[y]es"),
		errorStyle.Render("[n]o"),
		userStyle.Render("[a]lways"),
		userStyle.Render("[t]urn"),
	}
	sb.WriteString(statusStyle.Render("  ") + strings.Join(choices, statusStyle.Render("  ")))
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
		// "Remember" — persist a matching allow pattern in the local
		// checker + project config, then allow this call. In attach
		// mode the checker lives in the daemon process and we have no
		// wire path to mutate it; fall back to plain Allow with a
		// notice so the user knows persistence didn't happen.
		if p.checker == nil {
			cmd = tea.Println(noticeStyle.Render("(allowed once; remember/turn unavailable in attach mode)"))
			return permissions.Allow, true, cmd
		}
		pattern := permissions.DerivePattern(p.req.ToolName, p.req.Args)
		if err := p.checker.AddAllow(pattern); err != nil {
			cmd = tea.Println(errorStyle.Render(fmt.Sprintf("remember %s: %v", pattern, err)))
			return permissions.Allow, true, cmd
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
		// doesn't persist. Same attach-mode caveat.
		if p.checker == nil {
			cmd = tea.Println(noticeStyle.Render("(allowed once; remember/turn unavailable in attach mode)"))
			return permissions.Allow, true, cmd
		}
		pattern := permissions.DerivePattern(p.req.ToolName, p.req.Args)
		if err := p.checker.AddTurnAllow(pattern); err != nil {
			cmd = tea.Println(errorStyle.Render(fmt.Sprintf("allow-turn %s: %v", pattern, err)))
			return permissions.Allow, true, cmd
		}
		cmd = tea.Println(statusStyle.Render(fmt.Sprintf("→ allowing %s for this turn", pattern)))
		return permissions.Allow, true, cmd
	}
	return permissions.Deny, false, nil
}
