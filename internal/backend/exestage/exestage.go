// SPDX-License-Identifier: AGPL-3.0-or-later

// Package exestage gives isolated backends (lima/podman) an IMMUTABLE,
// content-addressed copy of the enso binary to mount/exec, instead of
// the host's live build-output file.
//
// Why this exists: the lima backend 9p-mounts the binary's directory
// read-only into a PERSISTENT VM and execs it 1:1; podman bind-mounts
// the file. If the host binary is rebuilt in place (the Go toolchain
// O_TRUNC-overwrites the output — `make build`/`make install`/`go
// install`) while a guest worker still has it mmap'd, 9p/virtfs faults
// in a mix of old+new pages and the Go runtime's pclntab spans
// corrupted pages, yielding "fatal error: invalid runtime symbol
// table". A
// content-addressed snapshot is never overwritten (a rebuilt host
// binary hashes differently, getting a brand-new <hash>/enso path), so
// an in-flight worker keeps executing its own stable copy.
//
// Stage returns TWO paths: the content-addressed EXEC path
// (<root>/<hash>/enso — what the guest runs / podman bind-mounts) and
// the STABLE mount ROOT (<root>, parent of every snapshot). The lima
// backend mounts the ROOT, which is constant across host rebuilds, so
// the generated VM YAML never changes and the persistent per-project
// VM is NOT drift-recreated on every `make build` (which otherwise
// costs a full cold boot — ~10s of Alpine bootloader + reprovision —
// per dev iteration). Freshness still holds: a rebuilt binary appears
// as a NEW <hash> subdir under the already-9p-mounted root, so the
// next `limactl shell` execs the new code without a remount, while old
// snapshots stay immutable for any worker still mmap'ing them. The
// tradeoff is a slightly wider mount (the whole exe/ tree — every
// snapshot, all just enso binaries, no secrets; reclaimed by `enso
// prune`).
package exestage

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/TaraTheStar/enso/internal/paths"
)

// dirName is the stable parent under $XDG_STATE_HOME/enso.
const dirName = "exe"

// Stage returns an immutable copy of exe at
// $XDG_STATE_HOME/enso/exe/<sha256[:16]>/enso (the exec path), plus the
// stable parent root $XDG_STATE_HOME/enso/exe (the lima mount point —
// constant across rebuilds; see the package doc). Copying happens once
// if absent. Idempotent and content-addressed: identical binary content
// always maps to the same exec path and is copied at most once; a
// different (rebuilt) binary gets a brand-new path and never overwrites
// an existing snapshot a running guest may still be executing.
func Stage(exe string) (execPath, root string, err error) {
	abs, err := filepath.Abs(exe)
	if err != nil {
		return "", "", err
	}
	sum, err := hashFile(abs)
	if err != nil {
		return "", "", fmt.Errorf("exestage: hash %s: %w", abs, err)
	}
	sd, err := paths.StateDir()
	if err != nil {
		return "", "", err
	}
	root = filepath.Join(sd, dirName)
	dir := filepath.Join(root, sum)
	dst := filepath.Join(dir, "enso")
	if fi, statErr := os.Stat(dst); statErr == nil && fi.Mode().IsRegular() {
		// Already staged (same content) — touch the dir mtime so Sweep's
		// age threshold tracks last use, then reuse.
		_ = os.Chtimes(dir, time.Now(), time.Now())
		return dst, root, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", err
	}
	// Atomic publish: copy to a temp sibling, fsync, chmod, rename. A
	// concurrent Stage of the same content either wins the rename or
	// finds dst already present — both fine (content-identical).
	tmp, err := os.CreateTemp(dir, ".enso-*")
	if err != nil {
		return "", "", err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	src, err := os.Open(abs)
	if err != nil {
		tmp.Close()
		return "", "", err
	}
	if _, err := io.Copy(tmp, src); err != nil {
		src.Close()
		tmp.Close()
		return "", "", err
	}
	src.Close()
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", "", err
	}
	if err := tmp.Close(); err != nil {
		return "", "", err
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		return "", "", err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		// Lost a race to a concurrent Stage of identical content, or
		// the snapshot now exists — accept it if dst is present.
		if fi, statErr := os.Stat(dst); statErr == nil && fi.Mode().IsRegular() {
			return dst, root, nil
		}
		return "", "", err
	}
	return dst, root, nil
}

// hashFile returns the first 16 hex chars of the file's SHA-256 — a
// stable, collision-safe-enough content key for a local cache dir.
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil))[:16], nil
}

// Sweep removes staged binary snapshots. olderThan>0 keeps any whose
// directory mtime is within the window (recently used — a persistent
// VM may still 9p-mount it); 0 removes all. The `enso prune` backstop;
// never called implicitly. Best-effort; returns how many were removed.
func Sweep(olderThan time.Duration) (int, error) {
	sd, err := paths.StateDir()
	if err != nil {
		return 0, err
	}
	base := filepath.Join(sd, dirName)
	entries, err := os.ReadDir(base)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	cutoff := time.Time{}
	if olderThan > 0 {
		cutoff = time.Now().Add(-olderThan)
	}
	removed := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p := filepath.Join(base, e.Name())
		if !cutoff.IsZero() {
			if fi, statErr := os.Stat(p); statErr == nil && fi.ModTime().After(cutoff) {
				continue // recently used — a running VM may still mount it
			}
		}
		if os.RemoveAll(p) == nil {
			removed++
		}
	}
	return removed, nil
}
