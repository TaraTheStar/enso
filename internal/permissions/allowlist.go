// SPDX-License-Identifier: AGPL-3.0-or-later

package permissions

import (
	"net/url"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// Kind classifies a Pattern's effect: allow, prompt-the-user (ask), or deny.
// Ask wins over allow but loses to deny — it's "matched, but always run
// the call past the user". Useful for blast-radius rules like
// `Bash(git push *)` even when broader allows are in place.
type Kind int

const (
	KindAllow Kind = iota
	KindAsk
	KindDeny
)

// Pattern represents one allow / ask / deny rule of the form
// `tool_name(arg_pattern)`.
type Pattern struct {
	Tool string
	Arg  string
	Kind Kind
}

// ParsePattern parses a pattern string like "bash(git *)", "edit(./src/**)",
// or "web_fetch(domain:example.com)". A leading `!` makes the rule a deny;
// caller-set Kind otherwise wins.
func ParsePattern(s string) (*Pattern, error) {
	s = strings.TrimSpace(s)
	deny := strings.HasPrefix(s, "!")
	if deny {
		s = strings.TrimPrefix(s, "!")
	}
	idx := strings.IndexByte(s, '(')
	if idx < 0 {
		return nil, nil // bare tool name matches all args
	}
	tool := s[:idx]
	arg := s[idx+1 : strings.LastIndexByte(s, ')')]
	p := &Pattern{Tool: tool, Arg: arg}
	if deny {
		p.Kind = KindDeny
	}
	return p, nil
}

// Allowlist holds the parsed rules in evaluation order: deny first, then
// ask, then allow. Within a kind, first match wins (currently moot — Match
// returns on first hit).
type Allowlist struct {
	patterns []*Pattern
}

// NewAllowlist builds an Allowlist from three pattern lists. The order
// inside the resulting slice is deny → ask → allow, so a single forward
// pass naturally honours deny→ask→allow precedence.
func NewAllowlist(allow, ask, deny []string) *Allowlist {
	al := &Allowlist{}
	add := func(list []string, kind Kind) {
		for _, s := range list {
			p, _ := ParsePattern(s)
			if p == nil {
				continue
			}
			p.Kind = kind
			al.patterns = append(al.patterns, p)
		}
	}
	add(deny, KindDeny)
	add(ask, KindAsk)
	add(allow, KindAllow)
	return al
}

// Match evaluates a tool call against every pattern in deny→ask→allow
// order and returns (matched, kind). On no hit, matched is false and the
// caller should fall back to the configured mode default.
//
// Bash allow rules carry an extra constraint: any shell metacharacter
// present in the command must also appear in the pattern. Without that,
// `bash(git *)` would auto-allow `git status; rm -rf ~` (the `*`
// happily swallows the chained command). Ask rules don't get this gate
// — gating ask would weaken the safety prompt.
//
// Bash deny rules get the symmetric extension: if the full-command
// match misses, every top-level segment (split on `;`, `&&`, `||`, `|`,
// `&`, newline) is also tested. That closes `do_evil; rm -rf /` evading
// `bash(rm -rf *)` — the deny still fires on the trailing segment.
// Residual gap (not closed here): command substitution like
// `$(rm -rf /)` and backtick equivalents still bypass; for adversarial
// inputs, an isolating backend (`[backend] type = "podman"` or
// `"lima"`) is the real boundary, not deny rules.
func (al *Allowlist) Match(tool, arg string) (matched bool, kind Kind) {
	for _, p := range al.patterns {
		if p.Tool != "*" && p.Tool != tool {
			continue
		}
		if matchArg(tool, p.Arg, arg) {
			if p.Kind == KindAllow && tool == "bash" && bashHasUnchainedMetachars(p.Arg, arg) {
				continue
			}
			return true, p.Kind
		}
		// Bash deny: re-test against each top-level segment. Allow/ask
		// stay full-command-match-only because their semantics ("user
		// explicitly opted into this exact pattern" / "always confirm")
		// shouldn't fire on a fragment of an unrelated command.
		if p.Kind == KindDeny && tool == "bash" {
			for _, seg := range bashSplitTopLevel(arg) {
				if matchArg(tool, p.Arg, seg) {
					return true, KindDeny
				}
			}
		}
	}
	return false, KindAllow
}

// bashShellMetachars are the characters whose presence in a bash
// command can introduce a new command, redirect output, or hide chained
// behaviour from a naive token match. If any of these is in the user's
// command but NOT in the matched allow pattern, the pattern doesn't
// honour the call — the user must opt in explicitly by writing the
// metachar into the pattern (e.g. `bash(git * | *)` to allow piping).
//
// Coverage:
//
//	; & |        — command separators / pipes / background
//	< > $        — redirection / variable + command substitution
//	` ( ) \      — backtick subst, subshell, escape (line-continuation)
//	\n           — newline acts as `;`
const bashShellMetachars = ";&|<>$`()\\\n"

// bashHasUnchainedMetachars reports whether `cmd` contains any
// metachar that's missing from `pattern`. The bare "*" pattern is
// special-cased as "user explicitly opted into anything goes" — it
// short-circuits earlier in MatchCommand before this gate even runs,
// but we double-check defensively here.
func bashHasUnchainedMetachars(pattern, cmd string) bool {
	if strings.TrimSpace(pattern) == "*" {
		return false
	}
	for _, c := range bashShellMetachars {
		if strings.ContainsRune(cmd, c) && !strings.ContainsRune(pattern, c) {
			return true
		}
	}
	return false
}

// bashSplitTopLevel returns the top-level command segments of a bash
// string, splitting on `;`, `&&`, `||`, `|`, `&`, and newlines.
// Single- and double-quoted runs are honoured so a literal separator
// inside a string doesn't trigger a split. Subshell extraction is
// deliberately out of scope (`$(...)` and backticks are NOT recursed
// into) — this matcher is the cheap separator-split tier; real
// adversarial isolation belongs in an isolating backend
// (`[backend] type = "podman"` or `"lima"`).
//
// Empty segments are dropped, so trailing or doubled separators
// don't produce blank entries the caller has to filter.
func bashSplitTopLevel(cmd string) []string {
	var segs []string
	var cur strings.Builder
	var quote byte
	flush := func() {
		seg := strings.TrimSpace(cur.String())
		if seg != "" {
			segs = append(segs, seg)
		}
		cur.Reset()
	}
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		if quote != 0 {
			cur.WriteByte(c)
			if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
			cur.WriteByte(c)
		case ';', '\n':
			flush()
		case '|', '&':
			// `&&` and `||` are two-byte separators; consume the
			// second byte. `|` and `&` alone are also separators
			// (pipe and background), so the single-byte form falls
			// through to the same flush.
			if i+1 < len(cmd) && cmd[i+1] == c {
				i++
			}
			flush()
		default:
			cur.WriteByte(c)
		}
	}
	flush()
	return segs
}

