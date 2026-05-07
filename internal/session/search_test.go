// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/TaraTheStar/enso/internal/llm"
)

func TestSearch_CwdFilterDefault(t *testing.T) {
	s := openTestStore(t)

	wA, _ := NewSession(s, "m", "p", "/proj/a")
	_ = wA.AppendMessage(llm.Message{Role: "user", Content: "talking about widgets here"}, "")

	wB, _ := NewSession(s, "m", "p", "/proj/b")
	_ = wB.AppendMessage(llm.Message{Role: "user", Content: "also widgets but in B"}, "")

	hits, err := Search(s, "widgets", "/proj/a", 50)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("want 1 hit scoped to /proj/a, got %d", len(hits))
	}
	if hits[0].SessionID != wA.SessionID() {
		t.Errorf("wrong session: got %s want %s", hits[0].SessionID, wA.SessionID())
	}
}

func TestSearch_AllCwdsWhenEmpty(t *testing.T) {
	s := openTestStore(t)

	wA, _ := NewSession(s, "m", "p", "/proj/a")
	_ = wA.AppendMessage(llm.Message{Role: "user", Content: "hello widgets"}, "")
	wB, _ := NewSession(s, "m", "p", "/proj/b")
	_ = wB.AppendMessage(llm.Message{Role: "user", Content: "more widgets"}, "")

	hits, err := Search(s, "widgets", "", 50)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("want 2 hits across cwds, got %d", len(hits))
	}
}

func TestSearch_ExcludesSubAgentRows(t *testing.T) {
	s := openTestStore(t)

	w, _ := NewSession(s, "m", "p", "/proj/x")
	_ = w.AppendMessage(llm.Message{Role: "user", Content: "top-level mentions widgets"}, "")
	_ = w.AppendMessage(llm.Message{Role: "user", Content: "sub mentions widgets"}, "sub-1")

	hits, err := Search(s, "widgets", "", 50)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("want 1 top-level hit, got %d", len(hits))
	}
	if !strings.HasPrefix(hits[0].Snippet, "top-level") {
		t.Errorf("got sub-agent row: %q", hits[0].Snippet)
	}
}

func TestSearch_OrderByUpdatedAtDesc(t *testing.T) {
	s := openTestStore(t)

	wOld, _ := NewSession(s, "m", "p", "/proj/x")
	_ = wOld.AppendMessage(llm.Message{Role: "user", Content: "first widgets"}, "")
	// Backdate the older session.
	if _, err := s.DB.Exec(`UPDATE sessions SET updated_at = 100 WHERE id = ?`, wOld.SessionID()); err != nil {
		t.Fatal(err)
	}

	wNew, _ := NewSession(s, "m", "p", "/proj/x")
	_ = wNew.AppendMessage(llm.Message{Role: "user", Content: "second widgets"}, "")
	if _, err := s.DB.Exec(`UPDATE sessions SET updated_at = 200 WHERE id = ?`, wNew.SessionID()); err != nil {
		t.Fatal(err)
	}

	hits, err := Search(s, "widgets", "", 50)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("want 2 hits, got %d", len(hits))
	}
	if hits[0].SessionID != wNew.SessionID() {
		t.Errorf("newer session must come first")
	}
}

func TestSearch_CaseInsensitive(t *testing.T) {
	s := openTestStore(t)
	w, _ := NewSession(s, "m", "p", "/proj/x")
	_ = w.AppendMessage(llm.Message{Role: "user", Content: "Hello WIDGETS world"}, "")

	hits, err := Search(s, "widgets", "", 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("want case-insensitive hit, got %d", len(hits))
	}
}

func TestSearch_LikeWildcardsTreatedLiterally(t *testing.T) {
	s := openTestStore(t)
	w, _ := NewSession(s, "m", "p", "/proj/x")
	_ = w.AppendMessage(llm.Message{Role: "user", Content: "literal % sign here"}, "")
	_ = w.AppendMessage(llm.Message{Role: "user", Content: "no percent"}, "")

	hits, err := Search(s, "%", "", 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("LIKE %% must be escaped: got %d hits", len(hits))
	}
}

