// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"fmt"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// findDebounce keeps a fast-typed query from kicking off N searches —
// only the last keystroke before the timer fires triggers the actual
// scan. Mirrors grepDebounce; the in-chat corpus is small enough that
// the savings are mostly cosmetic, but the consistent UX is worth it.
const findDebounce = 80 * time.Millisecond

// findMaxResults caps how many hits we surface. A `for` in a long
// session can match hundreds of times — past 200 the result list
// stops being scannable, and the user should refine the query.
const findMaxResults = 200

// ShowFindOverlay opens an incremental in-chat search. Unlike /grep
// (which spans past sessions on disk), /find scans the current chat's
// block model — `chatDisp.Blocks()`. On Enter, the picked hit's block
// is highlighted via tview region markup and the chat scrolls to it.
// Esc clears the highlight and dismisses.
//
// In-overlay toggle:
//
//	Ctrl-R  flip regex mode
func ShowFindOverlay(
	app *tview.Application,
	pages *tview.Pages,
	focusAfter tview.Primitive,
	chatDisp *ChatDisplay,
	initial string,
	regexInitial bool,
) {
	const pageName = "find"

	// Region tags on chat blocks are only present after a redraw — the
	// live-paint path bypasses them. Force one before the overlay opens
	// so HighlightBlock can find its target regardless of when the
	// in-flight stream last touched the view.
	chatDisp.Redraw()

	useRegex := regexInitial

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

	var debounceTimer *time.Timer

	close := func() {
		if debounceTimer != nil {
			debounceTimer.Stop()
		}
		chatDisp.ClearHighlight()
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
		search.SetLabel(fmt.Sprintf(" /find [%s] ", mode))
		frame.SetTitle(fmt.Sprintf(
			" find  (Enter jumps · Ctrl-R %s · Esc cancels) ",
			toggleHint("regex", useRegex),
		))
	}

	// rowCallbacks parallels the list rows so Enter from the search
	// field can fire the current row's action without going through
	// tview's List click handler. Placeholder rows get nil callbacks.
	var rowCallbacks []func()

	repopulate := func(query string) {
		list.Clear()
		rowCallbacks = rowCallbacks[:0]

		addPlaceholder := func(main, secondary string) {
			list.AddItem(main, secondary, 0, nil)
			rowCallbacks = append(rowCallbacks, nil)
		}

		if query == "" {
			addPlaceholder("(type to search this chat)", "")
			return
		}

		hits, err := findInBlocks(chatDisp.Blocks(), query, useRegex)
		if err != nil {
			addPlaceholder("(invalid regex)", err.Error())
			return
		}
		if len(hits) == 0 {
			addPlaceholder("(no matches)", "")
			return
		}

		truncated := false
		if len(hits) > findMaxResults {
			hits = hits[:findMaxResults]
			truncated = true
		}

		for _, h := range hits {
			h := h
			main := fmt.Sprintf("%s: %s", h.role, h.snippet)
			secondary := fmt.Sprintf("block %d", h.blockIdx)
			cb := func() {
				chatDisp.HighlightBlock(h.blockIdx)
				close()
			}
			list.AddItem(main, secondary, 0, cb)
			rowCallbacks = append(rowCallbacks, cb)
		}
		if truncated {
			addPlaceholder(fmt.Sprintf("(showing first %d — narrow your query for more)", findMaxResults), "")
		}
	}

	refreshLabel()
	repopulate(initial)

	search.SetChangedFunc(func(text string) {
		if debounceTimer != nil {
			debounceTimer.Stop()
		}
		debounceTimer = time.AfterFunc(findDebounce, func() {
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
		}
		return event
	})

	overlay := centered(frame, 100, 24)
	pages.AddPage(pageName, overlay, true, true)
	app.SetFocus(search)
}
