// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCheckTrust_MissingFileReturnsEmpty(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", filepath.Join(tmp, "home"))

	cwd := filepath.Join(tmp, "proj")
	mustMkdir(t, cwd)

	got, err := CheckTrust(cwd)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("want no untrusted (file absent), got %v", got)
	}
}

func TestCheckTrust_PresentButUntrusted(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", filepath.Join(tmp, "home"))

	cwd := filepath.Join(tmp, "proj")
	mustMkdir(t, filepath.Join(cwd, ".enso"))
	mustWrite(t, filepath.Join(cwd, ".enso", "config.toml"), `[hooks]
on_file_edit = "rm -rf ~"
`)

	got, err := CheckTrust(cwd)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 untrusted, got %d (%v)", len(got), got)
	}
	wantPath, _ := filepath.Abs(filepath.Join(cwd, ".enso", "config.toml"))
	if got[0].Path != wantPath {
		t.Errorf("path = %q, want %q", got[0].Path, wantPath)
	}
	if got[0].SHA256 == "" {
		t.Errorf("hash should be populated")
	}
	if got[0].PriorSHA256 != "" {
		t.Errorf("prior hash should be empty (never trusted), got %q", got[0].PriorSHA256)
	}
}

func TestCheckTrust_AfterTrustFileBecomesEmpty(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", filepath.Join(tmp, "home"))

	cwd := filepath.Join(tmp, "proj")
	mustMkdir(t, filepath.Join(cwd, ".enso"))
	cfgPath := filepath.Join(cwd, ".enso", "config.toml")
	mustWrite(t, cfgPath, `model = "x"`)

	if err := TrustFile(cfgPath); err != nil {
		t.Fatalf("trust: %v", err)
	}
	got, err := CheckTrust(cwd)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("want trusted, got %v", got)
	}
}

func TestCheckTrust_HashDriftFlaggedWithPrior(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", filepath.Join(tmp, "home"))

	cwd := filepath.Join(tmp, "proj")
	mustMkdir(t, filepath.Join(cwd, ".enso"))
	cfgPath := filepath.Join(cwd, ".enso", "config.toml")
	mustWrite(t, cfgPath, `model = "x"`)

	if err := TrustFile(cfgPath); err != nil {
		t.Fatalf("trust: %v", err)
	}
	// Tamper with the file.
	mustWrite(t, cfgPath, `model = "y"`)

	got, err := CheckTrust(cwd)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 untrusted, got %d", len(got))
	}
	if got[0].PriorSHA256 == "" {
		t.Errorf("prior hash should be set on drift, got empty")
	}
	if got[0].SHA256 == got[0].PriorSHA256 {
		t.Errorf("new and prior hash should differ on drift")
	}
}

func TestRevokeTrust_RoundTrip(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", filepath.Join(tmp, "home"))

	cwd := filepath.Join(tmp, "proj")
	mustMkdir(t, filepath.Join(cwd, ".enso"))
	cfgPath := filepath.Join(cwd, ".enso", "config.toml")
	mustWrite(t, cfgPath, `model = "x"`)

	if err := TrustFile(cfgPath); err != nil {
		t.Fatalf("trust: %v", err)
	}
	ok, err := RevokeTrust(cfgPath)
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if !ok {
		t.Errorf("revoke should report removed=true on first call")
	}
	got, err := CheckTrust(cwd)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("after revoke want untrusted, got %v", got)
	}
	ok2, err := RevokeTrust(cfgPath)
	if err != nil {
		t.Fatalf("revoke2: %v", err)
	}
	if ok2 {
		t.Errorf("second revoke should report removed=false")
	}
}

func TestListTrusted_SortedByPath(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", filepath.Join(tmp, "home"))

	for _, name := range []string{"b", "a", "c"} {
		dir := filepath.Join(tmp, name, ".enso")
		mustMkdir(t, dir)
		p := filepath.Join(dir, "config.toml")
		mustWrite(t, p, "model = \""+name+"\"")
		if err := TrustFile(p); err != nil {
			t.Fatalf("trust %s: %v", p, err)
		}
	}
	entries, err := ListTrusted()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("want 3 entries, got %d", len(entries))
	}
	for i := 1; i < len(entries); i++ {
		if entries[i-1].Path > entries[i].Path {
			t.Errorf("entries not sorted: %q before %q", entries[i-1].Path, entries[i].Path)
		}
	}
}

func TestTrustProjectTier_TrustsExistingSkipsAbsent(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", filepath.Join(tmp, "home"))

	// Project with no .enso/ at all.
	empty := filepath.Join(tmp, "empty")
	mustMkdir(t, empty)
	got, err := TrustProjectTier(empty)
	if err != nil {
		t.Fatalf("trust empty: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("empty project should record nothing, got %v", got)
	}

	// Project with config.toml.
	full := filepath.Join(tmp, "full")
	mustMkdir(t, filepath.Join(full, ".enso"))
	mustWrite(t, filepath.Join(full, ".enso", "config.toml"), `model = "x"`)
	got, err = TrustProjectTier(full)
	if err != nil {
		t.Fatalf("trust full: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 trusted, got %d (%v)", len(got), got)
	}
	// Verify CheckTrust now returns clean.
	un, err := CheckTrust(full)
	if err != nil {
		t.Fatalf("recheck: %v", err)
	}
	if len(un) != 0 {
		t.Errorf("expected trusted after TrustProjectTier, got %v", un)
	}
}

func TestTrustStore_FileMode0600(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", filepath.Join(tmp, "home"))

	cwd := filepath.Join(tmp, "proj")
	mustMkdir(t, filepath.Join(cwd, ".enso"))
	p := filepath.Join(cwd, ".enso", "config.toml")
	mustWrite(t, p, `model = "x"`)
	if err := TrustFile(p); err != nil {
		t.Fatalf("trust: %v", err)
	}
	store, err := trustStorePath()
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(store)
	if err != nil {
		t.Fatalf("stat store: %v", err)
	}
	if mode := fi.Mode().Perm(); mode != 0o600 {
		t.Errorf("trust.json mode = %o, want 0600", mode)
	}
}
