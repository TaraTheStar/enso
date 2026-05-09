// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"testing"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// TestHandleKey_DoesNotIntercept_UndoRedo locks in that input.go's
// handleKey passes Ctrl-Z and Ctrl-Y through to tview's built-in
// TextArea handler. tview internally binds Ctrl-Z to Undo and Ctrl-Y
// to Redo (see textarea.go ~line 2291), and that's the only path
// users have today to recover an accidentally-deleted prompt. A
// future change that adds `case tcell.KeyCtrlZ: ... return nil` to
// handleKey would silently break that recovery — this test catches
// that regression.
func TestHandleKey_DoesNotIntercept_UndoRedo(t *testing.T) {
	area := tview.NewTextArea()
	h := NewInputHandler(area, func(string) {}, func() {})

	for _, key := range []tcell.Key{tcell.KeyCtrlZ, tcell.KeyCtrlY} {
		evt := tcell.NewEventKey(key, 0, tcell.ModCtrl)
		got := h.handleKey(evt)
		if got == nil {
			t.Errorf("handleKey intercepted %v (returned nil); tview's built-in undo/redo would no longer fire", key)
			continue
		}
		// Same event ref preserves modifiers/rune for the downstream
		// handler. We don't strictly require ref equality, but if
		// something rewrapped the event (e.g. Ctrl-J's LF rewrite) we
		// want to know.
		if got.Key() != key {
			t.Errorf("key %v rewritten to %v on the way through", key, got.Key())
		}
	}
}

// TestVimNormalKey_DoesNotIntercept_UndoRedo: vim's normal-mode
// handler swallows most non-listed runes by returning nil, but Ctrl-
// chord events fall through to the default branch (return event).
// Vim users get tview's undo/redo too — same regression guard.
func TestVimNormalKey_DoesNotIntercept_UndoRedo(t *testing.T) {
	area := tview.NewTextArea()
	h := NewInputHandler(area, func(string) {}, func() {})
	h.vim = true
	h.vimNormal = true

	for _, key := range []tcell.Key{tcell.KeyCtrlZ, tcell.KeyCtrlY} {
		evt := tcell.NewEventKey(key, 0, tcell.ModCtrl)
		got := h.handleKey(evt)
		if got == nil {
			t.Errorf("vim-normal handleKey intercepted %v", key)
		}
	}
}
