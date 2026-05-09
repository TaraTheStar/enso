// SPDX-License-Identifier: AGPL-3.0-or-later

package find

import "testing"

func TestSearch_SubstringCaseInsensitive(t *testing.T) {
	srcs := []Source{
		{Role: "user", Text: "Find the FOO in this Foo and that foo."},
	}
	hits, err := Search(srcs, "foo", false)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(hits) != 3 {
		t.Fatalf("hits=%d, want 3 (case-insensitive substring)", len(hits))
	}
	for _, h := range hits {
		if h.Role != "user" {
			t.Errorf("role=%q, want 'user'", h.Role)
		}
		if h.SourceIdx != 0 {
			t.Errorf("SourceIdx=%d, want 0", h.SourceIdx)
		}
	}
}

func TestSearch_RegexMatchesAllHits(t *testing.T) {
	srcs := []Source{
		{Role: "assistant", Text: "ID 12 then ID 345 then ID 6789"},
	}
	hits, err := Search(srcs, `ID \d+`, true)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(hits) != 3 {
		t.Errorf("hits=%d, want 3 regex matches", len(hits))
	}
}

func TestSearch_InvalidRegexErrors(t *testing.T) {
	srcs := []Source{{Role: "user", Text: "anything"}}
	if _, err := Search(srcs, "(unclosed", true); err == nil {
		t.Error("expected error from invalid regex")
	}
}

func TestSearch_EmptyQueryReturnsNil(t *testing.T) {
	srcs := []Source{{Role: "user", Text: "hello world"}}
	hits, err := Search(srcs, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if hits != nil {
		t.Errorf("empty query should return nil, got %v", hits)
	}
}

func TestSearch_SourceIdxPointsAtSlice(t *testing.T) {
	srcs := []Source{
		{Role: "user", Text: "header line"},
		{Role: "assistant", Text: "the needle is here"},
		{Role: "user", Text: "needle again"},
	}
	hits, err := Search(srcs, "needle", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("hits=%d, want 2", len(hits))
	}
	if hits[0].SourceIdx != 1 {
		t.Errorf("first hit SourceIdx=%d, want 1", hits[0].SourceIdx)
	}
	if hits[1].SourceIdx != 2 {
		t.Errorf("second hit SourceIdx=%d, want 2", hits[1].SourceIdx)
	}
}

func TestSearch_HitPositions(t *testing.T) {
	// Match positions are byte offsets into Source.Text — caller uses
	// them to build snippets in its own markup style.
	srcs := []Source{
		{Role: "user", Text: "the quick brown fox"},
	}
	hits, err := Search(srcs, "brown", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("hits=%d, want 1", len(hits))
	}
	if hits[0].Start != 10 || hits[0].End != 15 {
		t.Errorf("got Start=%d End=%d, want 10/15", hits[0].Start, hits[0].End)
	}
}

func TestSearch_EmptySourceTextSkipped(t *testing.T) {
	srcs := []Source{
		{Role: "user", Text: ""},
		{Role: "assistant", Text: "needle here"},
	}
	hits, err := Search(srcs, "needle", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("hits=%d, want 1", len(hits))
	}
	if hits[0].SourceIdx != 1 {
		t.Errorf("hit at empty source: %v", hits[0])
	}
}
