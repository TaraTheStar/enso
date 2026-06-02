// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad_LayeredMerge(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "xdg"))
	t.Setenv("HOME", filepath.Join(tmp, "home"))

	// User layer — sets endpoint and an allow rule.
	xdg := filepath.Join(tmp, "xdg", "enso")
	mustMkdir(t, xdg)
	mustWrite(t, filepath.Join(xdg, "config.toml"), `
[providers.local]
endpoint = "http://user:8080/v1"
model = "from-user"

[permissions]
allow = ["read(*)"]
`)

	// Project layer at <cwd>/.enso/config.toml — overrides model only;
	// endpoint should survive from the user layer.
	cwd := filepath.Join(tmp, "proj")
	mustMkdir(t, filepath.Join(cwd, ".enso"))
	mustWrite(t, filepath.Join(cwd, ".enso", "config.toml"), `
[providers.local]
model = "from-project"

[permissions]
allow = ["bash(git *)"]
`)

	// Explicit -c layer at <tmp>/explicit.toml — overrides only the model.
	explicit := filepath.Join(tmp, "explicit.toml")
	mustWrite(t, explicit, `
[providers.local]
model = "from-flag"
`)

	cfg, err := Load(cwd, explicit)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	p, ok := cfg.Providers["local"]
	if !ok {
		t.Fatalf("missing providers.local")
	}
	// model: user < project < flag → flag wins
	if p.Model != "from-flag" {
		t.Errorf("model = %q, want from-flag", p.Model)
	}
	// endpoint: only user sets it → user value survives
	if p.Endpoint != "http://user:8080/v1" {
		t.Errorf("endpoint = %q, want http://user:8080/v1", p.Endpoint)
	}
	// allow: security arrays UNION across tiers (S7) — the user's
	// read(*) survives the project layer adding bash(git *), lower-tier
	// entries first. A higher tier can add rules but never wipe lower
	// (more trusted) ones.
	if got, want := cfg.Permissions.Allow, []string{"read(*)", "bash(git *)"}; !equalStrings(got, want) {
		t.Errorf("allow = %v, want %v", got, want)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestLoad_ExpandsEnsoEnvInProviderAPIKey(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "xdg"))
	t.Setenv("HOME", filepath.Join(tmp, "home"))
	t.Setenv("ENSO_OPENAI_KEY", "sk-real-token")
	// A non-ENSO_ var that must NOT bleed into the api_key, even though
	// it's set in the process env.
	t.Setenv("OPENAI_API_KEY", "do-not-leak")

	xdg := filepath.Join(tmp, "xdg", "enso")
	mustMkdir(t, xdg)
	mustWrite(t, filepath.Join(xdg, "config.toml"), `
[providers.allowed]
endpoint = "http://x/v1"
model = "m"
api_key = "$ENSO_OPENAI_KEY"

[providers.refused]
endpoint = "http://y/v1"
model = "m"
api_key = "$OPENAI_API_KEY"

[providers.literal]
endpoint = "http://z/v1"
model = "m"
api_key = "literal-token"
`)

	cfg, err := Load(filepath.Join(tmp, "no-cwd"), "")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := cfg.Providers["allowed"].APIKey; got != "sk-real-token" {
		t.Errorf("ENSO_-prefixed: got %q, want sk-real-token", got)
	}
	if got := cfg.Providers["refused"].APIKey; got != "" {
		t.Errorf("non-prefixed must collapse to empty, got %q", got)
	}
	if got := cfg.Providers["literal"].APIKey; got != "literal-token" {
		t.Errorf("literal: got %q, want literal-token", got)
	}
}

