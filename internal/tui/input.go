// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// InputHandler manages the text input area and key bindings.
//
// Ctrl-C is NOT handled here. tview's Application.Run intercepts Ctrl-C
// before the focused primitive's InputCapture and calls Stop() unless the
// app-level inputCapture swallows it — see internal/tui/app.go and
// internal/tui/attach.go for the actual cancel wiring.
type InputHandler struct {
	area        *tview.TextArea
	onSubmit    func(text string)
	onQuit      func()
	onAtTrigger func() // called when @ would start a new token; set by host
	busy        bool   // true while agent is processing

	// Vim-mode state. When `vim` is true, `vimNormal` toggles between
	// normal-mode (key commands navigate/edit) and insert-mode (default
	// typing). A nil onVimMode is fine — the host only needs it if it
	// wants to surface NORMAL/INSERT in the status bar.
	vim       bool
	vimNormal bool
	onVimMode func(string)
}

// NewInputHandler creates an input handler with key bindings.
func NewInputHandler(area *tview.TextArea, onSubmit func(string), onQuit func()) *InputHandler {
	h := &InputHandler{
		area:     area,
		onSubmit: onSubmit,
		onQuit:   onQuit,
	}

	area.SetInputCapture(h.handleKey)

	return h
}

// EnableVim flips the input handler into vim-mode. The handler starts
// in normal mode; `onMode` is called with "NORMAL" / "INSERT" each time
// the mode changes so the host can surface it in the status bar. Calling
// EnableVim twice is fine; calling with vim=false reverts to default.
func (h *InputHandler) EnableVim(vim bool, onMode func(string)) {
	h.vim = vim
	h.vimNormal = vim // start in normal when entering vim mode
	h.onVimMode = onMode
	if vim {
		h.notifyVimMode()
	}
}

func (h *InputHandler) notifyVimMode() {
	if h.onVimMode == nil {
		return
	}
	if h.vimNormal {
		h.onVimMode("NORMAL")
	} else {
		h.onVimMode("INSERT")
	}
}

// SetOnAtTrigger registers a callback for when the user presses `@` at a
// position where it would start a new token (start of input or after
// whitespace). The handler swallows the keystroke; the callback is
// expected to open the file picker and, on selection, call InsertAtCursor.
func (h *InputHandler) SetOnAtTrigger(cb func()) {
	h.onAtTrigger = cb
}

// InsertAtCursor inserts `text` at the current cursor position in the
// input area. Useful for the file-picker callback.
func (h *InputHandler) InsertAtCursor(text string) {
	off := cursorByteOffset(h.area)
	h.area.Replace(off, off, text)
}

// SetBusy toggles the busy state (disables submit while true).
func (h *InputHandler) SetBusy(busy bool) {
	h.busy = busy
}

// IsBusy reports whether the agent is currently processing a turn.
func (h *InputHandler) IsBusy() bool {
	return h.busy
}

func (h *InputHandler) handleKey(event *tcell.EventKey) *tcell.EventKey {
	if h.vim {
		if h.vimNormal {
			return h.vimNormalKey(event)
		}
		// Insert mode — Esc returns to normal, otherwise fall through.
		if event.Key() == tcell.KeyEscape {
			h.vimNormal = true
			h.notifyVimMode()
			return nil
		}
	}
	switch event.Key() {
	case tcell.KeyEnter:
		// Shift-Enter (kitty-protocol terminals) or Alt-Enter → newline.
		// On terminals that don't propagate the modifier, tcell sees plain
		// Enter here, which we treat as submit. Use Ctrl-J or Alt-Enter as
		// the reliable newline shortcut.
		if event.Modifiers()&(tcell.ModShift|tcell.ModAlt) != 0 {
			return event
		}
		text := h.area.GetText()
		h.area.SetText("", false)
		if text != "" {
			h.onSubmit(text)
		}
		return nil

	case tcell.KeyCtrlJ:
		// ASCII LF — pass through so the TextArea inserts a newline. Works
		// in every terminal regardless of keyboard-protocol support.
		return tcell.NewEventKey(tcell.KeyEnter, '\n', tcell.ModNone)

	case tcell.KeyCtrlD:
		h.onQuit()
		return nil

	case tcell.KeyRune:
		if event.Rune() == '@' && h.onAtTrigger != nil && atIsTokenStart(h.area) {
			h.onAtTrigger()
			return nil
		}
	}

	return event
}

