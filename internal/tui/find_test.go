// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"strings"
	"testing"

	"github.com/rivo/tview"
)

func TestFindInBlocks_SubstringCaseInsensitive(t *testing.T) {
	blocks := []chatBlock{
		&userBlock{text: "Find the FOO in this Foo and that foo."},
	}
	hits, err := findInBlocks(blocks, "foo", false)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	// Three occurrences regardless of case — this is the value-prop.
	if len(hits) != 3 {
		t.Fatalf("hits=%d, want 3 (case-insensitive substring)", len(hits))
	}
	for _, h := range hits {
		if h.role != "user" {
			t.Errorf("role=%q, want 'user'", h.role)
		}
		if h.blockIdx != 0 {
			t.Errorf("blockIdx=%d, want 0", h.blockIdx)
		}
	}
}

func TestFindInBlocks_RegexMatchesAllHits(t *testing.T) {
	blocks := []chatBlock{
		&assistantBlock{text: "ID 12 then ID 345 then ID 6789"},
	}
	hits, err := findInBlocks(blocks, `ID \d+`, true)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(hits) != 3 {
		t.Errorf("hits=%d, want 3 regex matches", len(hits))
	}
}

func TestFindInBlocks_InvalidRegexErrors(t *testing.T) {
	blocks := []chatBlock{&userBlock{text: "anything"}}
	if _, err := findInBlocks(blocks, "(unclosed", true); err == nil {
		t.Error("expected error from invalid regex")
	}
}

func TestFindInBlocks_AllRoles(t *testing.T) {
	// Each block type with text should be searchable. Tool blocks
	// produce two hits (call + output).
	blocks := []chatBlock{
		&userBlock{text: "needle in user"},
		&assistantHeaderBlock{},
		&assistantBlock{text: "needle in assistant"},
		&toolBlock{call: "bash(needle=1)", output: "found needle in stdout"},
		&reasoningBlock{text: "needle in reasoning"},
		&errorBlock{msg: "needle in error"},
		&cancelledBlock{}, // no text — should be skipped
	}
	hits, err := findInBlocks(blocks, "needle", false)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	wantRoles := map[string]bool{
		"user":        false,
		"assistant":   false,
		"tool":        false,
		"tool-output": false,
		"reasoning":   false,
		"error":       false,
	}
	for _, h := range hits {
		if _, ok := wantRoles[h.role]; ok {
			wantRoles[h.role] = true
		}
	}
	for role, seen := range wantRoles {
		if !seen {
			t.Errorf("missing role %q in hits", role)
		}
	}
}

func TestFindInBlocks_SnippetHasMatchHighlight(t *testing.T) {
	blocks := []chatBlock{&userBlock{text: "the quick brown fox jumps"}}
	hits, err := findInBlocks(blocks, "brown", false)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("hits=%d, want 1", len(hits))
	}
	if !strings.Contains(hits[0].snippet, "[yellow]brown[-]") {
		t.Errorf("snippet missing yellow-tagged match: %q", hits[0].snippet)
	}
}

func TestFindInBlocks_SnippetCollapsesNewlines(t *testing.T) {
	// Match-context snippets must stay single-line so the result list
	// row doesn't wrap weirdly. This is the most likely class-of-bug to
	// regress as block content evolves.
	blocks := []chatBlock{&toolBlock{
		call:   "bash()",
		output: "before\nthe NEEDLE here\nafter",
	}}
	hits, err := findInBlocks(blocks, "NEEDLE", false)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("hits=%d, want 1", len(hits))
	}
	if strings.Contains(hits[0].snippet, "\n") {
		t.Errorf("snippet contains newline: %q", hits[0].snippet)
	}
}

func TestFindInBlocks_EmptyQueryReturnsNil(t *testing.T) {
	blocks := []chatBlock{&userBlock{text: "hello world"}}
	hits, err := findInBlocks(blocks, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if hits != nil {
		t.Errorf("empty query should return nil, got %v", hits)
	}
}

func TestFindInBlocks_BlockIdxPointsAtSlice(t *testing.T) {
	blocks := []chatBlock{
		&userBlock{text: "header line"},
		&assistantBlock{text: "the needle is here"},
		&userBlock{text: "needle again"},
	}
	hits, err := findInBlocks(blocks, "needle", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("hits=%d, want 2", len(hits))
	}
	// First hit is the assistantBlock at index 1 — search must report
	// the slice position so HighlightBlock can address the right
	// region tag.
	if hits[0].blockIdx != 1 {
		t.Errorf("first hit blockIdx=%d, want 1", hits[0].blockIdx)
	}
	if hits[1].blockIdx != 2 {
		t.Errorf("second hit blockIdx=%d, want 2", hits[1].blockIdx)
	}
}

func TestChatRedraw_EmitsBlockRegionTags(t *testing.T) {
	c := NewChatDisplay(tview.NewTextView(), "test")
	c.blocks = []chatBlock{
		&userBlock{text: "hi"},
		&assistantHeaderBlock{},
		&assistantBlock{text: "hello"},
	}
	c.Redraw()
	got := c.view.GetText(false)
	for i := range c.blocks {
		want := `["block-` + itoa(i) + `"]`
		if !strings.Contains(got, want) {
			t.Errorf("expected region open tag %q in rendered view", want)
		}
	}
	if !strings.Contains(got, `[""]`) {
		t.Errorf("expected region close tag in rendered view")
	}
}

func TestHighlightBlock_OutOfRangeIsNoop(t *testing.T) {
	// Defensive: stale blockIdx (e.g., session re-rendered after a
	// hit was captured) should not panic.
	c := NewChatDisplay(tview.NewTextView(), "test")
	c.blocks = []chatBlock{&userBlock{text: "x"}}
	c.HighlightBlock(-1)
	c.HighlightBlock(99)
}

// itoa is a tiny helper because we can't import strconv via the
// constraints of the test file's existing imports without churn.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}
