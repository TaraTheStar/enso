// SPDX-License-Identifier: AGPL-3.0-or-later

package tools

import (
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"strings"
	"sync/atomic"
)

// CompressionStats accumulates the tokens saved this session by output
// compression + truncation. AgentContext carries one; the bash/read paths
// add to it as they trim output, and /context surfaces the running total
// (H11/R7). The zero value and a nil pointer are both safe to call.
type CompressionStats struct {
	saved atomic.Int64
}

// Add records n saved tokens (no-op for n <= 0 or a nil receiver).
func (c *CompressionStats) Add(n int) {
	if c == nil || n <= 0 {
		return
	}
	c.saved.Add(int64(n))
}

// Saved returns the cumulative tokens saved.
func (c *CompressionStats) Saved() int64 {
	if c == nil {
		return 0
	}
	return c.saved.Load()
}

// Structural, type-aware output compressors (H5). Where the declarative
// filters (filter.go) key off the *command*, these key off the *shape* of
// the content — unified diffs, repetitive logs, big JSON arrays — and are
// applied to any command output a filter didn't already handle. Each is
// pure Go, conservative (never invents content), and reversible: the raw
// output is still spilled to disk, so the model can `read` back anything a
// compressor dropped.
//
// Every compressor returns (out, changed). The orchestrator
// (compressOutput) only adopts a result that is actually smaller in tokens
// (H3 — never let a "compression" inflate the prompt).

// estTokens approximates the token cost of a string with the same
// 4-chars-per-token heuristic used by llm.Estimate, kept local so the hot
// tool path doesn't import internal/llm just for this.
func estTokens(s string) int { return len(s) / 4 }

// compressOutput is the single entry point used by the bash path. It tries,
// in order: the command-keyed declarative filter, then a content-shape
// structural compressor. Whichever first produces a smaller (in tokens)
// result wins; if neither helps, raw is returned unchanged. Returns the
// (possibly unchanged) text and the number of tokens saved versus raw.
func compressOutput(fs *FilterSet, cmd, raw string) (out string, savedTokens int) {
	rawTokens := estTokens(raw)

	if filtered, changed := fs.Apply(cmd, raw); changed {
		if estTokens(filtered) < rawTokens {
			return filtered, rawTokens - estTokens(filtered)
		}
	}

	if structural, changed := structuralCompress(raw); changed {
		if estTokens(structural) < rawTokens {
			return structural, rawTokens - estTokens(structural)
		}
	}

	return raw, 0
}

// structuralCompress dispatches on the detected content shape. Returns
// (out, changed). Detection is cheap and conservative — an ambiguous input
// falls through unchanged rather than risk mangling it.
func structuralCompress(s string) (string, bool) {
	switch detectShape(s) {
	case shapeDiff:
		return compressDiff(s)
	case shapeJSONArray:
		return compressJSONArray(s)
	default:
		return compressLog(s)
	}
}

type contentShape int

const (
	shapeUnknown contentShape = iota
	shapeDiff
	shapeJSONArray
)

func detectShape(s string) contentShape {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return shapeUnknown
	}
	if strings.HasPrefix(trimmed, "diff --git ") || strings.Contains(s, "\ndiff --git ") {
		return shapeDiff
	}
	// Unified diff without git headers: a "--- " / "+++ " pair followed by
	// an @@ hunk marker.
	if strings.HasPrefix(trimmed, "--- ") && strings.Contains(s, "\n+++ ") && strings.Contains(s, "\n@@ ") {
		return shapeDiff
	}
	if strings.HasPrefix(trimmed, "[") {
		return shapeJSONArray
	}
	return shapeUnknown
}

// --- diff compression -------------------------------------------------

// lockfiles are dependency-pin files whose diffs are pure churn the model
// rarely needs line-by-line. Their hunks are collapsed to a one-line
// summary. Matched by base name.
var lockfiles = map[string]bool{
	"go.sum":            true,
	"package-lock.json": true,
	"pnpm-lock.yaml":    true,
	"yarn.lock":         true,
	"Cargo.lock":        true,
	"poetry.lock":       true,
	"composer.lock":     true,
	"Gemfile.lock":      true,
	"flake.lock":        true,
}

var gitDiffHeaderRe = regexp.MustCompile(`^diff --git a/(.+?) b/(.+)$`)

// compressDiff collapses lockfile file-sections to a summary and elides
// whitespace-only hunks in other files. Header lines and substantive hunks
// are preserved verbatim.
func compressDiff(s string) (string, bool) {
	lines := strings.Split(s, "\n")
	var out []string
	changed := false

	i := 0
	for i < len(lines) {
		m := gitDiffHeaderRe.FindStringSubmatch(lines[i])
		if m == nil {
			out = append(out, lines[i])
			i++
			continue
		}
		// Collect this file's section [i, j).
		j := i + 1
		for j < len(lines) && !strings.HasPrefix(lines[j], "diff --git ") {
			j++
		}
		section := lines[i:j]
		file := m[2]
		base := path.Base(file)
		if lockfiles[base] {
			added, removed := countDiffChanges(section)
			out = append(out, lines[i])
			out = append(out, fmt.Sprintf("[lockfile diff elided: %s, +%d/-%d lines — recover from full output if needed]", file, added, removed))
			changed = true
		} else {
			compactedSection, secChanged := compactHunks(section)
			out = append(out, compactedSection...)
			if secChanged {
				changed = true
			}
		}
		i = j
	}

	if !changed {
		return s, false
	}
	return strings.Join(out, "\n"), true
}

