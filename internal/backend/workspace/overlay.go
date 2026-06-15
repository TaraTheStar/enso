// SPDX-License-Identifier: AGPL-3.0-or-later

// Package workspace implements the throwaway workspace overlay.
//
// Rather than podman's `:O` (which hides and unconditionally discards
// its upper layer — destroying the agent's legitimate work), the
// overlay is a host-controlled THREE-WAY copy. At task start the
// project is cloned (`cp --reflink=auto`, near-free on CoW
// filesystems) into two trees:
//
//   - base/   — a pristine snapshot of the project at clone time,
//     never mutated; the merge baseline.
//   - merged/ — what the agent works in (bind-mounted at the
//     project's REAL path inside the box, so the
//     one-filesystem-namespace property holds and the real
//     project is untouched while the agent runs).
//
// At task end a three-way compare separates what the AGENT changed
// (base→merged) from what the HOST changed concurrently (base→project).
// Non-conflicting agent changes can be applied per file; files both
// sides touched are CONFLICTS and are never silently clobbered — the
// copy + baseline are kept for manual merge unless the user explicitly
// forces. Commit is per-file (create/modify/delete), never a blind
// `rsync --delete` from a stale snapshot. `.git` is excluded from the
// compare: the overlay syncs working-tree files, not git internals,
// and ordinary git churn would otherwise dominate the result.
//
// Sub-agents share the one worker, hence the one copy — a single
// overlay per task, as designed.
package workspace

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

// Overlay is one task's throwaway working copy of a project.
type Overlay struct {
	Project string // real project dir (never mutated until apply)
	Copy    string // merged/: the bind-mount source; the agent works here
	base    string // pristine project-at-clone baseline (never mutated)
	root    string // dir removed by Cleanup (tmpdir for New)
	kept    bool   // KeepPath was called: Cleanup is a hard no-op
}

// clone copies src's CONTENTS into dst (which must already exist),
// reflinking on CoW filesystems and doing a faithful full copy
// elsewhere. `<src>/.` includes dotfiles.
func clone(ctx context.Context, src, dst string) error {
	cmd := exec.CommandContext(ctx, "cp", "-a", "--reflink=auto", src+"/.", dst)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("workspace: clone %s: %v\n%s", src, err, out)
	}
	return nil
}

// New clones project into a fresh throwaway directory and returns the
// overlay. base/ and merged/ both start as exact copies of the project
// (merged is cloned FROM base so base is provably merged's baseline).
func New(ctx context.Context, project string) (*Overlay, error) {
	abs, err := filepath.Abs(project)
	if err != nil {
		return nil, err
	}
	root, err := os.MkdirTemp("", "enso-workspace-")
	if err != nil {
		return nil, err
	}
	base := filepath.Join(root, "base")
	merged := filepath.Join(root, "merged")
	for _, d := range []string{base, merged} {
		if err := os.Mkdir(d, 0o755); err != nil {
			_ = os.RemoveAll(root)
			return nil, err
		}
	}
	if err := clone(ctx, abs, base); err != nil {
		_ = os.RemoveAll(root)
		return nil, err
	}
	if err := clone(ctx, base, merged); err != nil {
		_ = os.RemoveAll(root)
		return nil, err
	}
	return &Overlay{Project: abs, Copy: merged, base: base, root: root}, nil
}

