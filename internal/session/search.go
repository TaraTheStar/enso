// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// regexScanCap caps how much of any single message content is scanned
// by SearchRegex. Tool results (file reads, web fetches) can be 200 KiB+
// each; without a cap, a `/grep --regex foo` over a backloaded store
// blocks the TUI goroutine for seconds while RE2 chews through every
// row. 256 KiB covers the common "read this big file" case and leaves
// per-message worst-case in the millisecond range. Substring Search
// (SQLite LIKE, server-side) doesn't need a cap.
const regexScanCap = 256 * 1024

// Hit is one matching message returned by Search. SessionID + UpdatedAt
// identify the session; Snippet is a windowed substring of the matched
// message content with the match centred.
type Hit struct {
	SessionID   string
	UpdatedAt   time.Time
	Cwd         string
	Interrupted bool
	Role        string
	Snippet     string
	// Truncated is true when SearchRegex scanned only the head of a
	// message that was longer than regexScanCap. The match (and snippet)
	// are accurate within that window, but a match that lived in the
	// truncated tail wouldn't have been found. Surfaced in the TUI grep
	// overlay so the user knows to widen / refine the query.
	Truncated bool
}

// Search returns messages whose content matches `pattern` (case-insensitive
// substring). When `cwd` is non-empty, only sessions whose `cwd` column
// equals it are considered; pass "" to search every session in the store.
// Sub-agent rows are excluded (agent_id = ”) so results align with the
// top-level resume view.
//
// Results are ordered by session updated_at DESC, then by message seq ASC
// inside each session, capped at `limit` rows. limit <= 0 disables the cap.
func Search(s *Store, pattern, cwd string, limit int) ([]Hit, error) {
	if strings.TrimSpace(pattern) == "" {
		return nil, fmt.Errorf("empty pattern")
	}

	args := []any{}
	q := strings.Builder{}
	q.WriteString(`SELECT m.session_id, m.role, m.content, s.updated_at, s.cwd, s.interrupted
		FROM messages m JOIN sessions s ON s.id = m.session_id
		WHERE m.agent_id = '' AND m.content LIKE ? ESCAPE '\' COLLATE NOCASE`)
	args = append(args, "%"+escapeLike(pattern)+"%")
	if cwd != "" {
		q.WriteString(` AND s.cwd = ?`)
		args = append(args, cwd)
	}
	q.WriteString(` ORDER BY s.updated_at DESC, m.seq ASC`)
	if limit > 0 {
		q.WriteString(` LIMIT ?`)
		args = append(args, limit)
	}

	rows, err := s.DB.Query(q.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	defer rows.Close()

	var out []Hit
	for rows.Next() {
		var h Hit
		var content string
		var updated int64
		var inter int
		if err := rows.Scan(&h.SessionID, &h.Role, &content, &updated, &h.Cwd, &inter); err != nil {
			return nil, fmt.Errorf("scan hit: %w", err)
		}
		h.UpdatedAt = time.Unix(updated, 0)
		h.Interrupted = inter != 0
		h.Snippet = makeSnippet(content, pattern, 40)
		out = append(out, h)
	}
	return out, rows.Err()
}

// SearchRegex is the regex variant of Search. It scans every top-level
// message in the cwd-filtered set and applies `re` Go-side (SQLite has no
// native REGEXP without a custom function). At enso's typical scale
// (single-digit thousand messages) this is fine; revisit if it stops being.
//
// Per-message content is capped at regexScanCap bytes — anything longer
// is scanned only over its head, with Hit.Truncated set so the TUI can
// flag it. This keeps the synchronous TUI-side call from freezing on
// stores that contain large `read`/`web_fetch` tool-result rows.
//
// Ordering and limit semantics match Search.
func SearchRegex(s *Store, re *regexp.Regexp, cwd string, limit int) ([]Hit, error) {
	if re == nil {
		return nil, fmt.Errorf("nil regexp")
	}

	args := []any{}
	q := strings.Builder{}
	q.WriteString(`SELECT m.session_id, m.role, m.content, s.updated_at, s.cwd, s.interrupted
		FROM messages m JOIN sessions s ON s.id = m.session_id
		WHERE m.agent_id = ''`)
	if cwd != "" {
		q.WriteString(` AND s.cwd = ?`)
		args = append(args, cwd)
	}
	q.WriteString(` ORDER BY s.updated_at DESC, m.seq ASC`)

	rows, err := s.DB.Query(q.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("search regex: %w", err)
	}
	defer rows.Close()

	var out []Hit
	for rows.Next() {
		var h Hit
		var content string
		var updated int64
		var inter int
		if err := rows.Scan(&h.SessionID, &h.Role, &content, &updated, &h.Cwd, &inter); err != nil {
			return nil, fmt.Errorf("scan hit: %w", err)
		}
		scan, truncated := capForRegex(content)
		loc := re.FindStringIndex(scan)
		if loc == nil {
			continue
		}
		h.UpdatedAt = time.Unix(updated, 0)
		h.Interrupted = inter != 0
		h.Snippet = makeSnippetAt(scan, loc[0], loc[1], 40)
		h.Truncated = truncated
		out = append(out, h)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, rows.Err()
}

// capForRegex returns content unchanged if it fits in regexScanCap, or
// the head of it backed off to a UTF-8 rune-start boundary so we don't
// hand RE2 a partial multibyte sequence at the tail (would cause
// spurious match misses on the last few bytes). Second return is true
// when truncation happened.
func capForRegex(content string) (string, bool) {
	if len(content) <= regexScanCap {
		return content, false
	}
	scan := content[:regexScanCap]
	for len(scan) > 0 && !utf8.RuneStart(scan[len(scan)-1]) {
		scan = scan[:len(scan)-1]
	}
	return scan, true
}

// makeSnippetAt is the offset-based variant of makeSnippet, used by
// SearchRegex which already knows where the match landed.
func makeSnippetAt(content string, start, end, pad int) string {
	from := start - pad
	if from < 0 {
		from = 0
	}
	to := end + pad
	if to > len(content) {
		to = len(content)
	}
	prefix := ""
	if from > 0 {
		prefix = "…"
	}
	suffix := ""
	if to < len(content) {
		suffix = "…"
	}
	return prefix + collapseWhitespace(content[from:to]) + suffix
}

// escapeLike escapes the LIKE wildcards `%` and `_` (and the `\` escape
// character itself) so user input is matched literally.
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// makeSnippet returns up to `pad` chars on each side of the first
// case-insensitive match of `pattern` in `content`, with newlines
// collapsed to spaces. Prefix/suffix ellipsis indicate truncation.
func makeSnippet(content, pattern string, pad int) string {
	idx := strings.Index(strings.ToLower(content), strings.ToLower(pattern))
	if idx < 0 {
		// LIKE matched but our literal scan didn't — return a
		// best-effort head.
		return collapseWhitespace(truncateRunes(content, 2*pad))
	}
	start := idx - pad
	if start < 0 {
		start = 0
	}
	end := idx + len(pattern) + pad
	if end > len(content) {
		end = len(content)
	}
	prefix := ""
	if start > 0 {
		prefix = "…"
	}
	suffix := ""
	if end < len(content) {
		suffix = "…"
	}
	return prefix + collapseWhitespace(content[start:end]) + suffix
}

func collapseWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func truncateRunes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}
