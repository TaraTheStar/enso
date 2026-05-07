// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"testing"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

func newVimHandler(t *testing.T, text string) (*InputHandler, *tview.TextArea) {
	t.Helper()
	area := tview.NewTextArea()
	area.SetText(text, true) // cursor at end
	h := &InputHandler{area: area}
	h.EnableVim(true, nil)
	return h, area
}

func TestVimNormalKey_HToLeftJtoDown(t *testing.T) {
	h, _ := newVimHandler(t, "")
	for ch, want := range map[rune]tcell.Key{
		'h': tcell.KeyLeft,
		'j': tcell.KeyDown,
		'k': tcell.KeyUp,
		'l': tcell.KeyRight,
		'0': tcell.KeyHome,
		'$': tcell.KeyEnd,
	} {
		ev := h.handleKey(tcell.NewEventKey(tcell.KeyRune, ch, tcell.ModNone))
		if ev == nil {
			t.Errorf("%q: expected translated event, got nil", ch)
			continue
		}
		if ev.Key() != want {
			t.Errorf("%q: got key %v, want %v", ch, ev.Key(), want)
		}
	}
}

func TestVimNormalSwallowsLetters(t *testing.T) {
	h, _ := newVimHandler(t, "")
	// `z` is not bound; should be swallowed.
	if ev := h.handleKey(tcell.NewEventKey(tcell.KeyRune, 'z', tcell.ModNone)); ev != nil {
		t.Errorf("normal-mode `z` should be swallowed, got %v", ev)
	}
}

func TestVimEnterInsertViaI(t *testing.T) {
	h, _ := newVimHandler(t, "abc")
	if !h.vimNormal {
		t.Fatalf("expected to start in normal mode")
	}
	h.handleKey(tcell.NewEventKey(tcell.KeyRune, 'i', tcell.ModNone))
	if h.vimNormal {
		t.Errorf("`i` should switch to insert mode")
	}
}

func TestVimEscReturnsToNormal(t *testing.T) {
	h, _ := newVimHandler(t, "abc")
	h.handleKey(tcell.NewEventKey(tcell.KeyRune, 'i', tcell.ModNone))
	if h.vimNormal {
		t.Fatal("expected insert mode after `i`")
	}
	h.handleKey(tcell.NewEventKey(tcell.KeyEscape, 0, tcell.ModNone))
	if !h.vimNormal {
		t.Errorf("Esc should return to normal mode")
	}
}

func TestVimOInsertsNewline(t *testing.T) {
	h, area := newVimHandler(t, "first\nsecond")
	// Cursor is at end of "second"; `o` should insert a newline after it.
	h.handleKey(tcell.NewEventKey(tcell.KeyRune, 'o', tcell.ModNone))
	if h.vimNormal {
		t.Errorf("`o` should switch to insert mode")
	}
	if got := area.GetText(); got != "first\nsecond\n" {
		t.Errorf("expected newline appended, got %q", got)
	}
}

func TestVimSubmitInNormalMode(t *testing.T) {
	var got string
	h, area := newVimHandler(t, "do thing")
	h.onSubmit = func(text string) { got = text }

	h.handleKey(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone))
	if got != "do thing" {
		t.Errorf("Enter in normal mode should submit, got %q", got)
	}
	if area.GetText() != "" {
		t.Errorf("expected input cleared after submit, got %q", area.GetText())
	}
}

func TestNonVimUnaffected(t *testing.T) {
	area := tview.NewTextArea()
	h := &InputHandler{area: area}
	// Without EnableVim, `h` should reach the textarea unchanged so the
	// user can type literal "h".
	ev := h.handleKey(tcell.NewEventKey(tcell.KeyRune, 'h', tcell.ModNone))
	if ev == nil {
		t.Errorf("non-vim mode should pass `h` through")
	}
}