// NewAt is New for a caller-chosen stable directory instead of a fresh
// os.MkdirTemp. BackendLima needs this: a persistent per-project VM
// declares its mounts once, so the host path mounted at the project's
// real path must be STABLE across tasks (a random per-task tmpdir would
// force a VM reconfigure/restart every task and defeat the
// persistent-substrate perf win). NewAt makes the overlay live at
// stageDir/{base,merged}; only their CONTENTS are refreshed per task.
//
// Safety: if stageDir/merged already holds a diverged copy a prior
// NON-interactive run kept for review, it is renamed aside to
// merged.kept-<unixnano> (reported on out) BEFORE re-cloning — a stable
// directory never silently destroys work the user was told to review.
// To bound the resulting accumulation, only the KeptCap most recent
// merged.kept-* are retained; older ones are pruned (reported on out).
// The stale base/ is just the old baseline (not user data) and is
// removed.
func NewAt(ctx context.Context, project, stageDir string, out io.Writer) (*Overlay, error) {
	abs, err := filepath.Abs(project)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(stageDir, 0o755); err != nil {
		return nil, err
	}
	base := filepath.Join(stageDir, "base")
	merged := filepath.Join(stageDir, "merged")

	// INODE STABILITY (load-bearing): a persistent Lima VM 9p-exports
	// `merged` by the directory inode it had at VM boot — qemu virtfs
	// does NOT re-resolve the path. So once the VM exists, `merged` must
	// keep its inode forever; we only ever refresh its CONTENTS in place
	// (qemu 9p resolves children by name on each access). Renaming or
	// removing the dir itself would strand the reused guest mount on the
	// orphaned old inode: empty project, writes lost, Resolve sees
	// nothing. So: MkdirAll (create once, reused thereafter), never
	// rename/rmdir `merged`.
	if err := os.MkdirAll(merged, 0o755); err != nil {
		return nil, err
	}
	// A prior non-empty `merged` (a NON-interactive run kept it for
	// review) is preserved by moving its CONTENTS aside, not the dir.
	if entries, _ := os.ReadDir(merged); len(entries) > 0 {
		kept := filepath.Join(stageDir, fmt.Sprintf("merged.kept-%d", time.Now().UnixNano()))
		moved := false
		if os.MkdirAll(kept, 0o755) == nil {
			for _, e := range entries {
				if os.Rename(filepath.Join(merged, e.Name()), filepath.Join(kept, e.Name())) == nil {
					moved = true
				}
			}
		}
		if moved && out != nil {
			fmt.Fprintf(out, "workspace: a prior kept-for-review copy was preserved at %s\n", kept)
		} else if !moved {
			_ = os.RemoveAll(kept)
		}
	}
	clearDirContents(merged) // any leftover (partial move) — inode kept
	pruneKept(stageDir, KeptCap, out)

	// base is host-only (never 9p-exported) → free to recreate.
	_ = os.RemoveAll(base)
	if err := os.MkdirAll(base, 0o755); err != nil {
		return nil, err
	}
	if err := clone(ctx, abs, base); err != nil {
		return nil, err
	}
	if err := clone(ctx, base, merged); err != nil { // into the inode-stable dir
		return nil, err
	}
	// root="" : Cleanup clears merged IN PLACE (inode preserved for the
	// persistent VM) and removes base; it never rmdir's merged.
	return &Overlay{Project: abs, Copy: merged, base: base, root: ""}, nil
}

// clearDirContents removes every entry inside dir but not dir itself,
// preserving dir's inode (required for the persistent-VM 9p export).
func clearDirContents(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		_ = os.RemoveAll(filepath.Join(dir, e.Name()))
	}
}

// KeptCap bounds how many superseded merged.kept-* review copies a
// stable stage dir retains. Newer ones are the ones a user is likely
// still reviewing; very old ones are almost certainly abandoned.
const KeptCap = 3

// PruneKept deletes all but the `keep` most recent merged.kept-* (by
// mtime) directly under stageDir, reporting removals on out (may be
// nil). Best-effort. Exported so `enso prune` can sweep stale
// review copies across every project stage dir as a manual backstop.
func PruneKept(stageDir string, keep int, out io.Writer) {
	matches, _ := filepath.Glob(filepath.Join(stageDir, "merged.kept-*"))
	if len(matches) <= keep {
		return
	}
	type ent struct {
		path string
		mod  time.Time
	}
	ents := make([]ent, 0, len(matches))
	for _, m := range matches {
		fi, err := os.Stat(m)
		if err != nil {
			continue
		}
		ents = append(ents, ent{m, fi.ModTime()})
	}
	if len(ents) <= keep {
		return
	}
	sort.Slice(ents, func(i, j int) bool { return ents[i].mod.After(ents[j].mod) })
	for _, e := range ents[keep:] {
		if os.RemoveAll(e.path) == nil && out != nil {
			fmt.Fprintf(out, "workspace: pruned stale review copy %s\n", e.path)
		}
	}
}

