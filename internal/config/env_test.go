// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"strings"
	"testing"
)

// Non-ENSO_ env vars must NOT expand even when they're set in the
// process env. A hostile config saying `Bearer $AWS_SECRET_ACCESS_KEY`
// gets an empty string instead of the actual secret.
func TestExpandEnsoEnv_NonPrefixedRefusedEvenWhenSet(t *testing.T) {
	t.Setenv("AWS_SECRET_ACCESS_KEY", "do-not-leak")
	t.Setenv("GITHUB_TOKEN", "do-not-leak-either")

	for _, in := range []string{
		"Bearer $AWS_SECRET_ACCESS_KEY",
		"$GITHUB_TOKEN",
		"${AWS_SECRET_ACCESS_KEY}",
	} {
		got := ExpandEnsoEnv(in)
		if strings.Contains(got, "do-not-leak") {
			t.Errorf("ExpandEnsoEnv(%q) leaked secret: %q", in, got)
		}
	}
}

// Mixed strings: ENSO_ portions expand, non-ENSO portions go empty.
func TestExpandEnsoEnv_MixedReferences(t *testing.T) {
	t.Setenv("ENSO_HEAD", "ok")
	t.Setenv("PRIVATE", "leak")

	got := ExpandEnsoEnv("$ENSO_HEAD-$PRIVATE")
	if got != "ok-" {
		t.Errorf("got %q, want %q", got, "ok-")
	}
}

// Both $VAR and ${VAR} forms are gated.
func TestExpandEnsoEnv_BraceForm(t *testing.T) {
	t.Setenv("ENSO_X", "ok")
	t.Setenv("OTHER_X", "leak")

	if got := ExpandEnsoEnv("${ENSO_X}"); got != "ok" {
		t.Errorf("brace ENSO_ form: got %q, want ok", got)
	}
	if got := ExpandEnsoEnv("${OTHER_X}"); got != "" {
		t.Errorf("brace non-ENSO form: got %q, want empty", got)
	}
}

// Strings without any $-references pass through unchanged.
func TestExpandEnsoEnv_LiteralPassthrough(t *testing.T) {
	for _, s := range []string{"", "literal", "no-vars-here", "Bearer literal-token-xyz"} {
		if got := ExpandEnsoEnv(s); got != s {
			t.Errorf("ExpandEnsoEnv(%q) = %q, want unchanged", s, got)
		}
	}
}

// Same offending var name multiple times in one process should warn
// only once (dedupe). Hard to assert log-output directly without
// plumbing a custom slog handler, so just exercise the path and rely
// on the dedupe map logic; if the dedupe regresses, the warning flood
// would be visible in `~/.enso/enso.log` during real use.
func TestExpandEnsoEnv_WarnDedupeDoesNotCrash(t *testing.T) {
	for range 5 {
		_ = ExpandEnsoEnv("$SOME_OTHER_VAR")
	}
}