// TestLoad_SecurityArraysUnionAcrossTiers is the S7 regression: the
// security arrays union across config tiers instead of a higher tier
// replacing (and thus wiping) a lower one. A project's
// .enso/config.local.toml trying `deny = []` must NOT erase the user's
// system-wide deny rules; it can only ADD.
func TestLoad_SecurityArraysUnionAcrossTiers(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "xdg"))
	t.Setenv("HOME", filepath.Join(tmp, "home"))

	// User tier: the trusted baseline guardrails.
	xdg := filepath.Join(tmp, "xdg", "enso")
	mustMkdir(t, xdg)
	mustWrite(t, filepath.Join(xdg, "config.toml"), `
[permissions]
deny = ["bash(rm -rf *)", "read(/etc/**)"]
allow = ["read(**)"]

[web_fetch]
allow_hosts = ["localhost"]
`)

	// Project tier (gitignored, higher priority): attempts to WIPE deny
	// with a bare empty array, and re-declares an allow that's already in
	// the user tier (must dedupe) plus a new one.
	cwd := filepath.Join(tmp, "proj")
	mustMkdir(t, filepath.Join(cwd, ".enso"))
	mustWrite(t, filepath.Join(cwd, ".enso", "config.local.toml"), `
[permissions]
deny = []
allow = ["read(**)", "bash(ls *)"]

[web_fetch]
allow_hosts = ["api.example.com"]
`)

	// Explicit -c tier (highest): legitimately ADDS a deny.
	explicit := filepath.Join(tmp, "explicit.toml")
	mustWrite(t, explicit, `
[permissions]
deny = ["bash(curl *)"]
`)

	cfg, err := Load(cwd, explicit)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	// deny: user's two rules survive the project's deny=[] wipe attempt,
	// and the -c tier's rule is appended. Grow-only.
	if got, want := cfg.Permissions.Deny,
		[]string{"bash(rm -rf *)", "read(/etc/**)", "bash(curl *)"}; !equalStrings(got, want) {
		t.Errorf("deny = %v, want %v", got, want)
	}
	// allow: union with dedup — read(**) declared in both tiers appears once.
	if got, want := cfg.Permissions.Allow,
		[]string{"read(**)", "bash(ls *)"}; !equalStrings(got, want) {
		t.Errorf("allow = %v, want %v", got, want)
	}
	// allow_hosts: union, lower (more trusted) tier first.
	if got, want := cfg.WebFetch.AllowHosts,
		[]string{"localhost", "api.example.com"}; !equalStrings(got, want) {
		t.Errorf("allow_hosts = %v, want %v", got, want)
	}
}

func TestLoad_ExplicitMissingIsError(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "xdg"))
	t.Setenv("HOME", filepath.Join(tmp, "home"))

	_, err := Load(tmp, filepath.Join(tmp, "does-not-exist.toml"))
	if err == nil {
		t.Errorf("missing explicit config: want error, got nil")
	}
}

func TestLoad_AutoCreateOnEmpty(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "xdg"))
	t.Setenv("HOME", filepath.Join(tmp, "home"))

	cfg, err := Load(filepath.Join(tmp, "empty-cwd"), "")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// The default file should have been written into XDG.
	defaultPath := filepath.Join(tmp, "xdg", "enso", "config.toml")
	if _, err := os.Stat(defaultPath); err != nil {
		t.Errorf("default config not written at %s: %v", defaultPath, err)
	}
	// The default has providers.local with endpoint http://localhost:8080/v1.
	if p, ok := cfg.Providers["local"]; !ok || p.Endpoint == "" {
		t.Errorf("default did not populate providers.local")
	}
}

func TestSearchPaths_OrderAndExplicit(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/x")
	paths := SearchPaths("/proj", "/explicit.toml")
	want := []string{
		"/etc/enso/config.toml",
		"/x/enso/config.toml",
		"/proj/.enso/config.toml",
		"/proj/.enso/config.local.toml",
		"/explicit.toml",
	}
	if len(paths) != len(want) {
		t.Fatalf("got %d paths, want %d", len(paths), len(want))
	}
	for i, p := range paths {
		if p != want[i] {
			t.Errorf("paths[%d] = %q, want %q", i, p, want[i])
		}
	}
}

func TestAppendAllow_CreatesNewFileAndAppends(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".enso", "config.local.toml")

	if err := AppendAllow(path, "bash(git *)"); err != nil {
		t.Fatalf("first append: %v", err)
	}
	if err := AppendAllow(path, "read(*)"); err != nil {
		t.Fatalf("second append: %v", err)
	}
	// Dedupe: re-adding the same pattern is a no-op.
	if err := AppendAllow(path, "bash(git *)"); err != nil {
		t.Fatalf("dedupe append: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("readback: %v", err)
	}
	got := string(data)
	// go-toml/v2 may emit either single- or double-quoted strings.
	if !contains(got, `bash(git *)`) || !contains(got, `read(*)`) {
		t.Errorf("file contents missing expected entries:\n%s", got)
	}
	if cnt := count(got, `bash(git *)`); cnt != 1 {
		t.Errorf("dedupe failed: bash(git *) appears %d times in:\n%s", cnt, got)
	}
}

