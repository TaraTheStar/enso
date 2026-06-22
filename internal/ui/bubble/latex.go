// SPDX-License-Identifier: AGPL-3.0-or-later

package bubble

import (
	"regexp"
	"strings"
)

// Models — especially ones trained heavily on math/markdown corpora —
// frequently emit LaTeX fragments in plain prose: inline math spans like
// `$\rightarrow$` and bare commands like `\alpha` or `\times`. A terminal
// has no LaTeX engine, so these render literally as `$\rightarrow$` instead
// of the arrow the model meant. delatex rewrites the common subset to their
// Unicode equivalents before the text reaches glamour.
//
// It is deliberately conservative:
//   - fenced code blocks and inline `code` spans are passed through
//     untouched, so source that legitimately contains backslashes or `$`
//     (shell, regex, TeX-about-TeX) is never corrupted;
//   - a `$...$` span is only unwrapped when its content actually contains a
//     LaTeX command (a backslash). `$5 and $10` therefore stays as written;
//   - unknown `\command`s are left verbatim rather than blanked, so a stray
//     `\n` or `\section` in prose survives intact.

var (
	// mathBlockRE matches `$$...$$` display spans (no newline inside),
	// mathInlineRE matches `$...$` inline spans whose body has no `$`.
	mathBlockRE  = regexp.MustCompile(`\$\$([^\n]+?)\$\$`)
	mathInlineRE = regexp.MustCompile(`\$([^$\n]+?)\$`)
	// latexCmdRE matches a backslash followed by ASCII letters — the shape
	// of a LaTeX control word (\alpha, \rightarrow, \Sigma).
	latexCmdRE = regexp.MustCompile(`\\[a-zA-Z]+`)
)

