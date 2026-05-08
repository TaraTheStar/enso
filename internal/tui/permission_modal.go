// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"fmt"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/TaraTheStar/enso/internal/permissions"
)

// ShowPermissionModal overlays a permission-prompt modal on the given pages
// container and sends the user's decision on the request's Respond channel.
//
// `focusAfter` is the primitive that should regain focus once the modal is
// dismissed — typically the input TextArea. Without this restore, tview's
// Pages leaves focus on nothing after RemovePage and the user can't type.
//
// `onRemember`, when non-nil, is invoked when the user picks "Remember"
// (button or `r` shortcut) before the Allow decision is sent. The caller
// supplies it to add the rule to the live checker and persist it.
//
// `onAllowTurn`, when non-nil, is invoked when the user picks "Turn"
// (button or `t` shortcut). It should add a turn-scoped grant to the
// live checker — typically `Checker.AddTurnAllow(DerivePattern(...))`.
// nil hides the button (e.g., daemon-attach mode where the TUI has no
// in-process Checker to mutate).
//
// Visual: bordered box (yellow border so it's clearly a prompt), titled with
// the tool name. tview's default white-bg/black-text buttons are readable on
// every terminal we've tested; the active button gets a loud yellow-on-black
// override so it can't be missed. Single-key shortcuts: a/y allow, r remember,
// t turn-allow, d/n deny, Esc deny.
func ShowPermissionModal(app *tview.Application, pages *tview.Pages, focusAfter tview.Primitive, onRemember func(), onAllowTurn func(), req *permissions.PromptRequest) {
	argStr := formatArgs(req.Args)

	body := tview.NewTextView()
	body.SetDynamicColors(true)
	body.SetScrollable(true)
	body.SetWordWrap(true)
	body.SetBorderPadding(0, 0, 1, 1) // 1-col side padding inside the border

	header := fmt.Sprintf("[lavender::b]%s[-:-:-]\n\n%s", req.ToolName, argStr)
	if prefix := subagentPrefix(req); prefix != "" {
		header = prefix + " " + header
	}
	if req.Diff != "" {
		body.SetText(header + "\n" + RenderDiff(req.Diff))
	} else {
		body.SetText(header)
	}

	const (
		labelAllow     = "  (a)llow  "
		labelRemember  = "  (r)emember  "
		labelAllowTurn = "  (t)urn  "
		labelDeny      = "  (d)eny  "
	)

	form := tview.NewForm()
	form.SetButtonsAlign(tview.AlignCenter)
	// Visible-but-quiet inactive style; loud active style. White-on-black
	// reads well against most dark palettes; the active button gets a
	// mauve background for an unmistakable focus pop without warring with
	// dust/yellow elsewhere in the UI.
	form.SetButtonBackgroundColor(tcell.GetColor("white"))
	form.SetButtonTextColor(tcell.GetColor("black"))
	form.SetButtonActivatedStyle(
		tcell.StyleDefault.
			Background(tcell.GetColor("mauve")).
			Foreground(tcell.GetColor("black")).
			Bold(true),
	)

	resolved := false
	resolve := func(d permissions.Decision) {
		if resolved {
			return
		}
		resolved = true
		pages.RemovePage("perm")
		if focusAfter != nil {
			app.SetFocus(focusAfter)
		}
		select {
		case req.Respond <- d:
		default:
		}
	}

	remember := func() {
		if onRemember != nil {
			onRemember()
		}
		resolve(permissions.Allow)
	}

	allowTurn := func() {
		if onAllowTurn != nil {
			onAllowTurn()
		}
		resolve(permissions.Allow)
	}

	form.AddButton(labelAllow, func() { resolve(permissions.Allow) })
	form.AddButton(labelRemember, remember)
	if onAllowTurn != nil {
		form.AddButton(labelAllowTurn, allowTurn)
	}
	form.AddButton(labelDeny, func() { resolve(permissions.Deny) })

	// countdown is rendered between the body and the form when the
	// request carries a Deadline (attach mode — the daemon enforces a
	// hard timeout). In standalone mode the deadline is zero and we
	// hide the row so the modal is unchanged.
	countdown := tview.NewTextView()
	countdown.SetDynamicColors(true)
	countdown.SetTextAlign(tview.AlignCenter)

	frame := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(body, 0, 1, false)
	if !req.Deadline.IsZero() {
		frame.AddItem(countdown, 1, 0, false)
	}
	frame.AddItem(form, 3, 0, true)
	frame.SetBorder(true).
		SetBorderColor(tcell.GetColor("lavender")).
		SetTitle(" permission ").
		SetTitleColor(tcell.GetColor("lavender")).
		SetTitleAlign(tview.AlignLeft)

	// Explicit button-focus walking. tview's Form does internal navigation
	// for forms with input fields, but a buttons-only form doesn't always
	// pick up Left/Right/Tab — so we do it ourselves. focusedBtn is closed
	// over by the input handlers; SetFocus(idx) targets the n-th button
	// (form items count first, of which we have none). numBtns is dynamic
	// because the "Turn" button only appears when onAllowTurn is wired
	// (standalone mode); attach mode still has 3 buttons.
	numBtns := 3
	if onAllowTurn != nil {
		numBtns = 4
	}
	focusedBtn := 0
	advance := func(delta int) {
		focusedBtn = (focusedBtn + delta + numBtns) % numBtns
		form.SetFocus(focusedBtn)
		app.SetFocus(form)
	}

	frame.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Key() {
		case tcell.KeyEscape:
			resolve(permissions.Deny)
			return nil
		case tcell.KeyTab, tcell.KeyRight:
			advance(1)
			return nil
		case tcell.KeyBacktab, tcell.KeyLeft:
			advance(-1)
			return nil
		case tcell.KeyRune:
			switch event.Rune() {
			case 'a', 'A', 'y', 'Y':
				resolve(permissions.Allow)
				return nil
			case 'd', 'D', 'n', 'N':
				resolve(permissions.Deny)
				return nil
			case 'r', 'R':
				remember()
				return nil
			case 't', 'T':
				if onAllowTurn != nil {
					allowTurn()
					return nil
				}
			}
		}
		return event
	})

	overlay := centered(frame, 80, 14)
	pages.AddPage("perm", overlay, true, true)
	app.SetFocus(form)

	// Countdown loop. Updates the footer line each second; on
	// expiry, dismisses the modal as Deny so the daemon's auto-deny
	// and the visible UI agree. Goroutine exits when resolve() flips
	// `resolved` (button press, Esc, or expiry).
	if !req.Deadline.IsZero() {
		countdown.SetText(fmtCountdown(time.Until(req.Deadline)))
		go func() {
			t := time.NewTicker(time.Second)
			defer t.Stop()
			for {
				<-t.C
				if resolved {
					return
				}
				remaining := time.Until(req.Deadline)
				if remaining <= 0 {
					app.QueueUpdateDraw(func() { resolve(permissions.Deny) })
					return
				}
				app.QueueUpdateDraw(func() {
					countdown.SetText(fmtCountdown(remaining))
				})
			}
		}()
	}
}

