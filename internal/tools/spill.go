// SPDX-License-Identifier: AGPL-3.0-or-later

package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// SpillWriter persists a tool's full pre-truncation output to disk and
// returns a path the model can pass to the `read` tool for recovery.
// AgentContext.Spill is nil in tests and in subagent contexts where
// recovery doesn't make sense; truncateWithRecovery degrades to plain
// truncation in that case.
type SpillWriter interface {
	// Spill writes content and returns an absolute path. Implementations
	// may dedupe by content hash so identical outputs share a file.
	Spill(content string) (path string, err error)
}

// FileSpill writes spilled tool outputs under a session-scoped subdir
// of the configured root (typically $XDG_STATE_HOME/enso/truncated).
// Filenames are sha256[:16] of the content so identical outputs from
// different tool calls share storage and the path stays stable across
// re-runs of the same command.
type FileSpill struct {
	// Root is the directory under which session subdirs are created.
	// Typically <paths.StateDir>/truncated.
	Root string
	// SessionID scopes the spill subdir; lets SweepSession remove one
	// session's output without touching siblings.
	SessionID string
}

// Spill implements SpillWriter. The returned path is absolute, lives
// under Root/<session>/, and is named by sha256[:16] of the content
// with a .txt suffix so it opens in pagers without prompting.
func (f *FileSpill) Spill(content string) (string, error) {
	if f == nil || f.Root == "" || f.SessionID == "" {
		return "", fmt.Errorf("spill: misconfigured (root=%q session=%q)", f.Root, f.SessionID)
	}
	dir := filepath.Join(f.Root, f.SessionID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("spill mkdir: %w", err)
	}
	sum := sha256.Sum256([]byte(content))
	name := hex.EncodeToString(sum[:8]) + ".txt" // 16 hex chars
	path := filepath.Join(dir, name)
	// Skip the write if a previous call already produced the same
	// content — saves a syscall and keeps mtime stable for sweep.
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("spill write: %w", err)
	}
	return path, nil
}

// SweepSpills removes spilled tool-output files older than maxAge
// across all session subdirs of root. Called from agent.New on a
// best-effort basis (errors are logged, not returned) so a transient
// I/O failure can't block agent startup.
//
// Picks 7 days as the default age, matching opencode. Files younger
// than that are preserved so an in-flight session can still recover.
func SweepSpills(root string, maxAge time.Duration) (removed int, err error) {
	if root == "" {
		return 0, nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	cutoff := time.Now().Add(-maxAge)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		subdir := filepath.Join(root, e.Name())
		files, ferr := os.ReadDir(subdir)
		if ferr != nil {
			continue
		}
		empty := true
		for _, f := range files {
			fp := filepath.Join(subdir, f.Name())
			info, ierr := f.Info()
			if ierr != nil {
				empty = false
				continue
			}
			if info.ModTime().After(cutoff) {
				empty = false
				continue
			}
			if rmErr := os.Remove(fp); rmErr == nil {
				removed++
			} else {
				empty = false
			}
		}
		// Tidy the empty per-session subdir so a long-quiet host
		// doesn't accumulate thousands of stale dirs.
		if empty {
			_ = os.Remove(subdir)
		}
	}
	return removed, nil
}

// DefaultSpillMaxAge is the TTL applied by SweepSpills when callers
// don't specify one. 7 days matches opencode's tool-output retention.
const DefaultSpillMaxAge = 7 * 24 * time.Hour