// vimNormalKey is the key handler for vim normal mode. Most letters
// translate to a corresponding cursor or edit action; everything else
// is swallowed so the user doesn't accidentally type into the buffer.
//
// Implemented commands (a deliberately small subset — the goal is "vim
// users feel oriented", not a full vim emulation):
//
//	h j k l   — left, down, up, right
//	0 / $     — start / end of line
//	w / b     — word forward / backward (delegated to ctrl-arrow)
//	gg / G    — top / bottom of buffer
//	x         — delete the character under the cursor
//	i a A     — insert (at cursor / after / at end of line)
//	o O       — open new line below / above + insert
//	Enter     — submit
//	Esc       — no-op (already normal)
//	Ctrl-D    — quit
func (h *InputHandler) vimNormalKey(event *tcell.EventKey) *tcell.EventKey {
	enterInsert := func() {
		h.vimNormal = false
		h.notifyVimMode()
	}
	switch event.Key() {
	case tcell.KeyEscape:
		return nil
	case tcell.KeyEnter:
		text := h.area.GetText()
		h.area.SetText("", false)
		if text != "" {
			h.onSubmit(text)
		}
		return nil
	case tcell.KeyCtrlD:
		h.onQuit()
		return nil
	case tcell.KeyRune:
		// fall through to per-rune dispatch
	default:
		// Allow arrow keys, Backspace, etc. through unchanged so users
		// who reach for them still get the expected behaviour.
		return event
	}

	switch event.Rune() {
	case 'h':
		return tcell.NewEventKey(tcell.KeyLeft, 0, tcell.ModNone)
	case 'j':
		return tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone)
	case 'k':
		return tcell.NewEventKey(tcell.KeyUp, 0, tcell.ModNone)
	case 'l':
		return tcell.NewEventKey(tcell.KeyRight, 0, tcell.ModNone)
	case '0':
		return tcell.NewEventKey(tcell.KeyHome, 0, tcell.ModNone)
	case '$':
		return tcell.NewEventKey(tcell.KeyEnd, 0, tcell.ModNone)
	case 'w':
		return tcell.NewEventKey(tcell.KeyRight, 0, tcell.ModCtrl)
	case 'b':
		return tcell.NewEventKey(tcell.KeyLeft, 0, tcell.ModCtrl)
	case 'G':
		return tcell.NewEventKey(tcell.KeyEnd, 0, tcell.ModCtrl)
	case 'x':
		return tcell.NewEventKey(tcell.KeyDelete, 0, tcell.ModNone)
	case 'i':
		enterInsert()
		return nil
	case 'a':
		enterInsert()
		return tcell.NewEventKey(tcell.KeyRight, 0, tcell.ModNone)
	case 'A':
		enterInsert()
		return tcell.NewEventKey(tcell.KeyEnd, 0, tcell.ModNone)
	case 'o':
		// Move to end of line, insert newline, enter insert mode.
		off := cursorByteOffset(h.area)
		text := h.area.GetText()
		// Find end of current line.
		end := off
		for end < len(text) && text[end] != '\n' {
			end++
		}
		h.area.Replace(end, end, "\n")
		enterInsert()
		return nil
	case 'O':
		// Move to start of line, insert newline before, enter insert.
		off := cursorByteOffset(h.area)
		text := h.area.GetText()
		start := off
		for start > 0 && text[start-1] != '\n' {
			start--
		}
		h.area.Replace(start, start, "\n")
		// Cursor is now after the inserted newline; move up so the
		// caret lands on the new empty line.
		enterInsert()
		return tcell.NewEventKey(tcell.KeyUp, 0, tcell.ModNone)
	}
	// Swallow every other rune in normal mode.
	return nil
}

// atIsTokenStart reports whether inserting `@` at the current cursor would
// start a new token — i.e., the previous rune is whitespace, a newline, or
// the cursor is at index 0. Mid-word `@` (like in an email) is left alone.
func atIsTokenStart(area *tview.TextArea) bool {
	off := cursorByteOffset(area)
	if off == 0 {
		return true
	}
	text := area.GetText()
	if off > len(text) {
		return true
	}
	prev := text[off-1]
	return prev == ' ' || prev == '\t' || prev == '\n'
}

// cursorByteOffset returns the byte offset in area.GetText() that
// corresponds to the current cursor row+column. Walks the text rune-by-
// rune; rare multi-byte content costs a little extra CPU but the input
// area is small.
func cursorByteOffset(area *tview.TextArea) int {
	text := area.GetText()
	fr, fc, _, _ := area.GetCursor()
	row, col := 0, 0
	for i, r := range text {
		if row == fr && col == fc {
			return i
		}
		if r == '\n' {
			row++
			col = 0
		} else {
			col++
		}
	}
	return len(text)
}
