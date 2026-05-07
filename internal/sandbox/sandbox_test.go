// SPDX-License-Identifier: AGPL-3.0-or-later

package sandbox

import (
	"strings"
	"testing"
)

func TestSanitizeName(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"enso", "enso"},
		{"My Project", "my-project"},
		{"path/to/dir", "path-to-dir"},
		{"weird@@chars!!", "weird-chars"},
		{"---trim-dashes---", "trim-dashes"},
		{strings.Repeat("a", 64), strings.Repeat("a", 32)},
		{"", ""},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := sanitizeName(tc.in); got != tc.want {
				t.Errorf("sanitizeName(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSanitizeLabelValue(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"plain path passes through", "/home/user/proj", "/home/user/proj"},
		{"newline replaced", "/tmp/foo\nbar", "/tmp/foo?bar"},
		{"tab replaced", "/tmp/a\tb", "/tmp/a?b"},
		{"NUL replaced", "/tmp/a\x00b", "/tmp/a?b"},
		{"DEL replaced", "/tmp/a\x7fb", "/tmp/a?b"},
		{"CR replaced", "/tmp/a\rb", "/tmp/a?b"},
		{"empty stays empty", "", ""},
		{"truncated at 256", strings.Repeat("a", 300), strings.Repeat("a", 256)},
		// Non-ASCII high bytes (e.g. UTF-8 multibyte) pass through as the
		// raw bytes — they're not control chars; runtimes accept them and
		// modern terminals render them.
		{"utf8 passes through", "/tmp/café", "/tmp/café"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeLabelValue(tc.in); got != tc.want {
				t.Errorf("sanitizeLabelValue(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestResolveName(t *testing.T) {
	m := &Manager{cwd: "/home/user/myproj", cfg: Config{}}
	got := m.resolveName()
	if !strings.HasPrefix(got, "enso-myproj-") {
		t.Errorf("expected `enso-myproj-...` prefix, got %q", got)
	}
	// Hash suffix is 6 hex chars: `enso-myproj-<6hex>`.
	if len(got) != len("enso-myproj-")+6 {
		t.Errorf("unexpected total length %d in %q", len(got), got)
	}

	// Same basename, different abs path → different name.
	m2 := &Manager{cwd: "/var/www/myproj", cfg: Config{}}
	if m.resolveName() == m2.resolveName() {
		t.Errorf("collision: same name for distinct cwds %q vs %q", m.cwd, m2.cwd)
	}

	// Explicit name wins.
	m3 := &Manager{cwd: "/x", cfg: Config{Name: "custom-name"}}
	if got := m3.resolveName(); got != "custom-name" {
		t.Errorf("explicit name should win, got %q", got)
	}
}

func TestComputeHashChangesOnConfigChange(t *testing.T) {
	base := Config{
		Image: "alpine:latest",
		Init:  []string{"apk add git"},
	}
	m1 := &Manager{cfg: base}
	h1 := m1.computeHash()

	// Same config → same hash.
	m2 := &Manager{cfg: base}
	if m2.computeHash() != h1 {
		t.Errorf("hash changed despite identical config")
	}

	// Different image → different hash.
	cfg2 := base
	cfg2.Image = "alpine:3.19"
	if (&Manager{cfg: cfg2}).computeHash() == h1 {
		t.Errorf("hash should change on image switch")
	}

	// Different init → different hash.
	cfg3 := base
	cfg3.Init = []string{"apk add git make"}
	if (&Manager{cfg: cfg3}).computeHash() == h1 {
		t.Errorf("hash should change on init change")
	}

	// Reordered init → different hash (deterministic, position-sensitive).
	cfg4 := base
	cfg4.Init = []string{"apk add git", "echo hi"}
	cfg5 := base
	cfg5.Init = []string{"echo hi", "apk add git"}
	if (&Manager{cfg: cfg4}).computeHash() == (&Manager{cfg: cfg5}).computeHash() {
		t.Errorf("init order should affect hash")
	}
}

func TestPathInsideContainer(t *testing.T) {
	m := &Manager{cwd: "/home/user/proj", cfg: Config{WorkdirMount: "/work"}}
	cases := []struct {
		host, want string
	}{
		{"/home/user/proj/main.go", "/work/main.go"},
		{"/home/user/proj/sub/dir/x.txt", "/work/sub/dir/x.txt"},
		{"/home/user/proj", "/work"},
		{"/home/other/file.go", ""}, // outside cwd
		{"/etc/passwd", ""},
	}
	for _, tc := range cases {
		t.Run(tc.host, func(t *testing.T) {
			if got := m.PathInsideContainer(tc.host); got != tc.want {
				t.Errorf("PathInsideContainer(%q) = %q, want %q", tc.host, got, tc.want)
			}
		})
	}
}

func TestResolveRuntimeUnknown(t *testing.T) {
	if _, err := resolveRuntime(Runtime("nope")); err == nil {
		t.Errorf("expected error for unknown runtime")
	}
}
