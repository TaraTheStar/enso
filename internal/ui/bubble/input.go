// SPDX-License-Identifier: AGPL-3.0-or-later

package bubble

import (
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
)

// inputState owns the live input buffer, cursor position, and vim-mode
// state. The input is single-line by design (Enter submits) so the
// vim feature set here is single-line: motion (h, l, 0, $, w, b),
// edit (x), insert (i, a, A) — j/k and friends don't apply.
type inputState struct {
	buf       string // raw user-typed text
	cursor    int    // byte offset into buf; 0..len(buf) inclusive
	vim       bool   // vim mode enabled at all
	vimNormal bool   // true = normal mode, false = insert mode (only meaningful when vim=true)
}

// reset clears the buffer + cursor; vim mode and normal/insert state
// are preserved across submissions.
func (s *inputState) reset() {
	s.buf = ""
	s.cursor = 0
}

// insertString inserts at the cursor and advances it past the new text.
func (s *inputState) insertString(text string) {
	if text == "" {
		return
	}
	s.buf = s.buf[:s.cursor] + text + s.buf[s.cursor:]
	s.cursor += len(text)
}

// backspace removes the rune immediately before the cursor.
func (s *inputState) backspace() {
	if s.cursor == 0 {
		return
	}
	r, size := utf8.DecodeLastRuneInString(s.buf[:s.cursor])
	_ = r
	s.buf = s.buf[:s.cursor-size] + s.buf[s.cursor:]
	s.cursor -= size
}

// deleteAtCursor removes the rune under the cursor (vim `x`). Behaves
// like vim: at end-of-line moves cursor back so it stays on a valid
// position.
func (s *inputState) deleteAtCursor() {
	if s.cursor >= len(s.buf) {
		return
	}
	_, size := utf8.DecodeRuneInString(s.buf[s.cursor:])
	s.buf = s.buf[:s.cursor] + s.buf[s.cursor+size:]
	if s.cursor > 0 && s.cursor >= len(s.buf) {
		// Vim normal-mode cursor stays on a char, never past end.
		_, prev := utf8.DecodeLastRuneInString(s.buf[:s.cursor])
		s.cursor -= prev
	}
}

// left moves the cursor one rune left.
func (s *inputState) left() {
	if s.cursor == 0 {
		return
	}
	_, size := utf8.DecodeLastRuneInString(s.buf[:s.cursor])
	s.cursor -= size
}

// right moves the cursor one rune right. In normal-mode vim, cursor
// stays on the last char; in insert mode it can sit past end so newly
// typed characters append.
func (s *inputState) right() {
	if s.cursor >= len(s.buf) {
		return
	}
	_, size := utf8.DecodeRuneInString(s.buf[s.cursor:])
	advance := s.cursor + size
	if s.vim && s.vimNormal && advance >= len(s.buf) {
		// Stay on the last char in normal mode.
		_, lastSize := utf8.DecodeLastRuneInString(s.buf)
		s.cursor = len(s.buf) - lastSize
		return
	}
	s.cursor = advance
}

// home / end move to the start / end of the buffer.
func (s *inputState) home() { s.cursor = 0 }
func (s *inputState) end() {
	if s.vim && s.vimNormal && s.buf != "" {
		// Normal mode: cursor on last char, not past it.
		_, lastSize := utf8.DecodeLastRuneInString(s.buf)
		s.cursor = len(s.buf) - lastSize
		return
	}
	s.cursor = len(s.buf)
}

// wordForward moves to the start of the next word (whitespace-delimited).
// Maps to vim `w` and Ctrl-Right.
func (s *inputState) wordForward() {
	// Skip current word characters.
	for s.cursor < len(s.buf) {
		r, size := utf8.DecodeRuneInString(s.buf[s.cursor:])
		if isWordSep(r) {
			break
		}
		s.cursor += size
	}
	// Skip whitespace to the next word.
	for s.cursor < len(s.buf) {
		r, size := utf8.DecodeRuneInString(s.buf[s.cursor:])
		if !isWordSep(r) {
			break
		}
		s.cursor += size
	}
}

// wordBack moves to the start of the previous word. Maps to vim `b`.
func (s *inputState) wordBack() {
	// Skip whitespace immediately behind the cursor.
	for s.cursor > 0 {
		r, size := utf8.DecodeLastRuneInString(s.buf[:s.cursor])
		if !isWordSep(r) {
			break
		}
		s.cursor -= size
	}
	// Walk back to the start of the word we're now in.
	for s.cursor > 0 {
		r, size := utf8.DecodeLastRuneInString(s.buf[:s.cursor])
		if isWordSep(r) {
			break
		}
		s.cursor -= size
	}
}

func isWordSep(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n'
}

// render returns the prompt + buffer with the cursor visualised. The
// terminal cursor is left at the end of the View output by Bubble Tea;
// to indicate cursor-mid-buffer position we render the rune at cursor
// in reverse video.
//
// vimMode = true causes the prompt to carry a NORMAL/INSERT badge so the
// user knows which mode they're in.
func (s *inputState) render() string {
	prompt := promptStyle.Render("› ")
	if s.vim {
		var label string
		if s.vimNormal {
			label = noticeStyle.Render("NORMAL ")
		} else {
			label = statusStyle.Render("INSERT ")
		}
		prompt = label + prompt
	}

	cursor := lipgloss.NewStyle().Reverse(true)
	if s.cursor >= len(s.buf) {
		// Cursor at end: render a reversed space as the marker.
		return prompt + s.buf + cursor.Render(" ")
	}
	r, size := utf8.DecodeRuneInString(s.buf[s.cursor:])
	return prompt + s.buf[:s.cursor] + cursor.Render(string(r)) + s.buf[s.cursor+size:]
}

// atIsTokenStart reports whether inserting `@` at the current cursor
// would start a new token — i.e., the previous rune is whitespace or
// the cursor is at the start. Mid-token `@` (emails, URLs) doesn't
// fire the file picker.
func (s *inputState) atIsTokenStart() bool {
	if s.cursor == 0 {
		return true
	}
	r, _ := utf8.DecodeLastRuneInString(s.buf[:s.cursor])
	return r == ' ' || r == '\t' || r == '\n'
}

// trimSpace returns the submission-ready buffer (whitespace-trimmed).
func (s *inputState) trimSpace() string { return strings.TrimSpace(s.buf) }
