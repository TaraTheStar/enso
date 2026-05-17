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
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
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

	if fi, statErr := os.Stat(merged); statErr == nil && fi.IsDir() {
		if entries, _ := os.ReadDir(merged); len(entries) > 0 {
			kept := filepath.Join(stageDir, fmt.Sprintf("merged.kept-%d", time.Now().UnixNano()))
			if rerr := os.Rename(merged, kept); rerr == nil && out != nil {
				fmt.Fprintf(out, "workspace: a prior kept-for-review copy was preserved at %s\n", kept)
			}
		} else {
			_ = os.RemoveAll(merged)
		}
	}
	pruneKept(stageDir, KeptCap, out)
	_ = os.RemoveAll(base) // stale baseline: not user data
	for _, d := range []string{base, merged} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	if err := clone(ctx, abs, base); err != nil {
		return nil, err
	}
	if err := clone(ctx, base, merged); err != nil {
		return nil, err
	}
	// root="" : Cleanup removes base+merged explicitly, never the
	// stable parent or a renamed-aside kept copy.
	return &Overlay{Project: abs, Copy: merged, base: base, root: ""}, nil
}

// KeptCap bounds how many superseded merged.kept-* review copies a
// stable stage dir retains. Newer ones are the ones a user is likely
// still reviewing; very old ones are almost certainly abandoned.
const KeptCap = 3

// PruneKept deletes all but the `keep` most recent merged.kept-* (by
// mtime) directly under stageDir, reporting removals on out (may be
// nil). Best-effort. Exported so `enso sandbox prune` can sweep stale
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
func (o *Overlay) applyPaths(ctx context.Context, rels []string) error {
	for _, rel := range rels {
		src := filepath.Join(o.Copy, rel)
		dst := filepath.Join(o.Project, rel)
		if _, err := os.Lstat(src); err == nil {
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return fmt.Errorf("workspace: apply %s: %w", rel, err)
			}
			// cp -a per file: preserves mode/symlink, reflinks on CoW.
			c := exec.CommandContext(ctx, "cp", "-a", "--reflink=auto", "-f", "--", src, dst)
			if out, err := c.CombinedOutput(); err != nil {
				return fmt.Errorf("workspace: apply %s: %v\n%s", rel, err, out)
			}
		} else {
			// Agent deleted it.
			if err := os.RemoveAll(dst); err != nil {
				return fmt.Errorf("workspace: delete %s: %w", rel, err)
			}
			pruneEmptyParents(o.Project, filepath.Dir(dst))
		}
	}
	return nil
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

// Cleanup removes the throwaway trees (root for New; base+merged for
// NewAt). Idempotent. After KeepPath it is a HARD no-op — the copy and
// baseline the user was told are safe stay on disk.
func (o *Overlay) Cleanup() error {
	if o.kept {
		return nil
	}
	var firstErr error
	for _, p := range []string{o.root, o.base, o.Copy} {
		if p == "" {
			continue
		}
		if err := os.RemoveAll(p); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	o.root, o.base = "", ""
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
func Resolve(ctx context.Context, o *Overlay, interactive bool, in io.Reader, out io.Writer) error {
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
		len(agent), o.Copy, clip(summary, maxSummaryLines))

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
				fmt.Fprintf(out, "\n%s\n", full)
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
