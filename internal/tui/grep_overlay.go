// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"fmt"
	"regexp"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/TaraTheStar/enso/internal/session"
)

// grepDebounce is how long we wait after the last keystroke before
// re-running the search. Keeps a fast-typed query from kicking off N
// queries when the user only cares about the final one.
const grepDebounce = 100 * time.Millisecond

// ShowGrepOverlay opens an incremental search modal over past sessions.
// `initial` is prepopulated into the search field. As the user types,
// the result list refreshes via session.Search (substring) or
// session.SearchRegex (regex mode). Enter on a row fires
// `onSwitch(sessionID)`; Esc dismisses.
//
// In-overlay toggles:
//
//	Ctrl-R  flip regex mode
//	Ctrl-A  flip cwd-filter (off = search all sessions)
//
// `cwd` is the project cwd to scope to when the cwd filter is on.
// Pass allInitial=true to start in all-sessions mode.
func ShowGrepOverlay(
	app *tview.Application,
	pages *tview.Pages,
	focusAfter tview.Primitive,
	chatView *tview.TextView,
	store *session.Store,
	cwd, initial string,
	regexInitial, allInitial bool,
	onSwitch func(id string),
) {
	if store == nil {
		fmt.Fprint(chatView, "[teal]grep: store unavailable (running --ephemeral)[-]\n\n")
		chatView.ScrollToEnd()
		return
	}

	const pageName = "grep"
	const maxHits = 50

	useRegex := regexInitial
	allCwds := allInitial

	search := tview.NewInputField()
	search.SetFieldBackgroundColor(tcell.ColorDefault).
		SetLabelColor(tcell.GetColor("teal"))
	search.SetText(initial)

	list := tview.NewList()
	list.ShowSecondaryText(true)
	list.SetMainTextColor(tcell.GetColor("white"))
	list.SetSecondaryTextColor(tcell.GetColor("teal"))
	list.SetSelectedBackgroundColor(tcell.GetColor("mauve"))
	list.SetSelectedTextColor(tcell.GetColor("black"))
	list.SetHighlightFullLine(true)

	frame := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(search, 1, 0, true).
		AddItem(list, 0, 1, false)
	frame.SetBorder(true).
		SetBorderColor(tcell.GetColor("lavender")).
		SetTitleColor(tcell.GetColor("lavender")).
		SetTitleAlign(tview.AlignLeft)

	// debounceTimer fires repopulate after grepDebounce of typing
	// inactivity. Reset on each keystroke. The timer goroutine wraps
	// the redraw in QueueUpdateDraw because it's not on the tview event
	// loop. Stopped on close so no stray refresh hits a removed page.
	var debounceTimer *time.Timer

	close := func() {
		if debounceTimer != nil {
			debounceTimer.Stop()
		}
		pages.RemovePage(pageName)
		if focusAfter != nil {
			app.SetFocus(focusAfter)
		}
	}

	refreshLabel := func() {
		mode := "substring"
		if useRegex {
			mode = "regex"
		}
		scope := "cwd"
		if allCwds {
			scope = "all"
		}
		search.SetLabel(fmt.Sprintf(" /grep [%s · %s] ", mode, scope))
		frame.SetTitle(fmt.Sprintf(
			" grep  (Enter switches · Ctrl-R %s · Ctrl-A %s · Esc cancels) ",
			toggleHint("regex", useRegex),
			toggleHint("all", allCwds),
		))
	}

	// rowCallbacks parallels the list rows. tview's List doesn't expose
	// a way to invoke a row's callback programmatically, so we keep our
	// own slice and fire by index when Enter is pressed in the search
	// field. Placeholder rows ("(no matches)" etc.) get a nil callback.
	var rowCallbacks []func()

	repopulate := func(query string) {
		list.Clear()
		rowCallbacks = rowCallbacks[:0]

		addPlaceholder := func(main, secondary string) {
			list.AddItem(main, secondary, 0, nil)
			rowCallbacks = append(rowCallbacks, nil)
		}

		if query == "" {
			addPlaceholder("(type to search)", "")
			return
		}

		scope := cwd
		if allCwds {
			scope = ""
		}

		var hits []session.Hit
		var err error
		if useRegex {
			re, reErr := regexp.Compile(query)
			if reErr != nil {
				addPlaceholder("(invalid regex)", reErr.Error())
				return
			}
			hits, err = session.SearchRegex(store, re, scope, maxHits)
		} else {
			hits, err = session.Search(store, query, scope, maxHits)
		}
		if err != nil {
			addPlaceholder("(error)", err.Error())
			return
		}
		if len(hits) == 0 {
			addPlaceholder("(no matches)", "")
			return
		}
		for _, h := range hits {
			h := h
			snippet := h.Snippet
			if h.Truncated {
				snippet += " [yellow](scanned head only — message exceeds 256 KiB)[-]"
			}
			main := fmt.Sprintf("%s  %s: %s", shortID(h.SessionID), h.Role, snippet)
			secondary := fmt.Sprintf("%s · %s", relTime(h.UpdatedAt), h.Cwd)
			cb := func() {
				close()
				if onSwitch != nil {
					onSwitch(h.SessionID)
				}
			}
			list.AddItem(main, secondary, 0, cb)
			rowCallbacks = append(rowCallbacks, cb)
		}
	}

	refreshLabel()
	repopulate(initial)

	search.SetChangedFunc(func(text string) {
		if debounceTimer != nil {
			debounceTimer.Stop()
		}
		debounceTimer = time.AfterFunc(grepDebounce, func() {
			app.QueueUpdateDraw(func() { repopulate(text) })
		})
	})

	fireCurrent := func() {
		idx := list.GetCurrentItem()
		if idx < 0 || idx >= len(rowCallbacks) {
			return
		}
		if cb := rowCallbacks[idx]; cb != nil {
			cb()
		}
	}

	search.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Key() {
		case tcell.KeyDown:
			if cur := list.GetCurrentItem(); cur < list.GetItemCount()-1 {
				list.SetCurrentItem(cur + 1)
			}
			return nil
		case tcell.KeyUp:
			if cur := list.GetCurrentItem(); cur > 0 {
				list.SetCurrentItem(cur - 1)
			}
			return nil
		case tcell.KeyEnter:
			fireCurrent()
			return nil
		case tcell.KeyEscape:
			close()
			return nil
		case tcell.KeyCtrlR:
			useRegex = !useRegex
			refreshLabel()
			repopulate(search.GetText())
			return nil
		case tcell.KeyCtrlA:
			allCwds = !allCwds
			refreshLabel()
			repopulate(search.GetText())
			return nil
		}
		return event
	})

	overlay := centered(frame, 100, 24)
	pages.AddPage(pageName, overlay, true, true)
	app.SetFocus(search)
}

func toggleHint(label string, on bool) string {
	if on {
		return "[" + label + " on]"
	}
	return "[" + label + " off]"
}
