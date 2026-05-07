// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"fmt"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/TaraTheStar/enso/internal/picker"
)

// ShowFilePickerOverlay opens a fuzzy file-search modal over the current
// pages. On Enter, `onPick` is called with the chosen path; the host is
// responsible for inserting it into the input area. Esc dismisses without
// invoking onPick.
//
// The walker runs synchronously on the same goroutine; for repos under
// the 5000-file picker cap this is fast enough to feel instant.
func ShowFilePickerOverlay(
	app *tview.Application,
	pages *tview.Pages,
	focusAfter tview.Primitive,
	cwd string,
	extras []string,
	ignore []string,
	onPick func(path string),
) {
	files, err := picker.WalkAll(cwd, extras, ignore)
	if err != nil {
		// Fall back to focus return without spamming the chat — the only
		// way Walk fails is a permission/IO problem on the cwd itself,
		// in which case the agent is already going to misbehave.
		if focusAfter != nil {
			app.SetFocus(focusAfter)
		}
		return
	}

	const pageName = "filepicker"

	search := tview.NewInputField()
	search.SetLabel(" filter: ").
		SetFieldBackgroundColor(tcell.ColorDefault).
		SetLabelColor(tcell.GetColor("teal"))

	list := tview.NewList()
	list.ShowSecondaryText(false)
	list.SetMainTextColor(tcell.GetColor("white"))
	list.SetSelectedBackgroundColor(tcell.GetColor("mauve"))
	list.SetSelectedTextColor(tcell.GetColor("black"))
	list.SetHighlightFullLine(true)

	close := func() {
		pages.RemovePage(pageName)
		if focusAfter != nil {
			app.SetFocus(focusAfter)
		}
	}

	pickFromList := func() {
		if list.GetItemCount() == 0 {
			close()
			return
		}
		main, _ := list.GetItemText(list.GetCurrentItem())
		close()
		if onPick != nil {
			onPick(main)
		}
	}

	repopulate := func(query string) {
		ranked := picker.Rank(files, query, 30)
		list.Clear()
		if len(ranked) == 0 {
			list.AddItem("(no matches)", "", 0, nil)
			return
		}
		for _, p := range ranked {
			p := p
			list.AddItem(p, "", 0, func() {
				close()
				if onPick != nil {
					onPick(p)
				}
			})
		}
	}
	repopulate("")

	search.SetChangedFunc(func(text string) { repopulate(text) })

	// Search field captures arrow keys / Enter so users don't have to
	// tab between fields. Down / Up move the list selection; Enter picks
	// the current item; Esc dismisses.
	search.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Key() {
		case tcell.KeyDown:
			cur := list.GetCurrentItem()
			if cur < list.GetItemCount()-1 {
				list.SetCurrentItem(cur + 1)
			}
			return nil
		case tcell.KeyUp:
			cur := list.GetCurrentItem()
			if cur > 0 {
				list.SetCurrentItem(cur - 1)
			}
			return nil
		case tcell.KeyEnter:
			pickFromList()
			return nil
		case tcell.KeyEscape:
			close()
			return nil
		}
		return event
	})

	frame := tview.NewFlex().
		SetDirection(tview.FlexRow).
		AddItem(search, 1, 0, true).
		AddItem(list, 0, 1, false)
	frame.SetBorder(true).
		SetBorderColor(tcell.GetColor("lavender")).
		SetTitle(fmt.Sprintf(" files  (%d in tree · type to filter · Enter inserts · Esc cancels) ", len(files))).
		SetTitleColor(tcell.GetColor("lavender")).
		SetTitleAlign(tview.AlignLeft)

	overlay := centered(frame, 90, 22)
	pages.AddPage(pageName, overlay, true, true)
	app.SetFocus(search)
}
