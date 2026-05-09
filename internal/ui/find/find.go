// SPDX-License-Identifier: AGPL-3.0-or-later

// Package find is the substring/regex search engine used by /find and
// /grep. Callers pass in a []Source describing what's searchable —
// typically derived from the chat's block model — and receive []Hit
// with role and match positions. Snippet construction is the caller's
// job (so output styling stays in the renderer).
package find

import (
	"regexp"
	"strings"
)

// Source is one searchable piece of text within the chat. Backends
// build []Source by walking their block model: a tool block typically
// produces two sources (call signature + accumulated output), a user
// or assistant block produces one. Role disambiguates results in the
// UI.
type Source struct {
	Role string // "user" / "assistant" / "tool" / "tool-output" / "reasoning" / "error"
	Text string
}

// Hit is one match. SourceIdx points back at the caller's []Source so
// the UI can map a hit to the originating chat block. Start/End are
// byte offsets into Source.Text — callers use these to build snippets.
type Hit struct {
	SourceIdx int
	Role      string
	Start     int
	End       int
}

// Search walks `sources` and returns every match for `query`. Substring
// matches are case-insensitive; regex matches honour the pattern
// verbatim (use (?i) for case-insensitive there). Empty query returns
// (nil, nil); invalid regex surfaces as an error so the caller can
// display it.
//
// One source can yield multiple hits — long bash output frequently
// contains the same word many times, and surfacing each occurrence
// separately is more useful than a single "matched" marker.
func Search(sources []Source, query string, useRegex bool) ([]Hit, error) {
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

	var hits []Hit
	for i, src := range sources {
		hits = append(hits, scan(i, src.Role, src.Text, query, re)...)
	}
	return hits, nil
}

func scan(sourceIdx int, role, text, query string, re *regexp.Regexp) []Hit {
	if text == "" {
		return nil
	}
	if re != nil {
		var hits []Hit
		for _, m := range re.FindAllStringIndex(text, -1) {
			hits = append(hits, Hit{
				SourceIdx: sourceIdx,
				Role:      role,
				Start:     m[0],
				End:       m[1],
			})
		}
		return hits
	}

	// Substring path: case-insensitive scan via lower-cased haystack so
	// we can still cite original-case context in the snippet.
	low := strings.ToLower(text)
	q := strings.ToLower(query)
	var hits []Hit
	from := 0
	for {
		idx := strings.Index(low[from:], q)
		if idx < 0 {
			break
		}
		start := from + idx
		end := start + len(q)
		hits = append(hits, Hit{
			SourceIdx: sourceIdx,
			Role:      role,
			Start:     start,
			End:       end,
		})
		from = end
		if from >= len(text) {
			break
		}
	}
	return hits
}