func TestMakeSnippet_CentersMatchAndCollapsesWhitespace(t *testing.T) {
	src := strings.Repeat("a", 100) + " needle " + strings.Repeat("b", 100)
	snip := makeSnippet(src, "needle", 10)
	if !strings.Contains(snip, "needle") {
		t.Fatalf("snippet missing needle: %q", snip)
	}
	if !strings.HasPrefix(snip, "…") || !strings.HasSuffix(snip, "…") {
		t.Errorf("expected ellipsis on both sides: %q", snip)
	}
	src2 := "with\nembedded\nnewlines\nneedle\nand\nmore"
	snip2 := makeSnippet(src2, "needle", 40)
	if strings.Contains(snip2, "\n") {
		t.Errorf("newlines should be collapsed: %q", snip2)
	}
}

func TestSearch_EmptyPatternErrors(t *testing.T) {
	s := openTestStore(t)
	if _, err := Search(s, "  ", "", 10); err == nil {
		t.Fatal("expected error on empty pattern")
	}
}

func TestSearchRegex_BasicAndCwdFilter(t *testing.T) {
	s := openTestStore(t)

	wA, _ := NewSession(s, "m", "p", "/proj/a")
	_ = wA.AppendMessage(llm.Message{Role: "user", Content: "error code 4221 fired"}, "")
	_ = wA.AppendMessage(llm.Message{Role: "user", Content: "no number here"}, "")

	wB, _ := NewSession(s, "m", "p", "/proj/b")
	_ = wB.AppendMessage(llm.Message{Role: "user", Content: "error code 9001 fired"}, "")

	re := regexp.MustCompile(`error code \d+`)
	hits, err := SearchRegex(s, re, "/proj/a", 50)
	if err != nil {
		t.Fatalf("regex: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("want 1 hit scoped to /proj/a, got %d", len(hits))
	}
	if !strings.Contains(hits[0].Snippet, "4221") {
		t.Errorf("snippet missing match: %q", hits[0].Snippet)
	}

	all, err := SearchRegex(s, re, "", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("want 2 hits across cwds, got %d", len(all))
	}
}

func TestSearchRegex_ExcludesSubAgentRows(t *testing.T) {
	s := openTestStore(t)
	w, _ := NewSession(s, "m", "p", "/proj/x")
	_ = w.AppendMessage(llm.Message{Role: "user", Content: "top alpha-1"}, "")
	_ = w.AppendMessage(llm.Message{Role: "user", Content: "sub alpha-2"}, "sub-1")

	hits, err := SearchRegex(s, regexp.MustCompile(`alpha-\d`), "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || !strings.HasPrefix(hits[0].Snippet, "top") {
		t.Fatalf("expected top-level only, got %+v", hits)
	}
}

func TestSearchRegex_LimitRespected(t *testing.T) {
	s := openTestStore(t)
	w, _ := NewSession(s, "m", "p", "/proj/x")
	for i := 0; i < 10; i++ {
		_ = w.AppendMessage(llm.Message{Role: "user", Content: "match"}, "")
	}
	hits, err := SearchRegex(s, regexp.MustCompile(`match`), "", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 3 {
		t.Fatalf("want 3 (limit), got %d", len(hits))
	}
}

func TestSearchRegex_NilErrors(t *testing.T) {
	s := openTestStore(t)
	if _, err := SearchRegex(s, nil, "", 10); err == nil {
		t.Fatal("expected error on nil regexp")
	}
}

// Messages bigger than regexScanCap are scanned only over their head.
// A match in the head still hits (and Truncated is true so the UI can
// flag it); a match buried only in the tail is missed by design.
func TestSearchRegex_TruncatesLargeContent(t *testing.T) {
	s := openTestStore(t)
	w, _ := NewSession(s, "m", "p", "/proj/big")

	// Construct a > regexScanCap message: known head + lots of filler +
	// a tail-only marker.
	headMatch := "HEAD-MATCH-AAA"
	tailMatch := "TAIL-MATCH-ZZZ"
	filler := strings.Repeat("x", regexScanCap)
	body := headMatch + filler + tailMatch
	if err := w.AppendMessage(llm.Message{Role: "user", Content: body}, ""); err != nil {
		t.Fatal(err)
	}

	hitsHead, err := SearchRegex(s, regexp.MustCompile(headMatch), "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hitsHead) != 1 {
		t.Fatalf("head match not found: %d hits", len(hitsHead))
	}
	if !hitsHead[0].Truncated {
		t.Errorf("Truncated should be true on oversize message")
	}

	hitsTail, err := SearchRegex(s, regexp.MustCompile(tailMatch), "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hitsTail) != 0 {
		t.Errorf("tail match should be missed by cap, got %d hits", len(hitsTail))
	}
}

// Sub-cap messages are not flagged as truncated.
func TestSearchRegex_TruncatedFalseOnSmall(t *testing.T) {
	s := openTestStore(t)
	w, _ := NewSession(s, "m", "p", "/proj/small")
	_ = w.AppendMessage(llm.Message{Role: "user", Content: "find SMALLMARKER here"}, "")

	hits, err := SearchRegex(s, regexp.MustCompile("SMALLMARKER"), "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("want 1 hit, got %d", len(hits))
	}
	if hits[0].Truncated {
		t.Errorf("Truncated should be false on small message")
	}
}

func TestListRecentWithStats_CountsAndTokens(t *testing.T) {
	s := openTestStore(t)

	w, _ := NewSession(s, "m", "p", "/proj/x")
	_ = w.AppendMessage(llm.Message{Role: "user", Content: strings.Repeat("a", 40)}, "")      // 10 tok
	_ = w.AppendMessage(llm.Message{Role: "assistant", Content: strings.Repeat("b", 40)}, "") // 10 tok
	_ = w.AppendMessage(llm.Message{Role: "user", Content: "sub-agent talk"}, "sub-1")        // excluded

	got, err := ListRecentWithStats(s, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 row, got %d", len(got))
	}
	if got[0].MessageCount != 2 {
		t.Errorf("msg count: want 2 (top-level only), got %d", got[0].MessageCount)
	}
	if got[0].ApproxTokens != 20 {
		t.Errorf("tokens: want ~20, got %d", got[0].ApproxTokens)
	}
}

func TestListRecentWithStats_EmptySessionFilteredOut(t *testing.T) {
	// Sessions with no messages are skipped: there's nothing to resume,
	// and the most common cause is a launch+immediate-quit. See the
	// HAVING msg_count > 0 clause in ListRecentWithStats.
	s := openTestStore(t)
	_, _ = NewSession(s, "m", "p", "/proj/x")

	got, err := ListRecentWithStats(s, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("want 0 rows (empty session filtered), got %d", len(got))
	}
}

func TestListRecentWithStats_OrderedDescByUpdatedAt(t *testing.T) {
	s := openTestStore(t)
	wA, _ := NewSession(s, "m", "p", "/a")
	wB, _ := NewSession(s, "m", "p", "/b")
	// Both need at least one message to clear the empty-session filter.
	_ = wA.AppendMessage(llm.Message{Role: "user", Content: "hi from A"}, "")
	_ = wB.AppendMessage(llm.Message{Role: "user", Content: "hi from B"}, "")
	if _, err := s.DB.Exec(`UPDATE sessions SET updated_at = 100 WHERE id = ?`, wA.SessionID()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`UPDATE sessions SET updated_at = 200 WHERE id = ?`, wB.SessionID()); err != nil {
		t.Fatal(err)
	}
	got, err := ListRecentWithStats(s, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != wB.SessionID() {
		t.Errorf("expected newer first, got %+v", got)
	}
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	tmp := t.TempDir()
	s, err := OpenAt(filepath.Join(tmp, "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
