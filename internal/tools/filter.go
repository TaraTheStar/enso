// SPDX-License-Identifier: AGPL-3.0-or-later

package tools

import (
	"fmt"
	"regexp"
	"strings"

	toml "github.com/pelletier/go-toml/v2"
)

// Declarative command-output filters (R2, adapted from rtk's TOML filter
// format — see NOTICE). A filter matches a bash command by regex and then
// rewrites that command's output by stripping noise lines, keeping only
// signal lines, capping the line count, or collapsing an all-noise result
// to a one-line summary. The point is to add coverage for a new toolchain
// without recompiling: drop a *.toml under $XDG_CONFIG_HOME/enso/filters/.
//
// Filters run BEFORE the byte/line output caps (capTruncate). They reduce
// the structured noise a command emits (passing-test lines, ANSI colour,
// progress bars); the caps remain the dumb backstop after.

// ansiRe matches CSI/SGR escape sequences (colour, cursor moves, progress
// redraws). Stripped first when strip_ansi is set so downstream line
// matching sees clean text.
var ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

// Filter is one declarative command-output rule. Fields mirror rtk's TOML
// schema closely enough that filters can be ported across with only cosmetic
// edits; the matching/compiled regexps are derived in compile().
type Filter struct {
	// Name is a stable identifier. A user filter with the same Name as an
	// embedded default replaces it (last-loader-wins), which is how users
	// override a shipped filter.
	Name string `toml:"name"`
	// MatchCommand is a regexp tested against the full command string. The
	// first filter (in load order) whose MatchCommand matches wins.
	MatchCommand string `toml:"match_command"`
	// StripANSI removes terminal escape sequences before line filtering.
	StripANSI bool `toml:"strip_ansi"`
	// KeepLinesMatching, when non-empty, switches the filter to allowlist
	// mode: only lines matching at least one of these regexps survive
	// (failures-only output for test runners / linters). Applied before
	// StripLinesMatching.
	KeepLinesMatching []string `toml:"keep_lines_matching"`
	// StripLinesMatching drops any line matching at least one of these
	// regexps (denylist mode). Ignored for lines already kept by an
	// allowlist when KeepLinesMatching is set.
	StripLinesMatching []string `toml:"strip_lines_matching"`
	// MaxLines caps the surviving line count with a head/tail elision.
	// 0 disables (the global output cap still applies later).
	MaxLines int `toml:"max_lines"`
	// OnEmpty is substituted when filtering removed every line — turns a
	// "200 passing tests, nothing interesting" result into a one-liner
	// instead of an empty tool message.
	OnEmpty string `toml:"on_empty"`
	// Tests are inline self-tests run at load (RunSelfTests). A shipped
	// filter that fails its own test is a build-time bug; a user filter
	// that fails is skipped with a warning rather than killing startup.
	Tests []FilterTest `toml:"tests"`

	matchRe *regexp.Regexp
	keepRe  []*regexp.Regexp
	stripRe []*regexp.Regexp
}

// FilterTest is an inline assertion shipped alongside a filter. input is
// fed through the filter and the result checked against the expectations.
type FilterTest struct {
	Name              string   `toml:"name"`
	Input             string   `toml:"input"`
	ExpectContains    []string `toml:"expect_contains"`
	ExpectNotContains []string `toml:"expect_not_contains"`
	ExpectEqual       string   `toml:"expect_equal"`
}

// filterFile is the on-disk shape: a TOML document with one or more
// [[filter]] array entries.
type filterFile struct {
	Filters []*Filter `toml:"filter"`
}

// FilterSet is an ordered collection of compiled filters. Match returns the
// first filter whose MatchCommand matches a command; Apply runs it.
type FilterSet struct {
	filters []*Filter
	byName  map[string]int // name -> index in filters, for override-by-name
}

// NewFilterSet returns an empty set. Add appends/overrides; the zero value
// (nil) is safe to call Match/Apply/Covers on (they no-op).
func NewFilterSet() *FilterSet {
	return &FilterSet{byName: map[string]int{}}
}

// parseFilters decodes a TOML document into compiled filters. Returns the
// filters in document order; a malformed regexp or TOML is a hard error so
// the caller can decide whether to skip (user file) or fail (embedded).
func parseFilters(data []byte) ([]*Filter, error) {
	var ff filterFile
	if err := toml.Unmarshal(data, &ff); err != nil {
		return nil, fmt.Errorf("parse filter toml: %w", err)
	}
	for _, f := range ff.Filters {
		if err := f.compile(); err != nil {
			return nil, err
		}
	}
	return ff.Filters, nil
}