// fmtCountdown returns the styled "auto-deny in Ns" footer string.
// Color escalates so the user gets a clear urgency signal: dim while
// they have time, yellow under 30s, red under 10s. Sub-second
// remaining values floor to "1s" rather than "0s" so the user sees
// an honest last-tick value before dismissal.
func fmtCountdown(remaining time.Duration) string {
	secs := int(remaining.Seconds())
	if remaining > 0 && secs < 1 {
		secs = 1
	}
	switch {
	case secs <= 10:
		return fmt.Sprintf("[red]auto-deny in %ds[-]", secs)
	case secs <= 30:
		return fmt.Sprintf("[yellow]auto-deny in %ds[-]", secs)
	default:
		return fmt.Sprintf("[gray]auto-deny in %ds[-]", secs)
	}
}

// centered returns a Flex that centers the given primitive at the given
// width/height inside the available area.
func centered(p tview.Primitive, width, height int) tview.Primitive {
	return tview.NewFlex().
		AddItem(nil, 0, 1, false).
		AddItem(tview.NewFlex().SetDirection(tview.FlexRow).
			AddItem(nil, 0, 1, false).
			AddItem(p, height, 1, true).
			AddItem(nil, 0, 1, false), width, 1, true).
		AddItem(nil, 0, 1, false)
}

func formatArgs(args map[string]interface{}) string {
	if len(args) == 0 {
		return "[gray](no args)[-]"
	}
	out := ""
	for k, v := range args {
		s := fmt.Sprintf("%v", v)
		if len(s) > 200 {
			s = s[:200] + "…"
		}
		out += fmt.Sprintf("[gray]%s[-] = %s\n", k, s)
	}
	return out
}

// subagentPrefix returns "[cyan]<role> <short-id>[-]" for sub-agent
// prompts and "" for the top-level agent. Format prefers the role
// label when present (workflows pass the role name; spawn_agent's
// `role` arg overrides), falling back to "subagent" otherwise. Short
// id is the first 6 chars of the agent uuid — enough to disambiguate
// in a tree of <100 in-flight children.
func subagentPrefix(req *permissions.PromptRequest) string {
	if req.AgentID == "" {
		return ""
	}
	short := req.AgentID
	if len(short) > 6 {
		short = short[:6]
	}
	label := req.AgentRole
	if label == "" {
		label = "subagent"
	}
	return fmt.Sprintf("[cyan::b]%s %s[-:-:-]", label, short)
}
