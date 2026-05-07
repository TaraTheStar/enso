// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Trust gates loading of project-tier config files. The threat model is
// `git clone <hostile-repo> && cd <hostile-repo> && enso` — without a gate,
// a committed `.enso/config.toml` can inject arbitrary commands via
// [hooks], [lsp.*].command, [mcp.*].command, override providers/api_key,
// disable the bash sandbox, mount the host root, etc. The user-tier config
// (~/.config/enso) and -c flag files are user-controlled and so always
// trusted; system-tier (/etc) is admin-controlled and trusted.
//
// Only `<cwd>/.enso/config.toml` is gated. `config.local.toml` is gitignored
// (so it isn't part of the cloned hostile content) and is rewritten by
// enso itself on every "Allow + Remember" — gating it would mean constant
// trust drift with no real defence-in-depth.

// TrustEntry records that the file at Path was trusted at TrustedAt with
// contents hashing to SHA256. If a future load sees the file with a
// different hash (or no entry at all), it's treated as untrusted.
type TrustEntry struct {
	Path      string    `json:"path"`
	SHA256    string    `json:"sha256"`
	TrustedAt time.Time `json:"trusted_at"`
}

type trustStore struct {
	Version int                   `json:"version"`
	Entries map[string]TrustEntry `json:"entries"`
}

const trustVersion = 1

// trustStorePath returns ~/.enso/trust.json.
func trustStorePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("get home dir: %w", err)
	}
	return filepath.Join(home, ".enso", "trust.json"), nil
}

func loadTrustStore() (*trustStore, error) {
	path, err := trustStorePath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &trustStore{Version: trustVersion, Entries: map[string]TrustEntry{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var ts trustStore
	if err := json.Unmarshal(data, &ts); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if ts.Entries == nil {
		ts.Entries = map[string]TrustEntry{}
	}
	return &ts, nil
}

func saveTrustStore(ts *trustStore) error {
	path, err := trustStorePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	out, err := json.MarshalIndent(ts, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename %s -> %s: %w", tmp, path, err)
	}
	return nil
}

// hashFile returns the SHA-256 hex digest of the file at path.
func hashFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// gatedProjectPaths returns the project-tier config files that require the
// trust gate for cwd. Currently just `<cwd>/.enso/config.toml`.
func gatedProjectPaths(cwd string) []string {
	if cwd == "" {
		return nil
	}
	return []string{filepath.Join(cwd, ".enso", "config.toml")}
}

// UntrustedConfig describes a project-tier config file that exists on disk
// but is not currently trusted.
type UntrustedConfig struct {
	Path   string
	SHA256 string
	// PriorSHA256, if non-empty, means the file used to be trusted at
	// this hash and has since changed.
	PriorSHA256 string
}

// CheckTrust returns the gated project-tier config files for cwd that
// exist but are not currently trusted. An empty slice means safe-to-load.
func CheckTrust(cwd string) ([]UntrustedConfig, error) {
	store, err := loadTrustStore()
	if err != nil {
		return nil, err
	}
	var out []UntrustedConfig
	for _, p := range gatedProjectPaths(cwd) {
		abs, err := filepath.Abs(p)
		if err != nil {
			return nil, fmt.Errorf("resolve %s: %w", p, err)
		}
		hash, err := hashFile(abs)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("hash %s: %w", abs, err)
		}
		entry, ok := store.Entries[abs]
		switch {
		case !ok:
			out = append(out, UntrustedConfig{Path: abs, SHA256: hash})
		case entry.SHA256 != hash:
			out = append(out, UntrustedConfig{Path: abs, SHA256: hash, PriorSHA256: entry.SHA256})
		}
	}
	return out, nil
}

// TrustFile records the current contents of path as trusted.
func TrustFile(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", path, err)
	}
	hash, err := hashFile(abs)
	if err != nil {
		return fmt.Errorf("hash %s: %w", abs, err)
	}
	store, err := loadTrustStore()
	if err != nil {
		return err
	}
	store.Version = trustVersion
	store.Entries[abs] = TrustEntry{
		Path:      abs,
		SHA256:    hash,
		TrustedAt: time.Now().UTC(),
	}
	return saveTrustStore(store)
}

// RevokeTrust removes path from the trust store. Returns (true, nil) if an
// entry was found and removed, (false, nil) if not present.
func RevokeTrust(path string) (bool, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return false, fmt.Errorf("resolve %s: %w", path, err)
	}
	store, err := loadTrustStore()
	if err != nil {
		return false, err
	}
	if _, ok := store.Entries[abs]; !ok {
		return false, nil
	}
	delete(store.Entries, abs)
	if err := saveTrustStore(store); err != nil {
		return false, err
	}
	return true, nil
}

// ListTrusted returns all trust entries sorted by path.
func ListTrusted() ([]TrustEntry, error) {
	store, err := loadTrustStore()
	if err != nil {
		return nil, err
	}
	out := make([]TrustEntry, 0, len(store.Entries))
	for _, e := range store.Entries {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// TrustProjectTier records every gated project-tier config file under cwd
// as trusted, returning the absolute paths of files that were recorded.
// Used by `enso trust [path]`.
func TrustProjectTier(cwd string) ([]string, error) {
	var trusted []string
	for _, p := range gatedProjectPaths(cwd) {
		if _, err := os.Stat(p); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return nil, fmt.Errorf("stat %s: %w", p, err)
		}
		if err := TrustFile(p); err != nil {
			return nil, err
		}
		abs, _ := filepath.Abs(p)
		trusted = append(trusted, abs)
	}
	return trusted, nil
}
