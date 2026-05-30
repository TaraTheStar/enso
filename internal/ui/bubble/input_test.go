// SPDX-License-Identifier: AGPL-3.0-or-later

package bubble

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// Regression: a cursor sitting exactly on a '\n' byte belongs to no wrap
// segment (the line it terminates ends at that byte, exclusive; the next
// starts after it). cursorLine then fell through to the last row and
// render sliced s.buf[sg.start:s.cursor] with sg.start > s.cursor, panicking
// with "slice bounds out of range [N:N-1]".
func TestInputRender_CursorOnNewlineNoPanic(t *testing.T) {
	s := &inputState{buf: "ab\ncd", cursor: 2} // cursor on the '\n'
	out := s.render(80)
	if !strings.Contains(out, "ab") || !strings.Contains(out, "cd") {
		t.Fatalf("render dropped content: %q", out)
	}
}

func TestInputUpDown_AcrossNewlines(t *testing.T) {
	// "abc\ndef\nghi": a0 b1 c2 \n3 d4 e5 f6 \n7 g8 h9 i10
	s := &inputState{buf: "abc\ndef\nghi"}

	s.cursor = 5 // 'e' on "def", column 1
	s.up()
	if s.cursor != 1 { // 'b' on "abc", column 1
		t.Fatalf("up: cursor=%d, want 1", s.cursor)
	}

	s.cursor = 5
	s.down()
	if s.cursor != 9 { // 'h' on "ghi", column 1
		t.Fatalf("down: cursor=%d, want 9", s.cursor)
	}
}

func TestInputUpDown_Edges(t *testing.T) {
	s := &inputState{buf: "abc\ndef", cursor: 1} // top row
	s.up()
	if s.cursor != 0 {
		t.Fatalf("up at top row: cursor=%d, want 0 (buffer start)", s.cursor)
	}

	s = &inputState{buf: "abc\ndef", cursor: 5} // bottom row
	s.down()
	if s.cursor != len(s.buf) {
		t.Fatalf("down at bottom row: cursor=%d, want %d (buffer end)", s.cursor, len(s.buf))
	}
}

func TestInputUp_ColumnClampsToShorterLine(t *testing.T) {
	// "ab\nlongerline": cursor at column 5 of the long line clamps to the
	// end of the short line above.
	s := &inputState{buf: "ab\nlongerline", cursor: 8} // 'r', column 5 of "longerline"
	s.up()
	if s.cursor != 2 { // end of "ab" (the newline position)
		t.Fatalf("up into shorter line: cursor=%d, want 2", s.cursor)
	}
}

func TestInputUpDown_SoftWrappedRows(t *testing.T) {
	// No newline; a width of 5 wraps "xxxxxyyyyy" into two visual rows.
	s := &inputState{buf: "xxxxxyyyyy", cursor: 2, lastAvail: 5}
	s.down()
	if s.cursor != 7 { // row 1 (starts at 5), same column 2
		t.Fatalf("down across wrap: cursor=%d, want 7", s.cursor)
	}
	s.up()
	if s.cursor != 2 { // back to row 0, column 2
		t.Fatalf("up across wrap: cursor=%d, want 2", s.cursor)
	}
}

// render must never panic for any valid cursor position (rune boundary,
// 0..len(buf)) across newlines, wrapping, narrow widths, and multibyte
// runes. Exercises the gap-between-segments and wrap-boundary paths.
func TestInputRender_NoPanicAcrossCursorsAndWidths(t *testing.T) {
	bufs := []string{
		"",
		"ab\ncd",
		"line1\n\nline3",
		"a\n",
		"\n\n\n",
		strings.Repeat("x", 200) + "\n" + strings.Repeat("y", 200),
		"héllo\nwörld\n☃ snowman\n",
		"trailing newline then cursor past\n",
	}
	widths := []int{0, 1, 4, 10, 40, 80}
	for _, b := range bufs {
		// Valid cursor positions: every rune boundary, plus len(b).
		var positions []int
		for c := 0; c < len(b); {
			positions = append(positions, c)
			_, size := utf8.DecodeRuneInString(b[c:])
			c += size
		}
		positions = append(positions, len(b))

		for _, c := range positions {
			for _, w := range widths {
				func() {
					defer func() {
						if r := recover(); r != nil {
							t.Fatalf("render panicked: buf=%q cursor=%d width=%d: %v", b, c, w, r)
						}
					}()
					s := &inputState{buf: b, cursor: c}
					_ = s.render(w)
				}()
			}
		}
	}
}
