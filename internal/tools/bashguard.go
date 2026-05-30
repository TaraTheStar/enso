// SPDX-License-Identifier: AGPL-3.0-or-later

package tools

import "regexp"

// looksNonTerminating reports whether a foreground bash command, by its
// nature, won't return on its own — so running it in the foreground would
// only block until the timeout fires. BashTool.Run uses this to steer the
// model toward run_in_background *before* executing, turning the
// after-the-fact wall-clock kill into an immediate, cheaper nudge.
//
// Deliberately conservative. A false positive costs the model one extra
// turn (it re-runs with run_in_background or an explicit timeout — both
// always valid), while a false negative is still caught by the timeout
// backstop. Commands that already bound or detach themselves (a `timeout`
// wrapper, an explicit `&` / `nohup` / `setsid`, or a pipe into a
// self-terminating consumer like `head`) are left alone.
func looksNonTerminating(cmd string) (reason string, ok bool) {
	if alreadyBounded(cmd) {
		return "", false
	}
	for _, p := range nonTerminatingPatterns {
		if p.re.MatchString(cmd) {
			return p.reason, true
		}
	}
	return "", false
}

type nonTerminatingPattern struct {
	re     *regexp.Regexp
	reason string
}

// [^|&;] in the follow-flag patterns keeps the match within a single
// pipeline segment so `tail -f log | head` (which DOES terminate) isn't
// flagged by the flag landing in a later command.
var nonTerminatingPatterns = []nonTerminatingPattern{
	{regexp.MustCompile(`(?i)\btail\b[^|&;]*\s--?[a-z]*[fF]\b`), "`tail -f` follows the file and never exits on its own"},
	{regexp.MustCompile(`(?i)\btail\b[^|&;]*\s--follow\b`), "`tail --follow` never exits on its own"},
	{regexp.MustCompile(`(?i)(^|[\s|&;])watch\s`), "`watch` re-runs its command forever and never exits"},
	{regexp.MustCompile(`(?i)\bjournalctl\b[^|&;]*\s(--?[a-z]*f|--follow)\b`), "`journalctl -f` follows the journal and never exits"},
	{regexp.MustCompile(`(?i)\b(kubectl|docker|podman|nerdctl)\b[^|&;]*\blogs\b[^|&;]*\s(-f|--follow)\b`), "a `logs --follow` stream never exits on its own"},
	{regexp.MustCompile(`(?i)\b(npm|pnpm|yarn|bun)\b\s+(run\s+)?(dev|start|serve|watch)\b`), "a dev server / watcher runs until killed"},
	{regexp.MustCompile(`(?i)\b(vite|nuxt|astro|remix)\b`), "a dev server runs until killed"},
	{regexp.MustCompile(`(?i)\bnext\s+(dev|start)\b`), "the Next.js server runs until killed"},
	{regexp.MustCompile(`(?i)\bng\s+serve\b`), "`ng serve` runs until killed"},
	{regexp.MustCompile(`(?i)\bhugo\s+server\b`), "`hugo server` runs until killed"},
	{regexp.MustCompile(`(?i)\bjekyll\s+serve\b`), "`jekyll serve` runs until killed"},
	{regexp.MustCompile(`(?i)\bpython[0-9.]*\b[^|&;]*-m\s+http\.server\b`), "`http.server` runs until killed"},
	{regexp.MustCompile(`(?i)\bphp\s+-S\b`), "`php -S` runs a server until killed"},
	{regexp.MustCompile(`(?i)\b(rails|bin/rails)\s+(s|server)\b`), "`rails server` runs until killed"},
	{regexp.MustCompile(`(?i)\bflask\s+run\b`), "`flask run` runs until killed"},
	{regexp.MustCompile(`(?i)\bmanage\.py\s+runserver\b`), "Django `runserver` runs until killed"},
}

var (
	// wrapperBoundedRe matches constructs that already bound/detach the
	// command: a timeout-style wrapper, an explicit detach, or a pipe into
	// a consumer that closes the stream (head / grep -m).
	wrapperBoundedRe = regexp.MustCompile(`(?i)(\btimeout\s+\S|\btimelimit\s+\S|\bnohup\b|\bsetsid\b|\bdisown\b|\|\s*head\b|grep\b[^|]*\s-m\b)`)
	// backgroundedRe matches a single trailing/sequencing `&` (backgrounded),
	// but not the `&&` logical-AND operator.
	backgroundedRe = regexp.MustCompile(`(^|[^&])&(\s|$)`)
)

func alreadyBounded(cmd string) bool {
	return wrapperBoundedRe.MatchString(cmd) || backgroundedRe.MatchString(cmd)
}