// pruneKept is the internal wrapper NewAt uses.
func pruneKept(stageDir string, keep int, out io.Writer) { PruneKept(stageDir, keep, out) }

// --- three-way engine -------------------------------------------------

// sigOf is a cheap O(stat) signature: symlink target, or for a regular
// file size+mtime(ns)+perm, or a type tag. `cp -a` preserves
// size/mtime/mode through project→base→merged, so an untouched file
// has an identical signature on all three sides and any real write
// (agent or host) moves mtime (and usually size). This deliberately
// does NOT hash file contents: hashing base+merged+project in full on
// every resolve is O(all bytes × 3) and dominates large trees
// (node_modules, build artifacts) for no benefit under this threat
// model (the agent's own mistakes, not mtime-forging evasion). The
// only blind spot — a content change that preserves size AND mtime —
// is the same fidelity the pre-three-way overlay already accepted.
func sigOf(path string, d fs.DirEntry) string {
	info, err := d.Info()
	if err != nil {
		return "?err"
	}
	mode := info.Mode()
	switch {
	case mode&fs.ModeSymlink != 0:
		t, _ := os.Readlink(path)
		return "L:" + t
	case mode.IsRegular():
		return fmt.Sprintf("F:%d:%d:%o", info.Size(), info.ModTime().UnixNano(), mode.Perm())
	default:
		return "T:" + mode.String()
	}
}

// scan maps every non-dir path under dir (relative, .git excluded) to
// its signature. A missing dir scans as empty.
func scan(dir string) (map[string]string, error) {
	m := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, e error) error {
		if e != nil {
			if p == dir {
				return e
			}
			return nil // skip unreadable entry, keep going
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, rerr := filepath.Rel(dir, p)
		if rerr != nil {
			return nil
		}
		m[rel] = sigOf(p, d)
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	return m, nil
}

// threeWay returns the agent's changes (base→merged), the concurrent
// host drift (base→project), and their intersection (conflicts) — all
// as sorted relative paths.
func (o *Overlay) threeWay() (agent, drift, conflicts []string, err error) {
	sBase, err := scan(o.base)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("workspace: scan base: %w", err)
	}
	sMerged, err := scan(o.Copy)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("workspace: scan copy: %w", err)
	}
	sProj, err := scan(o.Project)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("workspace: scan project: %w", err)
	}
	diffKeys := func(a, b map[string]string) map[string]struct{} {
		out := map[string]struct{}{}
		for k, v := range a {
			if b[k] != v {
				out[k] = struct{}{}
			}
		}
		for k, v := range b {
			if a[k] != v {
				out[k] = struct{}{}
			}
		}
		return out
	}
	aSet := diffKeys(sBase, sMerged)
	dSet := diffKeys(sBase, sProj)
	for k := range aSet {
		agent = append(agent, k)
		if _, ok := dSet[k]; ok {
			conflicts = append(conflicts, k)
		}
	}
	for k := range dSet {
		drift = append(drift, k)
	}
	sort.Strings(agent)
	sort.Strings(drift)
	sort.Strings(conflicts)
	return agent, drift, conflicts, nil
}

// applyPaths mirrors each rel path from merged onto the project: copy
// (create/modify) when it exists in merged, delete when the agent
// removed it. Per-file — never a blanket rsync --delete — so files NOT
// in the set (incl. unrelated host work) are untouched.
//
// Each mutation is fd-anchored under the project root with O_NOFOLLOW on
// every path component (safeCopyInto / safeDelete), so a symlink swapped
// into the parent chain after the lexical containment check can't
// redirect the write/delete outside the project (closes the
// check→operate TOCTOU the old `cp`/`RemoveAll` execs had). Writes are
// stage-and-rename, so each file applies atomically — a crash or error
// mid-loop never leaves a half-written destination (though the set as a
// whole is not transactional: earlier files in rels stay applied).
func (o *Overlay) applyPaths(ctx context.Context, rels []string) error {
	for _, rel := range rels {
		if err := ctx.Err(); err != nil {
			return err
		}
		src := filepath.Join(o.Copy, rel)
		// Lexical + existing-symlink containment (S10), defence in depth;
		// the fd-anchored ops below independently refuse symlink traversal.
		if _, err := o.containedDst(rel); err != nil {
			return err
		}
		fi, statErr := os.Lstat(src)
		if statErr == nil {
			if err := safeCopyInto(o.Project, rel, src, fi); err != nil {
				return fmt.Errorf("workspace: apply %s: %w", rel, err)
			}
		} else {
			// Agent deleted it.
			if err := safeDelete(o.Project, rel); err != nil {
				return fmt.Errorf("workspace: delete %s: %w", rel, err)
			}
			pruneEmptyParents(o.Project, filepath.Dir(filepath.Join(o.Project, rel)))
		}
	}
	return nil
}

