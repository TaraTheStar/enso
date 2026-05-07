// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"github.com/rivo/tview"
)

// Layout holds the TUI screen regions.
type Layout struct {
	app         *tview.Application
	statusLeft  *tview.TextView // mode · activity
	statusRight *tview.TextView // tokens% · t/s
	chat        *tview.TextView
	input       *tview.TextArea
	sidebarView *tview.TextView // session inspector content
	sidebarBox  *tview.Flex     // wrapper that ShowSidebar adds/removes
	mainFlex    *tview.Flex
	topFlex     *tview.Flex
	pages       *tview.Pages
}

// NewLayout creates the three-region TUI layout.
//
// Visual style: low-chrome, linear scrollback feel. No backgrounds, no
// borders, dim accent colors only. The chat is the focus; status and input
// stay quiet.
func NewLayout() *Layout {
	statusLeft := tview.NewTextView().SetDynamicColors(true)
	statusRight := tview.NewTextView().SetDynamicColors(true).SetTextAlign(tview.AlignRight)
	statusBar := tview.NewFlex().
		AddItem(statusLeft, 0, 1, false).
		AddItem(statusRight, 0, 1, false)

	chat := tview.NewTextView()
	chat.SetDynamicColors(true).
		SetScrollable(true).
		SetRegions(true)

	input := tview.NewTextArea().
		SetPlaceholder("Type a message · Shift-Enter / Alt-Enter / Ctrl-J for newline · Ctrl-D quit")

	// Right-side session inspector (visible by default; Ctrl-A toggles
	// for full-width chat). TextView holds preformatted sections for
	// session info, LSPs, MCPs, and (when populated) subagents.
	sidebarView := tview.NewTextView().SetDynamicColors(true)
	sidebarBox := tview.NewFlex().AddItem(sidebarView, 0, 1, false)
	sidebarBox.SetBorderPadding(0, 0, 1, 1)

	// Chat column = scrolling chat with the status pinned to its
	// bottom. Wrapping chat + statusBar in their own vertical flex
	// (rather than dropping statusBar into topFlex) means the status
	// spans only the chat width when the agents pane is open, reading
	// as the trailing edge of the chat region itself rather than chrome
	// that floats across the full screen.
	chatColumn := tview.NewFlex().
		SetDirection(tview.FlexRow).
		AddItem(chat, 0, 1, true).
		AddItem(statusBar, 1, 1, false)

	mainFlex := tview.NewFlex().AddItem(chatColumn, 0, 1, true)

	// Top-level: chat-column (rest), input (3 lines). Status no longer
	// participates here — it lives inside chatColumn.
	topFlex := tview.NewFlex().
		SetDirection(tview.FlexRow).
		AddItem(mainFlex, 0, 1, true).
		AddItem(input, 3, 1, false)

	pages := tview.NewPages().
		AddPage("main", topFlex, true, true)

	return &Layout{
		statusLeft:  statusLeft,
		statusRight: statusRight,
		chat:        chat,
		input:       input,
		sidebarView: sidebarView,
		sidebarBox:  sidebarBox,
		mainFlex:    mainFlex,
		topFlex:     topFlex,
		pages:       pages,
	}
}

// SetStatus writes the two halves of the status bar at once.
func (l *Layout) SetStatus(left, right string) {
	l.statusLeft.SetText(" " + left)
	l.statusRight.SetText(right + " ")
}

// SidebarView returns the inner TextView used by the session inspector.
// Callers populate it via the Sidebar type's Refresh method.
func (l *Layout) SidebarView() *tview.TextView { return l.sidebarView }

// SetRoot sets the layout as the application root and runs. Mouse capture is
// intentionally left off so terminal-native click-and-drag selection (and
// therefore copy) keeps working. Modals are navigated with Tab + Enter.
func (l *Layout) SetRoot(app *tview.Application) error {
	l.app = app
	return app.SetRoot(l.pages, true).SetFocus(l.input).Run()
}

// App returns the underlying tview.Application (set after SetRoot).
func (l *Layout) App() *tview.Application { return l.app }

// Pages returns the pages container for overlaying modals.
func (l *Layout) Pages() *tview.Pages { return l.pages }

// StatusLeft / StatusRight return the two TextViews making up the status
// bar; most callers want SetStatus(left, right) instead.
func (l *Layout) StatusLeft() *tview.TextView  { return l.statusLeft }
func (l *Layout) StatusRight() *tview.TextView { return l.statusRight }

// Chat returns the chat TextView.
func (l *Layout) Chat() *tview.TextView { return l.chat }

// Input returns the input TextArea.
func (l *Layout) Input() *tview.TextArea { return l.input }

// ShowSidebar toggles the session-inspector sidebar's visibility.
// Bound to Ctrl-A in the host so users who want full-width chat can
// hide it and bring it back on demand.
//
// Does NOT call Application.Draw — that method deadlocks when invoked
// from the event loop (per tview's docs: "It can actually deadlock
// your application if you call it from the main thread, e.g. in a
// callback function of a widget"). Ctrl-A's input handler IS the main
// thread, so we let tview's natural post-handler redraw pick up the
// flex change. Initial-startup callers (before SetRoot) get the new
// layout on the very first frame anyway.
func (l *Layout) ShowSidebar(visible bool) {
	if visible {
		l.mainFlex.AddItem(l.sidebarBox, 32, 1, false)
	} else {
		l.mainFlex.RemoveItem(l.sidebarBox)
	}
}
