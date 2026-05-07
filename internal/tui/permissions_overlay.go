// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"fmt"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/TaraTheStar/enso/internal/config"
	"github.com/TaraTheStar/enso/internal/permissions"
)

// ShowPermissionsOverlay opens a modal listing the allow/ask/deny rules
// stored in <cwd>/.enso/config.local.toml — the same file that
// "Allow + Remember" writes to. Pressing Delete (or `d`) on a row
// removes the rule from the file *and* the live checker. Esc closes.
//
// Rules from system, user, project (non-local), or `-c` overrides are
// not surfaced here: deleting one would have cross-project effects we
// can't undo locally. Users edit those files by hand.
func ShowPermissionsOverlay(
	app *tview.Application,
	pages *tview.Pages,
	focusAfter tview.Primitive,
	chatView *tview.TextView,
	cwd string,
	checker *permissions.Checker,
) {
	const pageName = "permissions"
	path := config.ProjectLocalPath(cwd)

	list := tview.NewList()
	list.ShowSecondaryText(true)
	list.SetMainTextColor(tcell.GetColor("white"))
	list.SetSecondaryTextColor(tcell.GetColor("teal"))
	list.SetSelectedBackgroundColor(tcell.GetColor("mauve"))
	list.SetSelectedTextColor(tcell.GetColor("black"))
	list.SetHighlightFullLine(true)

	frame := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(list, 0, 1, true)
	frame.SetBorder(true).
		SetBorderColor(tcell.GetColor("lavender")).
		SetTitleColor(tcell.GetColor("lavender")).
		SetTitleAlign(tview.AlignLeft)
	frame.SetTitle(fmt.Sprintf(" permissions  (%s)  d/Del removes · Esc closes ", path))

	close := func() {
		pages.RemovePage(pageName)
		if focusAfter != nil {
			app.SetFocus(focusAfter)
		}
	}

	// rules tracks the current row order so Delete can find what to
	// remove without re-parsing list text.
	type rowRule struct {
		kind    string // "allow" / "ask" / "deny"
		pattern string
	}
	var rules []rowRule

	repopulate := func() {
		list.Clear()
		rules = rules[:0]

		allow, ask, deny, err := config.LoadRules(path)
		if err != nil {
			list.AddItem("(error loading rules)", err.Error(), 0, nil)
			rules = append(rules, rowRule{})
			return
		}
		if len(allow)+len(ask)+len(deny) == 0 {
			list.AddItem("(no project-local rules)",
				"\"Allow + Remember\" on a permission prompt writes here.", 0, nil)
			rules = append(rules, rowRule{})
			return
		}

		add := func(kindLabel, kindKey, color string, patterns []string) {
			for _, pat := range patterns {
				list.AddItem(
					fmt.Sprintf("[%s]%s[-]  %s", color, kindLabel, pat),
					"",
					0, nil,
				)
				rules = append(rules, rowRule{kind: kindKey, pattern: pat})
			}
		}
		add("DENY ", "deny", "red", deny)
		add("ASK  ", "ask", "yellow", ask)
		add("ALLOW", "allow", "teal", allow)
	}
	repopulate()

	deleteCurrent := func() {
		idx := list.GetCurrentItem()
		if idx < 0 || idx >= len(rules) {
			return
		}
		r := rules[idx]
		if r.pattern == "" {
			return
		}
		// Remove from disk first; if that fails we leave the live
		// checker untouched so on-disk and in-memory don't diverge.
		removed, err := config.RemoveRule(path, r.kind, r.pattern)
		if err != nil {
			fmt.Fprintf(chatView, "[red]/permissions: %v[-]\n\n", err)
			chatView.ScrollToEnd()
			return
		}
		if !removed {
			// File-side row vanished between load and delete (manual
			// edit, race). Just refresh.
			repopulate()
			return
		}
		var kind permissions.Kind
		switch r.kind {
		case "allow":
			kind = permissions.KindAllow
		case "ask":
			kind = permissions.KindAsk
		case "deny":
			kind = permissions.KindDeny
		}
		if _, err := checker.RemoveRule(r.pattern, kind); err != nil {
			fmt.Fprintf(chatView, "[yellow]removed from %s but live checker reports: %v[-]\n\n", path, err)
			chatView.ScrollToEnd()
		} else {
			fmt.Fprintf(chatView, "[teal]permissions: removed %s %q[-]\n\n", r.kind, r.pattern)
			chatView.ScrollToEnd()
		}
		repopulate()
	}

	frame.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Key() {
		case tcell.KeyEscape:
			close()
			return nil
		case tcell.KeyDelete:
			deleteCurrent()
			return nil
		}
		if event.Rune() == 'd' || event.Rune() == 'D' {
			deleteCurrent()
			return nil
		}
		return event
	})

	overlay := centered(frame, 90, 22)
	pages.AddPage(pageName, overlay, true, true)
	app.SetFocus(list)
}