// applyTmpSeq names staging files uniquely within a destination dir.
var applyTmpSeq atomic.Uint64

func applyTmpName() string {
	return fmt.Sprintf(".enso-apply.%d.%d.tmp", os.Getpid(), applyTmpSeq.Add(1))
}

// openDirNoFollow opens the directory root/relDir and returns its fd. The
// root itself is trusted and opened normally; every component BELOW it is
// opened with O_NOFOLLOW so a symlink anywhere in the chain fails the
// open (ELOOP) instead of redirecting the caller outside root. When
// create is true missing intermediate dirs are made (0o755, matching the
// old MkdirAll); when false a missing component surfaces as
// fs.ErrNotExist. Caller must Close the returned fd.
func openDirNoFollow(root, relDir string, create bool) (int, error) {
	fd, err := unix.Open(root, unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("open root: %w", err)
	}
	if relDir == "" || relDir == "." {
		return fd, nil
	}
	for part := range strings.SplitSeq(filepath.ToSlash(relDir), "/") {
		if part == "" || part == "." {
			continue
		}
		if create {
			if err := unix.Mkdirat(fd, part, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
				_ = unix.Close(fd)
				return -1, fmt.Errorf("mkdir %s: %w", part, err)
			}
		}
		next, err := unix.Openat(fd, part, unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		_ = unix.Close(fd)
		if err != nil {
			return -1, fmt.Errorf("open dir %s: %w", part, err)
		}
		fd = next
	}
	return fd, nil
}

// safeCopyInto copies src (a regular file or symlink, described by fi)
// onto root/rel via a fd anchored at the destination's parent directory
// (see openDirNoFollow). It stages into a temp name in that dir and
// renameat's it over the destination, so the apply is atomic and never
// writes through a pre-existing symlink. Note: unlike the old
// `cp --reflink=auto`, this is a byte copy (no CoW reflink) — acceptable
// for the small agent-changed set this runs over. Mode and mtime are
// preserved for regular files.
func safeCopyInto(root, rel, src string, fi os.FileInfo) error {
	dirfd, err := openDirNoFollow(root, filepath.Dir(rel), true)
	if err != nil {
		return err
	}
	defer unix.Close(dirfd)
	base := filepath.Base(rel)
	tmp := applyTmpName()

	if fi.Mode()&fs.ModeSymlink != 0 {
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		if err := unix.Symlinkat(target, dirfd, tmp); err != nil {
			return fmt.Errorf("symlink: %w", err)
		}
		if err := unix.Renameat(dirfd, tmp, dirfd, base); err != nil {
			_ = unix.Unlinkat(dirfd, tmp, 0)
			return fmt.Errorf("rename: %w", err)
		}
		return nil
	}

	perm := fi.Mode().Perm()
	tfd, err := unix.Openat(dirfd, tmp,
		unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, uint32(perm))
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tf := os.NewFile(uintptr(tfd), tmp)
	in, err := os.Open(src)
	if err != nil {
		_ = tf.Close()
		_ = unix.Unlinkat(dirfd, tmp, 0)
		return err
	}
	_, cerr := io.Copy(tf, in)
	_ = in.Close()
	if cerr == nil {
		cerr = tf.Chmod(perm) // fchmod: O_CREAT mode is masked by umask
	}
	if ferr := tf.Close(); cerr == nil {
		cerr = ferr
	}
	if cerr != nil {
		_ = unix.Unlinkat(dirfd, tmp, 0)
		return cerr
	}
	// Preserve mtime so downstream build tools don't see a spurious
	// change; best-effort.
	ts := unix.NsecToTimespec(fi.ModTime().UnixNano())
	_ = unix.UtimesNanoAt(dirfd, tmp, []unix.Timespec{ts, ts}, unix.AT_SYMLINK_NOFOLLOW)

	if err := unix.Renameat(dirfd, tmp, dirfd, base); err != nil {
		_ = unix.Unlinkat(dirfd, tmp, 0)
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

// safeDelete removes root/rel via a parent-dir fd opened with O_NOFOLLOW
// on every component, so the unlink can't be redirected through a
// symlinked ancestor. A missing path (or missing parent) is a no-op.
// rels are always files; a directory that drifted in is removed only if
// empty (the conservative AT_REMOVEDIR fallback).
func safeDelete(root, rel string) error {
	dirfd, err := openDirNoFollow(root, filepath.Dir(rel), false)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil // nothing to delete under a missing parent
		}
		return err
	}
	defer unix.Close(dirfd)
	base := filepath.Base(rel)
	err = unix.Unlinkat(dirfd, base, 0)
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if errors.Is(err, unix.EISDIR) || errors.Is(err, unix.EPERM) {
		if err2 := unix.Unlinkat(dirfd, base, unix.AT_REMOVEDIR); err2 == nil || errors.Is(err2, fs.ErrNotExist) {
			return nil
		}
	}
	return err
}

// containedDst returns the absolute destination for rel under the
// project root, or an error if rel would escape it. Two layers:
//
//  1. Lexical: filepath.Rel(root, dst) must not start with "..", so a
//     `rel` carrying `..` segments can't climb above the root.
//  2. Symlink: the nearest EXISTING ancestor of dst, with symlinks
//     resolved, must stay within the resolved root — so a symlink
//     already present in the project tree (e.g. a hostile `docs ->
//     /etc`) can't be used as a springboard to write/delete outside it.
//     dst itself need not exist (we're often creating it); we walk up to
//     the first ancestor that does.
func (o *Overlay) containedDst(rel string) (string, error) {
	return containedPath(o.Project, rel)
}

// containedPath is the free-function form of containedDst: it resolves
// rel under root with the same two-layer (lexical + symlink) escape
// guard, returning the absolute destination or an error. Shared with the
// checkpoint snapshot/restore engine (RestoreTree), which mirrors files
// back over an arbitrary project root.
func containedPath(root, rel string) (string, error) {
	dst := filepath.Join(root, rel)

	rl, err := filepath.Rel(root, dst)
	if err != nil || rl == ".." || strings.HasPrefix(rl, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("workspace: refusing path outside project: %s", rel)
	}

	rootResolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("workspace: resolve project root: %w", err)
	}
	anc := dst
	for {
		parent := filepath.Dir(anc)
		resolved, err := filepath.EvalSymlinks(parent)
		if err == nil {
			rl, rerr := filepath.Rel(rootResolved, resolved)
			if rerr != nil || rl == ".." || strings.HasPrefix(rl, ".."+string(os.PathSeparator)) {
				return "", fmt.Errorf("workspace: refusing path that escapes project via symlink: %s", rel)
			}
			return dst, nil
		}
		// parent doesn't exist yet — climb to the next existing ancestor.
		if parent == anc {
			// Reached the filesystem root without finding an existing
			// ancestor (shouldn't happen: root exists), fail closed.
			return "", fmt.Errorf("workspace: cannot resolve ancestor of %s", rel)
		}
		anc = parent
	}
}