// matchArg dispatches per-tool argument matching:
//
//	bash      → first-word-aware (so `bash(git *)` works on any "git ...")
//	read/write/edit/grep/glob → strict doublestar.PathMatch on the
//	                            absolute path. No basename fallback —
//	                            `*.go` does not match `/abs/foo.go`;
//	                            use `**/*.go` for "any .go file".
//	web_fetch → if pattern starts with `domain:`, match the URL's host
//	            against the rest; otherwise treat as a glob over the URL
//	*         → generic doublestar match
func matchArg(tool, pattern, arg string) bool {
	switch tool {
	case "bash":
		return MatchCommand(pattern, arg)
	case "read", "write", "edit", "grep", "glob":
		return MatchPath(pattern, arg)
	case "web_fetch":
		if strings.HasPrefix(pattern, "domain:") {
			return matchURLDomain(strings.TrimPrefix(pattern, "domain:"), arg)
		}
	}
	m, _ := doublestar.Match(pattern, arg)
	return m
}

// matchURLDomain returns true if `urlStr` parses to an absolute URL whose
// host matches `pattern` (doublestar). The bare-host pattern `example.com`
// also matches subdomains like `api.example.com` for ergonomics.
func matchURLDomain(pattern, urlStr string) bool {
	u, err := url.Parse(urlStr)
	if err != nil || u.Host == "" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	pat := strings.ToLower(pattern)
	if host == pat {
		return true
	}
	// Match bare-domain pattern against subdomains.
	if strings.HasSuffix(host, "."+pat) {
		return true
	}
	if m, _ := doublestar.Match(pat, host); m {
		return true
	}
	return false
}

// AppendPattern adds a parsed pattern to the live allowlist. Used by
// Checker.AddAllow when the user picks "Allow + Remember" in the modal.
func (al *Allowlist) AppendPattern(p *Pattern) {
	al.patterns = append(al.patterns, p)
}

// Remove drops the first pattern with the given tool/arg/kind. Returns
// true if anything was removed. Used by the /permissions overlay to
// reflect a deleted rule in the live checker without restarting.
func (al *Allowlist) Remove(tool, arg string, kind Kind) bool {
	for i, p := range al.patterns {
		if p.Tool == tool && p.Arg == arg && p.Kind == kind {
			al.patterns = append(al.patterns[:i], al.patterns[i+1:]...)
			return true
		}
	}
	return false
}

// Patterns returns a copy of the current rules. Used by the
// /permissions overlay to render the live state alongside the
// on-disk file.
func (al *Allowlist) Patterns() []Pattern {
	out := make([]Pattern, 0, len(al.patterns))
	for _, p := range al.patterns {
		out = append(out, *p)
	}
	return out
}
