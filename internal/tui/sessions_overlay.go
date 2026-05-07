// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"fmt"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/TaraTheStar/enso/internal/session"
)

// ShowSessionsOverlay opens a modal listing recent sessions. Arrow keys
// navigate; Enter on a row fires the onSwitch callback (host re-execs the
// process with `--session <id>` substituted); Esc closes.
func ShowSessionsOverlay(
	app *tview.Application,
	pages *tview.Pages,
	focusAfter tview.Primitive,
	chatView *tview.TextView,
	store *session.Store,
	onSwitch func(id string),
) {
	if store == nil {
		fmt.Fprint(chatView, "[teal]sessions: store unavailable (running --ephemeral)[-]\n\n")
		chatView.ScrollToEnd()
		return
	}
	infos, err := session.ListRecentWithStats(store, 50)
	if err != nil {
		fmt.Fprintf(chatView, "[red]sessions: %v[-]\n\n", err)
		chatView.ScrollToEnd()
		return
	}

	list := tview.NewList()
	list.ShowSecondaryText(true)
	list.SetMainTextColor(tcell.GetColor("white"))
	list.SetSecondaryTextColor(tcell.GetColor("teal"))
	list.SetSelectedBackgroundColor(tcell.GetColor("mauve"))
	list.SetSelectedTextColor(tcell.GetColor("black"))
	list.SetHighlightFullLine(true)

	close := func() {
		pages.RemovePage("sessions")
		if focusAfter != nil {
			app.SetFocus(focusAfter)
		}
	}

	if len(infos) == 0 {
		list.AddItem("(no sessions yet)", "Start a session by typing in the input below.", 0, close)
	}
	for _, info := range infos {
		info := info
		// Main text leads with the last user message so users can scan
		// by content; the id and metadata sit on the secondary line.
		// Sessions with no user messages yet fall back to the id.
		title := truncateOneLine(info.LastUserMessage, 80)
		if title == "" {
			title = "(no user messages yet)"
		}
		if info.Interrupted {
			title += "  [yellow](interrupted)[-]"
		}
		secondary := fmt.Sprintf("%s · %s · %s · %d msg%s · ~%s tok",
			shortID(info.ID),
			relTime(info.UpdatedAt), info.Cwd,
			info.MessageCount, plural(info.MessageCount),
			compactTokenCount(info.ApproxTokens))
		list.AddItem(title, secondary, 0, func() {
			close()
			if onSwitch != nil {
				onSwitch(info.ID)
			}
		})
	}

	frame := tview.NewFlex().
		SetDirection(tview.FlexRow).
		AddItem(list, 0, 1, true)
	frame.SetBorder(true).
		SetBorderColor(tcell.GetColor("lavender")).
		SetTitle(" sessions  (Enter copies launch command · Esc closes) ").
		SetTitleColor(tcell.GetColor("lavender")).
		SetTitleAlign(tview.AlignLeft)

	frame.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEscape {
			close()
			return nil
		}
		return event
	})

	frame.SetTitle(" sessions  (Enter switches session · Esc closes) ")
	overlay := centered(frame, 90, 22)
	pages.AddPage("sessions", overlay, true, true)
	app.SetFocus(list)
}

// relTime formats t as a short "N {unit} ago" hint.
func relTime(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	default:
		return t.Format("2006-01-02")
	}
}