// pruneEmptyParents removes now-empty directories from leaf up to (not
// including) root. Best-effort.
func pruneEmptyParents(root, dir string) {
	for dir != root && strings.HasPrefix(dir, root+string(os.PathSeparator)) {
		if entries, err := os.ReadDir(dir); err != nil || len(entries) > 0 {
			return
		}
		if os.Remove(dir) != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}

// --- human-readable diff (base→merged: what the AGENT did) ------------

const maxSummaryLines = 120

func (o *Overlay) rawAgentDiff(ctx context.Context) (string, error) {
	// base vs merged = exactly the agent's work, independent of any
	// concurrent host drift. -ruN so adds/deletes show as full hunks;
	// -x .git to match the engine's exclusion.
	cmd := exec.CommandContext(ctx, "diff", "-ruN", "-x", ".git", "--", o.base, o.Copy)
	b, derr := cmd.CombinedOutput()
	if derr == nil {
		return "", nil
	}
	if ee, ok := derr.(*exec.ExitError); ok && ee.ExitCode() == 1 {
		s := strings.ReplaceAll(string(b), o.base+"/", "")
		return strings.ReplaceAll(s, o.Copy+"/", ""), nil
	}
	return "", fmt.Errorf("workspace: diff: %v\n%s", derr, b)
}

func clip(s string, max int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= max {
		return s
	}
	return strings.Join(lines[:max], "\n") +
		fmt.Sprintf("\n… (%d more lines — choose [v]iew for the full diff)", len(lines)-max)
}

// Discard deletes the working copy + baseline; project untouched.
func (o *Overlay) Discard() error { return o.Cleanup() }

// Cleanup discards the throwaway trees. Idempotent. After KeepPath it
// is a HARD no-op — the copy + baseline the user was told are safe stay
// on disk.
//
//   - New() (root != ""): a private tmpdir, no persistent VM pinned to
//     it — remove the whole tree.
//   - NewAt() (root == ""): a persistent Lima VM 9p-exports o.Copy by
//     inode, so o.Copy's CONTENTS are cleared but the directory itself
//     is NEVER removed (rmdir'ing it would strand the reused guest
//     mount on a dangling inode). base is host-only → removed.
func (o *Overlay) Cleanup() error {
	if o.kept {
		return nil
	}
	var firstErr error
	if o.root != "" {
		if err := os.RemoveAll(o.root); err != nil {
			firstErr = err
		}
		o.root = ""
	} else if o.Copy != "" {
		clearDirContents(o.Copy) // preserve the inode-stable dir
	}
	if o.base != "" {
		if err := os.RemoveAll(o.base); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	o.base = ""
	return firstErr
}

// KeepPath marks the overlay kept (Cleanup becomes a hard no-op) and
// returns where the diverged copy lives, for the caller to tell the
// user. The baseline is kept too (needed for a manual three-way
// merge).
func (o *Overlay) KeepPath() string {
	o.kept = true
	return o.Copy
}

// Resolve runs the end-of-task decision. No agent changes → silent
// cleanup. Otherwise: non-interactive KEEPS (never silent
// commit/destroy) and prints how to reconcile; interactive offers
// commit (non-conflicting changes applied per-file) / discard / keep /
// view / force.
// ResolveOption tunes Resolve. Used to inject a diff colorizer from the
// UI layer without workspace importing it (that would be an import
// cycle and would break the internal/ui insulation boundary).
type ResolveOption func(*resolveOpts)

type resolveOpts struct {
	styleDiff func(string) string
}

// WithDiffStyler supplies a function that colorizes a unified-diff blob
// (one styled string in, one out). Applied to both the clipped summary
// and the full [v]iew output. Nil/absent → plain text.
func WithDiffStyler(f func(string) string) ResolveOption {
	return func(o *resolveOpts) { o.styleDiff = f }
}

func Resolve(ctx context.Context, o *Overlay, interactive bool, in io.Reader, out io.Writer, opts ...ResolveOption) error {
	var ro resolveOpts
	for _, opt := range opts {
		opt(&ro)
	}
	styleDiff := ro.styleDiff
	if styleDiff == nil {
		styleDiff = func(s string) string { return s }
	}

	agent, _, conflicts, err := o.threeWay()
	if err != nil {
		fmt.Fprintf(out, "workspace: could not compare (%v); changes kept at %s\n", err, o.KeepPath())
		return nil
	}
	if len(agent) == 0 {
		// The agent changed nothing (any divergence is host-side).
		return o.Discard()
	}

	summary, derr := o.rawAgentDiff(ctx)
	if derr != nil {
		summary = fmt.Sprintf("(could not render diff: %v)", derr)
	}
	fmt.Fprintf(out, "\nAgent changes (%d file(s); %s vs the project at task start):\n%s\n",
		len(agent), o.Copy, styleDiff(clip(summary, maxSummaryLines)))

	if len(conflicts) > 0 {
		fmt.Fprintf(out, "\n⚠ %d of these were ALSO changed on the host since the session started "+
			"(true conflicts — committing the safe set leaves these for you to merge):\n", len(conflicts))
		for _, p := range conflicts {
			fmt.Fprintf(out, "    %s\n", p)
		}
	}

	safe := minus(agent, conflicts)

	if !interactive || in == nil {
		fmt.Fprintf(out, "\nworkspace: kept for review\n  agent copy : %s\n  baseline   : %s\n  project    : %s\n",
			o.Copy, o.base, o.Project)
		fmt.Fprintf(out, "  %d change(s) apply cleanly; %d conflict(s) need a manual three-way merge.\n",
			len(safe), len(conflicts))
		o.KeepPath()
		return nil
	}

	r := bufio.NewReader(in)
	for {
		prompt := "\nApply to the real project? [c]ommit non-conflicting / [d]iscard / [k]eep / [v]iew diff"
		if len(conflicts) > 0 {
			prompt += " / [f]orce-all"
		}
		fmt.Fprint(out, prompt+": ")
		line, _ := r.ReadString('\n')
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "v", "view":
			full, e := o.rawAgentDiff(ctx)
			if e != nil {
				fmt.Fprintf(out, "  could not produce diff: %v\n", e)
			} else {
				fmt.Fprintf(out, "\n%s\n", styleDiff(full))
			}
		case "c", "commit":
			if err := o.applyPaths(ctx, safe); err != nil {
				fmt.Fprintf(out, "workspace: commit failed: %v (copy kept at %s)\n", err, o.KeepPath())
				return err
			}
			if len(conflicts) > 0 {
				fmt.Fprintf(out, "workspace: applied %d non-conflicting change(s) to %s\n", len(safe), o.Project)
				fmt.Fprintf(out, "  %d conflict(s) NOT applied — copy kept at %s, baseline at %s for manual merge.\n",
					len(conflicts), o.Copy, o.base)
				o.KeepPath()
				return nil
			}
			fmt.Fprintf(out, "workspace: applied %d change(s) to %s\n", len(safe), o.Project)
			return o.Cleanup()
		case "f", "force":
			if len(conflicts) == 0 {
				fmt.Fprintln(out, "  no conflicts; use [c]ommit")
				continue
			}
			fmt.Fprintf(out, "  force OVERWRITES %d host-edited file(s) with the agent's version.\n", len(conflicts))
			fmt.Fprint(out, "  type 'overwrite' to proceed, anything else to go back: ")
			conf, _ := r.ReadString('\n')
			if strings.TrimSpace(conf) != "overwrite" {
				fmt.Fprintln(out, "  force aborted; nothing changed")
				continue
			}
			if err := o.applyPaths(ctx, agent); err != nil {
				fmt.Fprintf(out, "workspace: force failed: %v (copy kept at %s)\n", err, o.KeepPath())
				return err
			}
			fmt.Fprintf(out, "workspace: force-applied %d change(s) to %s\n", len(agent), o.Project)
			return o.Cleanup()
		case "d", "discard":
			fmt.Fprintln(out, "workspace: discarded (project unchanged)")
			return o.Discard()
		case "k", "keep", "":
			fmt.Fprintf(out, "workspace: kept at %s (baseline %s)\n", o.Copy, o.base)
			o.KeepPath()
			return nil
		default:
			fmt.Fprintln(out, "  please answer c, d, k, v, or f")
		}
	}
}

// minus returns a sorted in order, dropping anything in b.
func minus(a, b []string) []string {
	drop := make(map[string]struct{}, len(b))
	for _, x := range b {
		drop[x] = struct{}{}
	}
	out := make([]string, 0, len(a))
	for _, x := range a {
		if _, ok := drop[x]; !ok {
			out = append(out, x)
		}
	}
	return out
}