// latexUnicode maps LaTeX control words (symbols + Greek) to Unicode.
// Commands absent from the map are left as-is by delatexProse.
var latexUnicode = map[string]string{
	// arrows
	`\rightarrow`:     "→",
	`\to`:             "→",
	`\longrightarrow`: "⟶",
	`\leftarrow`:      "←",
	`\gets`:           "←",
	`\longleftarrow`:  "⟵",
	`\leftrightarrow`: "↔",
	`\Rightarrow`:     "⇒",
	`\implies`:        "⇒",
	`\Leftarrow`:      "⇐",
	`\Leftrightarrow`: "⇔",
	`\iff`:            "⇔",
	`\uparrow`:        "↑",
	`\downarrow`:      "↓",
	`\updownarrow`:    "↕",
	`\mapsto`:         "↦",
	`\hookrightarrow`: "↪",
	`\hookleftarrow`:  "↩",

	// binary operators & relations
	`\times`:  "×",
	`\div`:    "÷",
	`\cdot`:   "·",
	`\ast`:    "∗",
	`\star`:   "⋆",
	`\bullet`: "•",
	`\circ`:   "∘",
	`\pm`:     "±",
	`\mp`:     "∓",
	`\oplus`:  "⊕",
	`\ominus`: "⊖",
	`\otimes`: "⊗",
	`\odot`:   "⊙",
	`\leq`:    "≤",
	`\le`:     "≤",
	`\geq`:    "≥",
	`\ge`:     "≥",
	`\neq`:    "≠",
	`\ne`:     "≠",
	`\approx`: "≈",
	`\equiv`:  "≡",
	`\cong`:   "≅",
	`\sim`:    "∼",
	`\simeq`:  "≃",
	`\propto`: "∝",
	`\ll`:     "≪",
	`\gg`:     "≫",

	// big operators & calculus
	`\sum`:     "∑",
	`\prod`:    "∏",
	`\int`:     "∫",
	`\oint`:    "∮",
	`\partial`: "∂",
	`\nabla`:   "∇",
	`\sqrt`:    "√",
	`\infty`:   "∞",

	// set theory & logic
	`\cup`:        "∪",
	`\cap`:        "∩",
	`\setminus`:   "∖",
	`\subset`:     "⊂",
	`\supset`:     "⊃",
	`\subseteq`:   "⊆",
	`\supseteq`:   "⊇",
	`\in`:         "∈",
	`\notin`:      "∉",
	`\ni`:         "∋",
	`\forall`:     "∀",
	`\exists`:     "∃",
	`\nexists`:    "∄",
	`\emptyset`:   "∅",
	`\varnothing`: "∅",
	`\wedge`:      "∧",
	`\land`:       "∧",
	`\vee`:        "∨",
	`\lor`:        "∨",
	`\neg`:        "¬",
	`\lnot`:       "¬",
	`\top`:        "⊤",
	`\bot`:        "⊥",
	`\vdash`:      "⊢",
	`\models`:     "⊨",

	// delimiters, dots, misc
	`\langle`:    "⟨",
	`\rangle`:    "⟩",
	`\lceil`:     "⌈",
	`\rceil`:     "⌉",
	`\lfloor`:    "⌊",
	`\rfloor`:    "⌋",
	`\ldots`:     "…",
	`\dots`:      "…",
	`\cdots`:     "⋯",
	`\vdots`:     "⋮",
	`\ddots`:     "⋱",
	`\prime`:     "′",
	`\deg`:       "°",
	`\angle`:     "∠",
	`\perp`:      "⊥",
	`\parallel`:  "∥",
	`\therefore`: "∴",
	`\because`:   "∵",

	// spacing / grouping noise → drop or collapse to a space
	`\left`:  "",
	`\right`: "",
	`\quad`:  " ",
	`\qquad`: "  ",
	`\,`:     " ", // not letter-matched, kept for documentation
	`\!`:     "",

	// Greek lowercase
	`\alpha`:      "α",
	`\beta`:       "β",
	`\gamma`:      "γ",
	`\delta`:      "δ",
	`\epsilon`:    "ε",
	`\varepsilon`: "ε",
	`\zeta`:       "ζ",
	`\eta`:        "η",
	`\theta`:      "θ",
	`\vartheta`:   "ϑ",
	`\iota`:       "ι",
	`\kappa`:      "κ",
	`\lambda`:     "λ",
	`\mu`:         "μ",
	`\nu`:         "ν",
	`\xi`:         "ξ",
	`\pi`:         "π",
	`\varpi`:      "ϖ",
	`\rho`:        "ρ",
	`\varrho`:     "ϱ",
	`\sigma`:      "σ",
	`\varsigma`:   "ς",
	`\tau`:        "τ",
	`\upsilon`:    "υ",
	`\phi`:        "φ",
	`\varphi`:     "φ",
	`\chi`:        "χ",
	`\psi`:        "ψ",
	`\omega`:      "ω",

	// Greek uppercase
	`\Gamma`:   "Γ",
	`\Delta`:   "Δ",
	`\Theta`:   "Θ",
	`\Lambda`:  "Λ",
	`\Xi`:      "Ξ",
	`\Pi`:      "Π",
	`\Sigma`:   "Σ",
	`\Upsilon`: "Υ",
	`\Phi`:     "Φ",
	`\Psi`:     "Ψ",
	`\Omega`:   "Ω",
}

// delatex rewrites LaTeX symbol/Greek fragments to Unicode across prose,
// leaving fenced code blocks and inline code spans untouched.
func delatex(s string) string {
	if !strings.ContainsAny(s, `$\`) {
		return s
	}
	lines := strings.Split(s, "\n")
	var inFence bool
	var fence string
	for i, ln := range lines {
		trimmed := strings.TrimLeft(ln, " \t")
		if inFence {
			if fence != "" && strings.HasPrefix(trimmed, fence) {
				inFence = false
			}
			continue
		}
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inFence = true
			fence = trimmed[:3]
			continue
		}
		lines[i] = delatexInline(ln)
	}
	return strings.Join(lines, "\n")
}

// delatexInline processes a single non-fenced line, skipping inline code
// spans delimited by runs of backticks (`x`, “x“), and rewriting the
// prose between them.
func delatexInline(line string) string {
	if !strings.ContainsAny(line, `$\`) {
		return line
	}
	if !strings.Contains(line, "`") {
		return delatexProse(line)
	}
	var b strings.Builder
	i := 0
	for i < len(line) {
		if line[i] == '`' {
			j := i
			for j < len(line) && line[j] == '`' {
				j++
			}
			ticks := line[i:j]
			if rel := strings.Index(line[j:], ticks); rel >= 0 {
				end := j + rel + len(ticks)
				b.WriteString(line[i:end]) // code span, verbatim
				i = end
				continue
			}
			b.WriteString(ticks) // unterminated run, emit literally
			i = j
			continue
		}
		next := strings.IndexByte(line[i:], '`')
		if next < 0 {
			b.WriteString(delatexProse(line[i:]))
			break
		}
		b.WriteString(delatexProse(line[i : i+next]))
		i += next
	}
	return b.String()
}