// compile builds the regexps from the string fields. Called once at load.
func (f *Filter) compile() error {
	if f.Name == "" {
		return fmt.Errorf("filter: missing name")
	}
	if f.MatchCommand == "" {
		return fmt.Errorf("filter %q: missing match_command", f.Name)
	}
	re, err := regexp.Compile(f.MatchCommand)
	if err != nil {
		return fmt.Errorf("filter %q: bad match_command %q: %w", f.Name, f.MatchCommand, err)
	}
	f.matchRe = re
	f.keepRe = f.keepRe[:0]
	for _, p := range f.KeepLinesMatching {
		r, err := regexp.Compile(p)
		if err != nil {
			return fmt.Errorf("filter %q: bad keep_lines_matching %q: %w", f.Name, p, err)
		}
		f.keepRe = append(f.keepRe, r)
	}
	f.stripRe = f.stripRe[:0]
	for _, p := range f.StripLinesMatching {
		r, err := regexp.Compile(p)
		if err != nil {
			return fmt.Errorf("filter %q: bad strip_lines_matching %q: %w", f.Name, p, err)
		}
		f.stripRe = append(f.stripRe, r)
	}
	return nil
}

// Matches reports whether this filter applies to cmd.
func (f *Filter) Matches(cmd string) bool {
	return f.matchRe != nil && f.matchRe.MatchString(cmd)
}

// Apply rewrites output per this filter's rules. Returns the rewritten text
// and whether it actually changed anything (so callers can skip a no-op).
func (f *Filter) Apply(output string) (string, bool) {
	in := output
	if f.StripANSI {
		in = ansiRe.ReplaceAllString(in, "")
	}

	lines := strings.Split(in, "\n")
	// Preserve a trailing-newline-only artefact of Split: drop a single
	// empty final element so we don't reintroduce a blank tail line.
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}

	kept := make([]string, 0, len(lines))
	for _, ln := range lines {
		if len(f.keepRe) > 0 && !anyMatch(f.keepRe, ln) {
			continue
		}
		if anyMatch(f.stripRe, ln) {
			continue
		}
		kept = append(kept, ln)
	}

	if len(kept) == 0 && f.OnEmpty != "" {
		out := f.OnEmpty
		return out, out != output
	}

	out := strings.Join(kept, "\n")
	if f.MaxLines > 0 && len(kept) > f.MaxLines {
		out, _ = HeadTail(out, f.MaxLines)
	}
	return out, out != output
}

func anyMatch(res []*regexp.Regexp, s string) bool {
	for _, r := range res {
		if r.MatchString(s) {
			return true
		}
	}
	return false
}

// Add appends a filter, or replaces an existing one with the same Name
// (so a user file can override an embedded default). The filter must
// already be compiled.
func (fs *FilterSet) Add(f *Filter) {
	if fs.byName == nil {
		fs.byName = map[string]int{}
	}
	if idx, ok := fs.byName[f.Name]; ok {
		fs.filters[idx] = f
		return
	}
	fs.byName[f.Name] = len(fs.filters)
	fs.filters = append(fs.filters, f)
}

// Match returns the first filter that applies to cmd, or nil.
func (fs *FilterSet) Match(cmd string) *Filter {
	if fs == nil {
		return nil
	}
	for _, f := range fs.filters {
		if f.Matches(cmd) {
			return f
		}
	}
	return nil
}

// Apply runs the matching filter (if any) over output. Returns the
// (possibly unchanged) output and whether a filter applied and changed it.
func (fs *FilterSet) Apply(cmd, output string) (string, bool) {
	f := fs.Match(cmd)
	if f == nil {
		return output, false
	}
	return f.Apply(output)
}

// Covers reports whether any filter matches cmd. Used by /discover to tell
// covered commands from uncovered ones.
func (fs *FilterSet) Covers(cmd string) bool {
	return fs.Match(cmd) != nil
}

// Names returns the loaded filter names in order — for diagnostics.
func (fs *FilterSet) Names() []string {
	if fs == nil {
		return nil
	}
	out := make([]string, len(fs.filters))
	for i, f := range fs.filters {
		out[i] = f.Name
	}
	return out
}

// RunSelfTests executes every filter's inline tests and returns the first
// failure (or nil). Embedded defaults are expected to pass; a regression
// test calls this so a broken shipped filter fails CI.
func (fs *FilterSet) RunSelfTests() error {
	if fs == nil {
		return nil
	}
	for _, f := range fs.filters {
		if err := f.runSelfTests(); err != nil {
			return err
		}
	}
	return nil
}

func (f *Filter) runSelfTests() error {
	for _, t := range f.Tests {
		got, _ := f.Apply(t.Input)
		if t.ExpectEqual != "" && got != t.ExpectEqual {
			return fmt.Errorf("filter %q test %q: expected %q, got %q", f.Name, t.Name, t.ExpectEqual, got)
		}
		for _, want := range t.ExpectContains {
			if !strings.Contains(got, want) {
				return fmt.Errorf("filter %q test %q: output missing %q\n--- got ---\n%s", f.Name, t.Name, want, got)
			}
		}
		for _, no := range t.ExpectNotContains {
			if strings.Contains(got, no) {
				return fmt.Errorf("filter %q test %q: output unexpectedly contains %q\n--- got ---\n%s", f.Name, t.Name, no, got)
			}
		}
	}
	return nil
}
