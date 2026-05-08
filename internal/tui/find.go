// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"regexp"
	"strings"
)

// findHit is one match in the active chat. blockIdx points back into
// ChatDisplay.blocks so the host can drive ScrollToHighlight on the
// region tag this block emits during redraw.
type findHit struct {
	blockIdx int
	role     string // "user" / "assistant" / "tool" / "tool-output" / "reasoning" / "error"
	snippet  string // single-line excerpt with the matched run highlighted via tview color tags
}

// findInBlocks walks the chat's block model and returns every match
// for `query`. Substring matches are case-insensitive; regex matches
// honour the pattern verbatim (use (?i) for case-insensitive there).
//
// One block can yield multiple hits — long bash output frequently
// contains the same word many times, and surfacing each occurrence
// separately is more useful than a single "matched" marker.
func findInBlocks(blocks []chatBlock, query string, useRegex bool) ([]findHit, error) {
	if query == "" {
		return nil, nil
	}

	var re *regexp.Regexp
	if useRegex {
		var err error
		re, err = regexp.Compile(query)
		if err != nil {
			return nil, err
		}
	}

	var hits []findHit
	for i, b := range blocks {
		for _, src := range searchableTexts(b) {
			hits = append(hits, scanText(i, src.role, src.text, query, re)...)
		}
	}
	return hits, nil
}

// blockSource is one searchable region within a block. A toolBlock
// produces two: the call signature and the accumulated output, each
// with its own role label so the result list can disambiguate.
type blockSource struct {
	role string
	text string
}

func searchableTexts(b chatBlock) []blockSource {
	switch v := b.(type) {
	case *userBlock:
		return []blockSource{{"user", v.text}}
	case *assistantBlock:
		return []blockSource{{"assistant", v.text}}
	case *toolBlock:
		out := []blockSource{{"tool", v.call}}
		if v.output != "" {
			out = append(out, blockSource{"tool-output", v.output})
		}
		return out
	case *reasoningBlock:
		return []blockSource{{"reasoning", v.text}}
	case *errorBlock:
		return []blockSource{{"error", v.msg}}
	}
	// assistantHeaderBlock, cancelledBlock, compactedBlock — no text.
	return nil
}

func scanText(blockIdx int, role, text, query string, re *regexp.Regexp) []findHit {
	if text == "" {
		return nil
	}
	if re != nil {
		var hits []findHit
		for _, m := range re.FindAllStringIndex(text, -1) {
			hits = append(hits, findHit{
				blockIdx: blockIdx,
				role:     role,
				snippet:  buildSnippet(text, m[0], m[1]),
			})
		}
		return hits
	}

	// Substring path: case-insensitive scan via lower-cased haystack so
	// we can still cite original-case context in the snippet.
	low := strings.ToLower(text)
	q := strings.ToLower(query)
	var hits []findHit
	from := 0
	for {
		idx := strings.Index(low[from:], q)
		if idx < 0 {
			break
		}
		start := from + idx
		end := start + len(q)
		hits = append(hits, findHit{
			blockIdx: blockIdx,
			role:     role,
			snippet:  buildSnippet(text, start, end),
		})
		from = end
		if from >= len(text) {
			break
		}
	}
	return hits
}

// snippetContext is how many bytes of context to surface on each side
// of a match. Wide enough to read the match in context, narrow enough
// to keep the result list dense.
const snippetContext = 30

// buildSnippet returns "...prefix [yellow]match[-] suffix..." with
// the matched range visually highlighted. Newlines collapsed to spaces
// so each list row stays single-line.
func buildSnippet(text string, matchStart, matchEnd int) string {
	start := matchStart - snippetContext
	if start < 0 {
		start = 0
	}
	end := matchEnd + snippetContext
	if end > len(text) {
		end = len(text)
	}
	prefix := strings.ReplaceAll(text[start:matchStart], "\n", " ")
	match := strings.ReplaceAll(text[matchStart:matchEnd], "\n", " ")
	suffix := strings.ReplaceAll(text[matchEnd:end], "\n", " ")

	var sb strings.Builder
	if start > 0 {
		sb.WriteString("…")
	}
	sb.WriteString(prefix)
	sb.WriteString("[yellow]")
	sb.WriteString(match)
	sb.WriteString("[-]")
	sb.WriteString(suffix)
	if end < len(text) {
		sb.WriteString("…")
	}
	return sb.String()
}
