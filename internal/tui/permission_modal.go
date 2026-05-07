// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"fmt"

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
// Visual: bordered box (yellow border so it's clearly a prompt), titled with
// the tool name. tview's default white-bg/black-text buttons are readable on
// every terminal we've tested; the active button gets a loud yellow-on-black
// override so it can't be missed. Single-key shortcuts: a/y allow, r remember,
// d/n deny, Esc deny.
func ShowPermissionModal(app *tview.Application, pages *tview.Pages, focusAfter tview.Primitive, onRemember func(), req *permissions.PromptRequest) {
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
		labelAllow    = "  (a)llow  "
		labelRemember = "  (r)emember  "
		labelDeny     = "  (d)eny  "
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

	form.AddButton(labelAllow, func() { resolve(permissions.Allow) })
	form.AddButton(labelRemember, remember)
	form.AddButton(labelDeny, func() { resolve(permissions.Deny) })

	frame := tview.NewFlex().
		SetDirection(tview.FlexRow).
		AddItem(body, 0, 1, false).
		AddItem(form, 3, 0, true)
	frame.SetBorder(true).
		SetBorderColor(tcell.GetColor("lavender")).
		SetTitle(" permission ").
		SetTitleColor(tcell.GetColor("lavender")).
		SetTitleAlign(tview.AlignLeft)

	// Explicit button-focus walking. tview's Form does internal navigation
	// for forms with input fields, but a buttons-only form doesn't always
	// pick up Left/Right/Tab — so we do it ourselves. focusedBtn is closed
	// over by the input handlers; SetFocus(idx) targets the n-th button
	// (form items count first, of which we have none).
	const numBtns = 3
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
			}
		}
		return event
	})

	overlay := centered(frame, 80, 14)
	pages.AddPage("perm", overlay, true, true)
	app.SetFocus(form)
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
