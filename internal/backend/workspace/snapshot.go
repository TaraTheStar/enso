// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
)

// Snapshot/restore is the per-turn checkpoint substrate for /rewind. It
// reuses the same three-way engine primitives the overlay uses (clone /
// scan / sigOf / the containment guard) but stands alone: a snapshot is
// a plain directory holding the project tree at a moment in time, and a
// restore mirrors it back over the working tree.
//
// Unlike the overlay, snapshots exclude `.git` entirely — they are NOT
// copied into the snapshot and are NEVER touched on restore — so a
// rewind reverts the agent's working-tree changes without rewriting (or
// bloating snapshots with) git internals, and the user's git history is
// left to git. The `cp --reflink=auto` clone is near-free on CoW
// filesystems; on others it's a real copy, the same cost the overlay
// already pays.

// snapshotExclude lists top-level names skipped by SnapshotTree and the
// scan/diff in RestoreTree. Mirrors the overlay's `.git` exclusion.
var snapshotExclude = map[string]bool{".git": true}

// SnapshotTree clones src's contents into a fresh snapshot directory dst
// (created if absent), skipping top-level snapshotExclude entries
// (`.git`). dst must not already contain a snapshot; callers pass a
// fresh per-turn path. Reflinks on CoW filesystems.
func SnapshotTree(ctx context.Context, src, dst string) error {
	abs, err := filepath.Abs(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return fmt.Errorf("workspace: snapshot mkdir: %w", err)
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return fmt.Errorf("workspace: snapshot read src: %w", err)
	}
	for _, e := range entries {
		if snapshotExclude[e.Name()] {
			continue
		}
		// cp -a per top-level entry: preserves mode/symlinks/dotfiles,
		// reflinks on CoW. Copying entries individually (rather than
		// `src/.`) is what lets us skip .git without copy-then-delete.
		c := exec.CommandContext(ctx, "cp", "-a", "--reflink=auto", "--",
			filepath.Join(abs, e.Name()), dst)
		if out, err := c.CombinedOutput(); err != nil {
			return fmt.Errorf("workspace: snapshot %s: %v\n%s", e.Name(), err, out)
		}
	}
	return nil
}

// RestoreTree mirrors the snapshot at snap back over project, reverting
// the working tree to the snapshot's state: every path that differs is
// either copied from the snapshot (created/modified files) or deleted
// from the project (paths the agent added since the snapshot). `.git` is
// excluded on both sides, so git internals are never read or written.
// Each destination is containment-checked (lexical + symlink) before any
// cp/RemoveAll, exactly like the overlay's per-file commit. Returns the
// sorted set of changed relative paths it touched.
func RestoreTree(ctx context.Context, snap, project string) ([]string, error) {
	absProj, err := filepath.Abs(project)
	if err != nil {
		return nil, err
	}
	sSnap, err := scan(snap)
	if err != nil {
		return nil, fmt.Errorf("workspace: restore scan snapshot: %w", err)
	}
	sProj, err := scan(absProj)
	if err != nil {
		return nil, fmt.Errorf("workspace: restore scan project: %w", err)
	}

	changed := map[string]struct{}{}
	for k, v := range sSnap {
		if sProj[k] != v {
			changed[k] = struct{}{}
		}
	}
	for k := range sProj {
		if _, ok := sSnap[k]; !ok {
			changed[k] = struct{}{}
		}
	}
	rels := make([]string, 0, len(changed))
	for k := range changed {
		rels = append(rels, k)
	}
	sort.Strings(rels)

	for _, rel := range rels {
		dst, err := containedPath(absProj, rel)
		if err != nil {
			return nil, err
		}
		src := filepath.Join(snap, rel)
		if _, ok := sSnap[rel]; ok {
			// Present in the snapshot → restore it (create/revert).
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return nil, fmt.Errorf("workspace: restore %s: %w", rel, err)
			}
			c := exec.CommandContext(ctx, "cp", "-a", "--reflink=auto", "-f", "--", src, dst)
			if out, err := c.CombinedOutput(); err != nil {
				return nil, fmt.Errorf("workspace: restore %s: %v\n%s", rel, err, out)
			}
		} else {
			// Absent in the snapshot → the agent added it after the
			// snapshot; remove it to match the snapshot state.
			if err := os.RemoveAll(dst); err != nil {
				return nil, fmt.Errorf("workspace: restore delete %s: %w", rel, err)
			}
			pruneEmptyParents(absProj, filepath.Dir(dst))
		}
	}
	return rels, nil
}
