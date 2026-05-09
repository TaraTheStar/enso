// SPDX-License-Identifier: AGPL-3.0-or-later

package bubble

// handleVimNormalKey is the key handler for vim normal mode. It
// returns (handled, exit). When handled=false the caller falls through
// to the default key handling. exit=true switches to insert mode.
//
// Single-line subset of vim's normal-mode keymap:
//
//	h l       cursor left / right
//	0 $       start / end of line
//	w b       word forward / backward
//	x         delete char under cursor
//	i         enter insert at cursor
//	a         enter insert one right of cursor
//	A         enter insert at end
//	Esc       no-op (already normal)
//
// j, k, gg, G, o, O are dropped — bubble's input is single-line so
// they don't apply.
func handleVimNormalKey(s *inputState, key string, runes []rune) (handled, exit bool) {
	// Esc explicitly stays in normal.
	if key == "esc" {
		return true, false
	}
	// Allow plain arrows / Home / End / Backspace through to the normal
	// handler so muscle memory still works.
	switch key {
	case "left", "right", "home", "end", "backspace", "ctrl+left", "ctrl+right":
		return false, false
	}

	if len(runes) == 0 {
		return true, false
	}
	switch runes[0] {
	case 'h':
		s.left()
	case 'l':
		s.right()
	case '0':
		s.home()
	case '$':
		s.end()
	case 'w':
		s.wordForward()
	case 'b':
		s.wordBack()
	case 'x':
		s.deleteAtCursor()
	case 'i':
		return true, true
	case 'a':
		// `a`: enter insert one rune right of cursor (so typing
		// appends after the current char).
		s.right()
		return true, true
	case 'A':
		s.cursor = len(s.buf)
		return true, true
	}
	// Every other rune is swallowed in normal mode so users don't
	// accidentally type into the buffer.
	return true, false
}
