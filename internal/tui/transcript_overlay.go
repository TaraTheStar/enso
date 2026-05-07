// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"fmt"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/TaraTheStar/enso/internal/llm"
)

// ShowTranscriptOverlay renders the captured History of a sub-agent in a
// modal overlay. Reuses the chat block model via ChatDisplay.ReplayHistory
// so the output looks identical to how the chat lane renders user/asst/
// tool turns.
//
// `history` may be empty — that means the agent is still in flight (or
// captured no messages); show a hint instead of a blank view.
func ShowTranscriptOverlay(
	app *tview.Application,
	pages *tview.Pages,
	focusAfter tview.Primitive,
	agentID string,
	history []llm.Message,
) {
	view := tview.NewTextView()
	view.SetDynamicColors(true)
	view.SetScrollable(true)
	view.SetWordWrap(true)
	view.SetBorderPadding(0, 0, 1, 1)

	if len(history) == 0 {
		fmt.Fprint(view,
			"[teal]No transcript captured yet. The agent is still running or completed without a stored history. Esc to close.[-]\n")
	} else {
		// Reuse the same renderer the main chat uses so styling matches.
		disp := NewChatDisplay(view, "agent")
		disp.ReplayHistory(history, "agent")
	}

	frame := tview.NewFlex().SetDirection(tview.FlexRow).AddItem(view, 0, 1, true)
	frame.SetBorder(true).
		SetBorderColor(tcell.GetColor("lavender")).
		SetTitle(fmt.Sprintf(" agent %s  (Esc closes) ", shortID(agentID))).
		SetTitleColor(tcell.GetColor("lavender")).
		SetTitleAlign(tview.AlignLeft)

	frame.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEscape {
			pages.RemovePage("transcript")
			if focusAfter != nil {
				app.SetFocus(focusAfter)
			}
			return nil
		}
		return event
	})

	overlay := centered(frame, 100, 30)
	pages.AddPage("transcript", overlay, true, true)
	app.SetFocus(view)
}
