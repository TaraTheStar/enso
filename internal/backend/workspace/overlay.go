// SPDX-License-Identifier: AGPL-3.0-or-later

// Package workspace implements the throwaway workspace overlay.
//
// Rather than podman's `:O` (which hides and unconditionally discards
// its upper layer — destroying the agent's legitimate work), the
// overlay is a host-controlled copy: at task start the project is
// cloned with `cp --reflink=auto` (near-free on CoW filesystems, a
// faithful full copy elsewhere); the COPY is bind-mounted into the
// container at the project's REAL path, so the one-filesystem-namespace
// property holds and the real project is never touched while the agent
// runs. At task end the host diffs copy-vs-project and the caller
// resolves it: commit (mirror the copy back), discard (delete it), or
// keep (leave it for manual review). Sub-agents share the one worker,
// hence the one copy — a single overlay per task, as designed.
package workspace

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Overlay is one task's throwaway working copy of a project.
type Overlay struct {
	Project string // real project dir (never mutated until Commit)
	Copy    string // the bind-mount source the container sees as Project
	root    string // temp dir holding Copy (removed by Cleanup/Discard)
}

// New clones project into a fresh throwaway directory and returns the
// overlay. The clone uses `cp --reflink=auto -a`: a reflink (CoW) clone
// where the filesystem supports it, a full recursive copy otherwise —
// either way a complete, independent tree (including .git) so the agent
// behaves identically to working in-place.
func New(ctx context.Context, project string) (*Overlay, error) {
	abs, err := filepath.Abs(project)
	if err != nil {
		return nil, err
	}
	root, err := os.MkdirTemp("", "enso-workspace-")
	if err != nil {
		return nil, err
	}
	copyDir := filepath.Join(root, "merged")
	if err := os.Mkdir(copyDir, 0o755); err != nil {
		_ = os.RemoveAll(root)
		return nil, err
	}
	// `<src>/.` copies contents (incl. dotfiles) into copyDir.
	cp := exec.CommandContext(ctx, "cp", "-a", "--reflink=auto", abs+"/.", copyDir)
	if out, err := cp.CombinedOutput(); err != nil {
		_ = os.RemoveAll(root)
		return nil, fmt.Errorf("workspace: clone %s: %v\n%s", abs, err, out)
	}
	return &Overlay{Project: abs, Copy: copyDir, root: root}, nil
}

// Changed reports whether the working copy diverged from the project,
// with a human-readable summary (first lines of a recursive brief
// diff). changed=false means nothing to commit.
func (o *Overlay) Changed(ctx context.Context) (summary string, changed bool, err error) {
	// diff exits 0 = identical, 1 = differences, >1 = trouble.
	cmd := exec.CommandContext(ctx, "diff", "-rq", "--", o.Project, o.Copy)
	out, derr := cmd.CombinedOutput()
	if derr == nil {
		return "", false, nil
	}
	if ee, ok := derr.(*exec.ExitError); ok && ee.ExitCode() == 1 {
		return summarize(string(out), o.Project, o.Copy), true, nil
	}
	return "", false, fmt.Errorf("workspace: diff: %v\n%s", derr, out)
}

// Commit mirrors the working copy back onto the real project
// (additions, modifications AND deletions the agent made), then removes
// the throwaway. After this the project reflects the task's result.
func (o *Overlay) Commit(ctx context.Context) error {
	// Trailing slashes: copy CONTENTS onto the project; --delete so a
	// file the agent removed is removed in the project too.
	cmd := exec.CommandContext(ctx, "rsync", "-a", "--delete",
		o.Copy+"/", o.Project+"/")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("workspace: commit: %v\n%s", err, out)
	}
	return o.Cleanup()
}

// Discard deletes the working copy; the project is left untouched
// (one-command rollback — here it is automatic and total).
func (o *Overlay) Discard() error { return o.Cleanup() }

// Cleanup removes the throwaway directory. Idempotent.
func (o *Overlay) Cleanup() error {
	if o.root == "" {
		return nil
	}
	err := os.RemoveAll(o.root)
	o.root = ""
	return err
}

// KeepPath disowns the throwaway (Cleanup becomes a no-op) and returns
// where the diverged copy now lives, for the caller to tell the user.
func (o *Overlay) KeepPath() string {
	o.root = ""
	return o.Copy
}

func summarize(diffOut, project, copyDir string) string {
	lines := strings.Split(strings.TrimSpace(diffOut), "\n")
	const max = 40
	clip := false
	if len(lines) > max {
		lines = lines[:max]
		clip = true
	}
	// Make paths project-relative so the summary is readable.
	for i, ln := range lines {
		ln = strings.ReplaceAll(ln, copyDir+"/", "")
		ln = strings.ReplaceAll(ln, project+"/", "")
		lines[i] = ln
	}
	s := strings.Join(lines, "\n")
	if clip {
		s += "\n… (more)"
	}
	return s
}

// Resolve runs the end-of-task decision. When the copy is unchanged it
// is silently cleaned up. When it diverged: an interactive session
// prompts commit / discard / keep; a non-interactive one defaults to
// KEEP and prints the path — it must never silently commit or destroy
// the agent's work. out receives user-facing messages; in is the
// prompt source (nil/non-interactive skips the prompt).
func Resolve(ctx context.Context, o *Overlay, interactive bool, in io.Reader, out io.Writer) error {
	summary, changed, err := o.Changed(ctx)
	if err != nil {
		// Be conservative: keep the copy so nothing is lost on a diff
		// failure, and surface where it is.
		fmt.Fprintf(out, "workspace: could not diff (%v); changes kept at %s\n", err, o.KeepPath())
		return nil
	}
	if !changed {
		return o.Discard()
	}

	fmt.Fprintf(out, "\nWorkspace changes (sandboxed copy diverged from %s):\n%s\n", o.Project, summary)

	if !interactive || in == nil {
		fmt.Fprintf(out, "\nworkspace: kept at %s\n  review, then `rsync -a --delete %s/ %s/` to apply, or delete to discard.\n",
			o.KeepPath(), o.Copy, o.Project)
		return nil
	}

	r := bufio.NewReader(in)
	for {
		fmt.Fprintf(out, "\nApply these changes to the real project? [c]ommit / [d]iscard / [k]eep for review: ")
		line, _ := r.ReadString('\n')
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "c", "commit":
			if err := o.Commit(ctx); err != nil {
				fmt.Fprintf(out, "workspace: commit failed: %v (copy kept at %s)\n", err, o.KeepPath())
				return err
			}
			fmt.Fprintf(out, "workspace: committed to %s\n", o.Project)
			return nil
		case "d", "discard":
			fmt.Fprintln(out, "workspace: discarded (project unchanged)")
			return o.Discard()
		case "k", "keep", "":
			fmt.Fprintf(out, "workspace: kept at %s\n", o.KeepPath())
			return nil
		default:
			fmt.Fprintln(out, "  please answer c, d, or k")
		}
	}
}