func TestAppendAllow_PreservesOtherSections(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "cfg.toml")
	mustWrite(t, path, `
[providers.local]
endpoint = "http://x"
model = "y"

[permissions]
allow = ["read(*)"]

[ui]
theme = "dark"
`)
	if err := AppendAllow(path, "bash(git *)"); err != nil {
		t.Fatalf("append: %v", err)
	}
	data, _ := os.ReadFile(path)
	got := string(data)
	for _, want := range []string{`endpoint = 'http://x'`, `model = 'y'`, `theme = 'dark'`, `bash(git *)`, `read(*)`} {
		if !contains(got, want) {
			t.Errorf("missing %q after append:\n%s", want, got)
		}
	}
}

func TestLoadRules_MissingFileReturnsEmpty(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "absent.toml")
	a, k, d, err := LoadRules(path)
	if err != nil {
		t.Fatalf("missing file: %v", err)
	}
	if len(a)+len(k)+len(d) != 0 {
		t.Errorf("want empty for missing file, got %v %v %v", a, k, d)
	}
}

func TestLoadRules_ReturnsAllThreeKinds(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "cfg.toml")
	mustWrite(t, path, `
[permissions]
allow = ["bash(git *)", "read(*)"]
ask = ["bash(git push *)"]
deny = ["bash(rm -rf *)"]
`)
	allow, ask, deny, err := LoadRules(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(allow) != 2 || allow[0] != "bash(git *)" {
		t.Errorf("allow: %v", allow)
	}
	if len(ask) != 1 || ask[0] != "bash(git push *)" {
		t.Errorf("ask: %v", ask)
	}
	if len(deny) != 1 || deny[0] != "bash(rm -rf *)" {
		t.Errorf("deny: %v", deny)
	}
}

func TestRemoveRule_DeletesAndPreservesOthers(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "cfg.toml")
	mustWrite(t, path, `
[ui]
theme = "dark"

[permissions]
allow = ["bash(git *)", "read(*)"]
deny = ["bash(rm -rf *)"]
`)
	ok, err := RemoveRule(path, "allow", "bash(git *)")
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if !ok {
		t.Errorf("expected found=true on first removal")
	}
	data, _ := os.ReadFile(path)
	got := string(data)
	if contains(got, "bash(git *)") {
		t.Errorf("rule still present:\n%s", got)
	}
	if !contains(got, "read(*)") || !contains(got, "bash(rm -rf *)") || !contains(got, "theme") {
		t.Errorf("siblings dropped:\n%s", got)
	}
	// Removing again is a no-op.
	ok2, err := RemoveRule(path, "allow", "bash(git *)")
	if err != nil {
		t.Fatal(err)
	}
	if ok2 {
		t.Errorf("second removal should report not-found")
	}
}

func TestRemoveRule_RejectsBadKind(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "cfg.toml")
	mustWrite(t, path, "")
	if _, err := RemoveRule(path, "wibble", "x"); err == nil {
		t.Fatal("expected error on bad kind")
	}
}

func TestRemoveRule_DropsEmptiedListKey(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "cfg.toml")
	mustWrite(t, path, `
[permissions]
allow = ["read(*)"]
`)
	if _, err := RemoveRule(path, "allow", "read(*)"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	data, _ := os.ReadFile(path)
	if contains(string(data), "allow") {
		t.Errorf("emptied key should be dropped, got:\n%s", data)
	}
}

// helpers

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}
func count(s, sub string) int {
	n, i := 0, 0
	for {
		j := indexOfAt(s, sub, i)
		if j < 0 {
			return n
		}
		n++
		i = j + len(sub)
	}
}
func indexOf(s, sub string) int { return indexOfAt(s, sub, 0) }
func indexOfAt(s, sub string, i int) int {
	for ; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func mustMkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", p, err)
	}
}

func mustWrite(t *testing.T, p, body string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
}