// delatexProse rewrites a stretch of prose: it first unwraps `$$...$$` and
// `$...$` math spans that contain a command, then substitutes known control
// words anywhere in the result (covering both unwrapped spans and bare
// commands like `\alpha`).
func delatexProse(s string) string {
	if !strings.ContainsAny(s, `$\`) {
		return s
	}
	if strings.Contains(s, "$") {
		s = mathBlockRE.ReplaceAllStringFunc(s, unwrapMath)
		s = mathInlineRE.ReplaceAllStringFunc(s, unwrapMath)
	}
	if strings.Contains(s, `\`) {
		s = latexCmdRE.ReplaceAllStringFunc(s, func(cmd string) string {
			if u, ok := latexUnicode[cmd]; ok {
				return u
			}
			return cmd
		})
	}
	return s
}

// delatexStream is the live-streaming variant of delatex, applied to the
// in-flight assistant block on every frame. It differs from delatex in two
// ways, both forced by streaming:
//
//   - It is not fence-aware. The live view shows raw text (markdown is only
//     parsed on graduation), and fence state is inherently multi-line, which
//     can't be reconstructed from the line-at-a-time folding the live-render
//     cache does. Inline `code` spans are still protected (that's per-line).
//
//   - It holds back a trailing *partial* command or unclosed `$` span on the
//     last line, leaving it raw until more text arrives. Without this, a
//     `\rightarrow` still mid-stream would flash through `\right` (a known
//     command → "") and the text would visibly jump.
//
// Every *complete* line (everything up to the last '\n') is transformed in
// full. Because a complete line never changes once its newline has streamed,
// its transformed bytes are immutable — which is exactly the invariant the
// live-render cache relies on to keep its stable prefix. Both renderBlock's
// live arm and the cache feed text through this function identically, so they
// stay byte-for-byte equal (see TestLiveRenderMatchesRenderBlock).
func delatexStream(text string) string {
	if !strings.ContainsAny(text, `$\`) {
		return text
	}
	head, tail := "", text
	if nl := strings.LastIndexByte(text, '\n'); nl >= 0 {
		head, tail = text[:nl+1], text[nl+1:]
	}
	if head != "" {
		lines := strings.Split(head, "\n")
		for i, ln := range lines {
			lines[i] = delatexInline(ln)
		}
		head = strings.Join(lines, "\n")
	}
	return head + delatexGuardedTail(tail)
}

// delatexGuardedTail transforms the still-streaming last line, but leaves a
// trailing incomplete construct untouched so it doesn't flicker: an unclosed
// `$` span (odd number of `$`) or a trailing `\command` whose name may still
// be growing. The held-back suffix is emitted raw and picked up once the next
// delta completes it.
func delatexGuardedTail(s string) string {
	cut := len(s)
	if strings.Count(s, "$")%2 == 1 {
		if i := strings.LastIndexByte(s, '$'); i >= 0 && i < cut {
			cut = i
		}
	}
	if i := strings.LastIndexByte(s[:cut], '\\'); i >= 0 && trailingLetters(s[i+1:cut]) {
		cut = i
	}
	if cut >= len(s) {
		return delatexInline(s)
	}
	return delatexInline(s[:cut]) + s[cut:]
}

// trailingLetters reports whether s is the (possibly empty) ASCII-letter tail
// of an in-progress control word. Empty qualifies: a bare trailing backslash
// could be the start of a command, so it's held back too.
func trailingLetters(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') {
			return false
		}
	}
	return true
}

// unwrapMath drops the `$` delimiters of a math span, but only when the body
// holds a LaTeX command. Spans without a backslash (currency, `$x$` used as a
// literal) are returned unchanged so prose isn't mangled.
func unwrapMath(span string) string {
	inner := strings.Trim(span, "$")
	if !strings.Contains(inner, `\`) {
		return span
	}
	return inner
}
