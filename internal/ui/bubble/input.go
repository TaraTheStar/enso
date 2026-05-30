// SPDX-License-Identifier: AGPL-3.0-or-later

package bubble

import (
	"strings"
	"unicode/utf8"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// inputState owns the live input buffer, cursor position, and vim-mode
// state. The buffer can hold newlines — shift+enter/alt+enter/ctrl+j
// insert literal \n, bracketed paste preserves them, and render
// soft-wraps + vertically scrolls within maxInputLines. Plain Enter
// submits the whole buffer (multi-line and all). The vim feature set
// is still single-line in spirit: motion (h, l, 0, $, w, b), edit (x),
// insert (i, a, A) — j/k across lines is not implemented.
type inputState struct {
	buf       string // raw user-typed text
	cursor    int    // byte offset into buf; 0..len(buf) inclusive
	vim       bool   // vim mode enabled at all
	vimNormal bool   // true = normal mode, false = insert mode (only meaningful when vim=true)
	lastAvail int    // available text columns from the last render; used by up/down to wrap consistently
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

// maxInputLines is how many visual rows the input grows to before it
// starts scrolling vertically within itself. Long input soft-wraps onto
// up to this many lines; past that the window scrolls so the cursor's
// line stays visible.
const maxInputLines = 3

// render draws the prompt + the buffer soft-wrapped to the terminal
// width, with the cursor visualised in reverse video. Rather than
// horizontally scrolling a single line, the buffer folds onto up to
// maxInputLines rows; once it would exceed that the visible window
// scrolls vertically so the cursor's row is always on-screen. The
// prompt sits on the first visible row; continuation rows are indented
// to align under it. width is the terminal width (m.width); <=0 (no
// WindowSizeMsg yet) falls back to 80.
//
// vim mode carries a NORMAL/INSERT badge on the prompt so the user
// knows which mode they're in.
func (s *inputState) render(width int) string {
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

	if width <= 0 {
		width = 80 // pre-WindowSizeMsg fallback
	}
	promptW := ansi.StringWidth(prompt)
	avail := width - promptW
	if avail < 8 {
		avail = 8 // degenerate-narrow guard; still bounded
	}
	// Remember the geometry so up/down can wrap the buffer the same way
	// render does, without the model having to re-derive promptW.
	s.lastAvail = avail

	segs := s.wrap(avail)
	// Cursor at end of a row that's exactly full: the cursor cell spills
	// onto a fresh row.
	cursorAtEnd := s.cursor >= len(s.buf)
	if cursorAtEnd && len(s.buf) > 0 {
		last := segs[len(segs)-1]
		if dispWidth(s.buf, last.start, last.end) == avail {
			segs = append(segs, inputSeg{len(s.buf), len(s.buf)})
		}
	}

	cursorLine, cursorOnNewline := locateRow(s.buf, s.cursor, segs)

	// Vertical window: keep the cursor row visible. Stateless and
	// jump-scrolling, mirroring how the old horizontal window worked.
	start := 0
	if cursorLine >= maxInputLines {
		start = cursorLine - maxInputLines + 1
	}
	if m := len(segs) - maxInputLines; m > 0 && start > m {
		start = m
	}
	end := start + maxInputLines
	if end > len(segs) {
		end = len(segs)
	}

	cursor := lipgloss.NewStyle().Reverse(true)
	indent := strings.Repeat(" ", promptW)
	out := make([]string, 0, end-start)
	for idx := start; idx < end; idx++ {
		sg := segs[idx]
		line := s.buf[sg.start:sg.end]
		if idx == cursorLine {
			if cursorAtEnd || cursorOnNewline {
				// Cursor past the buffer, or sitting on the newline that
				// terminates this line: show it as a trailing cell — the
				// rune at s.cursor is either absent or the '\n' itself, so
				// it can't be sliced into the line body.
				line += cursor.Render(" ")
			} else {
				r, size := utf8.DecodeRuneInString(s.buf[s.cursor:])
				line = s.buf[sg.start:s.cursor] + cursor.Render(string(r)) + s.buf[s.cursor+size:sg.end]
			}
		}
		prefix := indent
		if idx == start {
			prefix = prompt
		}
		out = append(out, prefix+line)
	}
	return strings.Join(out, "\n")
}

// inputSeg is a half-open byte range [start,end) of s.buf occupying one
// visual row after soft-wrapping.
type inputSeg struct{ start, end int }

// wrap soft-wraps the buffer into byte ranges, each <= avail display
// columns, breaking on explicit newlines. Done by hand (not ansi.Hardwrap)
// so the column geometry is exact — no leading-space trimming to desync the
// cursor row — and so render and up/down agree on row boundaries.
func (s *inputState) wrap(avail int) []inputSeg {
	if avail < 1 {
		avail = 1
	}
	var segs []inputSeg
	lineStart, col := 0, 0
	for i := 0; i < len(s.buf); {
		r, size := utf8.DecodeRuneInString(s.buf[i:])
		if r == '\n' {
			segs = append(segs, inputSeg{lineStart, i})
			i += size
			lineStart, col = i, 0
			continue
		}
		w := ansi.StringWidth(string(r))
		if col+w > avail {
			segs = append(segs, inputSeg{lineStart, i})
			lineStart, col = i, 0
		}
		col += w
		i += size
	}
	segs = append(segs, inputSeg{lineStart, len(s.buf)})
	return segs
}

// locateRow returns the index of the wrap segment the cursor sits on, and
// whether it sits exactly on the '\n' that terminates that line. A cursor
// on a wrap boundary shows on the next row (where the rune it points at
// lives). A '\n' byte belongs to no segment — the line it terminates ends
// at that byte (exclusive) and the next starts after it — so it is
// attributed to the line it terminates; without this the cursor row would
// fall through to the last row and render would slice buf[start:cursor]
// with start > cursor and panic.
func locateRow(buf string, cursor int, segs []inputSeg) (row int, onNewline bool) {
	for idx, sg := range segs {
		if cursor >= sg.start && cursor < sg.end {
			return idx, false
		}
		if cursor == sg.end && sg.end < len(buf) && buf[sg.end] == '\n' {
			return idx, true
		}
	}
	return len(segs) - 1, false
}

// dispWidth is the display-column width of buf[start:end].
func dispWidth(buf string, start, end int) int {
	w := 0
	for i := start; i < end; {
		r, size := utf8.DecodeRuneInString(buf[i:])
		w += ansi.StringWidth(string(r))
		i += size
	}
	return w
}

// offsetAtCol returns the byte offset within [sg.start, sg.end] whose
// display column is the largest not exceeding targetCol — i.e. where a
// cursor lands when moving vertically into this row while keeping its
// column. Always a rune boundary.
func offsetAtCol(buf string, sg inputSeg, targetCol int) int {
	col, i := 0, sg.start
	for i < sg.end {
		r, size := utf8.DecodeRuneInString(buf[i:])
		w := ansi.StringWidth(string(r))
		if col+w > targetCol {
			break
		}
		col += w
		i += size
	}
	return i
}

// availForNav returns the wrap width to use for vertical motion: the last
// rendered width, or an 80-col fallback before the first render.
func (s *inputState) availForNav() int {
	if s.lastAvail > 0 {
		return s.lastAvail
	}
	return 80 - 2 // minus the "› " prompt
}

// up moves the cursor to the row visually above, keeping the display column
// as close as possible (cursor stays on a rune boundary). On the top row it
// goes to the buffer start. Uses the geometry from the last render so the
// motion matches what the user sees.
func (s *inputState) up() {
	segs := s.wrap(s.availForNav())
	row, _ := locateRow(s.buf, s.cursor, segs)
	if row <= 0 {
		s.cursor = 0
		return
	}
	col := dispWidth(s.buf, segs[row].start, s.cursor)
	s.cursor = offsetAtCol(s.buf, segs[row-1], col)
}

// down moves the cursor to the row visually below, keeping the display
// column. On the bottom row it goes to the buffer end.
func (s *inputState) down() {
	segs := s.wrap(s.availForNav())
	row, _ := locateRow(s.buf, s.cursor, segs)
	if row >= len(segs)-1 {
		s.cursor = len(s.buf)
		return
	}
	col := dispWidth(s.buf, segs[row].start, s.cursor)
	s.cursor = offsetAtCol(s.buf, segs[row+1], col)
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