func countDiffChanges(section []string) (added, removed int) {
	for _, ln := range section {
		switch {
		case strings.HasPrefix(ln, "+++") || strings.HasPrefix(ln, "---"):
			// header, not a change
		case strings.HasPrefix(ln, "+"):
			added++
		case strings.HasPrefix(ln, "-"):
			removed++
		}
	}
	return added, removed
}

// compactHunks walks a single file's diff section and replaces any
// whitespace-only hunk body with a placeholder, keeping the @@ header.
func compactHunks(section []string) ([]string, bool) {
	var out []string
	changed := false
	i := 0
	for i < len(section) {
		if !strings.HasPrefix(section[i], "@@") {
			out = append(out, section[i])
			i++
			continue
		}
		// Hunk runs from this @@ line to the next @@ (or section end).
		hdr := section[i]
		j := i + 1
		for j < len(section) && !strings.HasPrefix(section[j], "@@") && !strings.HasPrefix(section[j], "diff --git ") {
			j++
		}
		body := section[i+1 : j]
		if hunkWhitespaceOnly(body) {
			out = append(out, hdr, "[whitespace-only hunk elided]")
			changed = true
		} else {
			out = append(out, hdr)
			out = append(out, body...)
		}
		i = j
	}
	return out, changed
}

// hunkWhitespaceOnly reports whether a hunk's added and removed lines differ
// only in whitespace — i.e. the multiset of removed lines equals the
// multiset of added lines after collapsing all whitespace. A hunk with only
// additions or only removals is NOT whitespace-only (real content changed).
func hunkWhitespaceOnly(body []string) bool {
	removed := map[string]int{}
	added := map[string]int{}
	sawChange := false
	for _, ln := range body {
		if ln == "" {
			continue
		}
		switch ln[0] {
		case '-':
			removed[collapseWS(ln[1:])]++
			sawChange = true
		case '+':
			added[collapseWS(ln[1:])]++
			sawChange = true
		}
	}
	if !sawChange {
		return false
	}
	if len(removed) != len(added) {
		return false
	}
	for k, v := range removed {
		if added[k] != v {
			return false
		}
	}
	return true
}

func collapseWS(s string) string {
	return strings.Join(strings.Fields(s), "")
}

// --- log template collapse -------------------------------------------

// numRe / hexRe / quoteRe mask the variable parts of a log line so two
// lines that differ only in those parts share a template.
var (
	logNumRe   = regexp.MustCompile(`[0-9]+`)
	logHexRe   = regexp.MustCompile(`\b[0-9a-fA-F]{8,}\b`)
	logQuoteRe = regexp.MustCompile(`"[^"]*"`)
)

// compressLog collapses runs of consecutive lines sharing a template (after
// masking numbers/hashes/quoted strings) into a single representative line
// annotated with the repeat count. Order-preserving; only runs of length
// >= 3 are collapsed so ordinary output is untouched.
func compressLog(s string) (string, bool) {
	lines := strings.Split(s, "\n")
	if len(lines) < 6 {
		return s, false
	}
	var out []string
	changed := false
	i := 0
	for i < len(lines) {
		tmpl := logTemplate(lines[i])
		j := i + 1
		for j < len(lines) && logTemplate(lines[j]) == tmpl {
			j++
		}
		runLen := j - i
		if tmpl != "" && runLen >= 3 {
			out = append(out, lines[i])
			out = append(out, fmt.Sprintf("... %d more lines matching this pattern elided ...", runLen-1))
			changed = true
		} else {
			out = append(out, lines[i:j]...)
		}
		i = j
	}
	if !changed {
		return s, false
	}
	return strings.Join(out, "\n"), true
}

func logTemplate(line string) string {
	if strings.TrimSpace(line) == "" {
		return ""
	}
	t := logQuoteRe.ReplaceAllString(line, `"<S>"`)
	t = logHexRe.ReplaceAllString(t, "<H>")
	t = logNumRe.ReplaceAllString(t, "<N>")
	return t
}

// --- JSON array sampling ---------------------------------------------

// jsonArrayThreshold is the element count above which a JSON array is
// sampled (head + tail) rather than shown whole.
const jsonArrayThreshold = 40

// jsonArraySampleEnds is how many elements to keep from each end.
const jsonArraySampleEnds = 10

// compressJSONArray, for a large top-level JSON array, keeps the first and
// last few elements and notes how many were elided. Re-serialised
// compactly. Non-array or small-array input is returned unchanged.
func compressJSONArray(s string) (string, bool) {
	var arr []json.RawMessage
	if err := json.Unmarshal([]byte(s), &arr); err != nil {
		return s, false
	}
	if len(arr) <= jsonArrayThreshold {
		return s, false
	}
	elided := len(arr) - 2*jsonArraySampleEnds
	if elided <= 0 {
		return s, false
	}
	var b strings.Builder
	b.WriteString("[\n")
	for k := 0; k < jsonArraySampleEnds; k++ {
		b.WriteString("  ")
		b.Write(arr[k])
		b.WriteString(",\n")
	}
	fmt.Fprintf(&b, "  \"... %d array elements elided (recover from full output) ...\",\n", elided)
	for k := len(arr) - jsonArraySampleEnds; k < len(arr); k++ {
		b.WriteString("  ")
		b.Write(arr[k])
		if k < len(arr)-1 {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}
	b.WriteString("]")
	return b.String(), true
}
