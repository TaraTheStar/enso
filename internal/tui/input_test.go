// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"testing"

	"github.com/rivo/tview"
)

// TestCursorByteOffsetMatchesText verifies that the row+column → byte-
// offset converter agrees with what Replace would consume after text was
// typed. We don't have access to the real key path, so we exercise it via
// SetText + Replace: insert text at that offset and confirm the result.
func TestCursorByteOffsetAfterSetText(t *testing.T) {
	cases := []struct {
		text   string
		insert string
		want   string
	}{
		{"hello", "X", "helloX"},                 // cursor at end
		{"line1\nline2", "X", "line1\nline2X"},   // multiline end
		{"", "X", "X"},                           // empty
		{"abc def", "[file] ", "abc def[file] "}, // typical @-picker insertion at end
	}
	for _, tc := range cases {
		area := tview.NewTextArea()
		area.SetText(tc.text, true) // cursor at end
		off := cursorByteOffset(area)
		area.Replace(off, off, tc.insert)
		if got := area.GetText(); got != tc.want {
			t.Errorf("text=%q insert=%q: got %q, want %q", tc.text, tc.insert, got, tc.want)
		}
	}
}

func TestAtIsTokenStart(t *testing.T) {
	cases := []struct {
		text string
		want bool
	}{
		{"", true},           // start of input
		{"summarise ", true}, // after a space
		{"line1\n", true},    // after newline
		{"hello", false},     // mid-word
		{"user@", false},     // looks like email — let it through
	}
	for _, tc := range cases {
		area := tview.NewTextArea()
		area.SetText(tc.text, true)
		if got := atIsTokenStart(area); got != tc.want {
			t.Errorf("text=%q: atIsTokenStart=%v, want %v", tc.text, got, tc.want)
		}
	}
}
