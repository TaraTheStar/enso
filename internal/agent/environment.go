// SPDX-License-Identifier: AGPL-3.0-or-later

package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// environmentNote returns a system-prompt addendum describing the agent's
// runtime environment: working directory, platform, date, and whether the
// cwd is a git repo. Computed once at agent creation; values are stable
// for the session.
//
// The git-repo flag is a yes/no, not a status dump — `git status` output
// becomes stale within seconds and can leak branch names or modified-file
// paths into the system prompt. The model can run `git status` itself if
// it needs the current state.
//
// isolation is a single honest sentence describing the box the agent
// runs in (supplied by the Backend seam). There is exactly one
// filesystem namespace now — the entire agent core, bash AND the file
// tools, runs in the same place — so there is deliberately NO
// host↔container path-translation caveat anymore: that whole class of
// "use /work not the host path" prompt instruction is gone because the
// situation it described cannot occur. Empty isolation falls back to
// the conservative truth (direct host execution, no rollback).
//
// restrictedRoots is the file-tool confinement list (typically
// `[cwd, ...permissions.additional_directories]`). Empty means
// confinement is off (`permissions.disable_file_confinement = true`)
// and file tools can touch any path the agent can read.
func environmentNote(cwd string, now time.Time, isolation string, restrictedRoots []string) string {
	if cwd == "" {
		return ""
	}
	gitRepo := "no"
	if isGitRepo(cwd) {
		gitRepo = "yes"
	}
	if strings.TrimSpace(isolation) == "" {
		isolation = "none — this agent runs directly on the host; changes apply in place with no sandbox and no automatic rollback."
	}
	var b strings.Builder
	b.WriteString("# Environment\n\n")
	fmt.Fprintf(&b, "- Working directory: %s\n", cwd)
	fmt.Fprintf(&b, "- Isolation: %s\n", isolation)
	if len(restrictedRoots) > 0 {
		fmt.Fprintf(&b, "- File-tool access: confined to %s. Paths outside these roots will be rejected.\n", strings.Join(restrictedRoots, ", "))
	} else {
		b.WriteString("- File-tool access: unrestricted (permissions.disable_file_confinement = true) — file tools can read/write anywhere the host process can.\n")
	}
	fmt.Fprintf(&b, "- Platform: %s/%s\n", runtime.GOOS, runtime.GOARCH)
	fmt.Fprintf(&b, "- Today's date: %s\n", now.Format("2006-01-02"))
	fmt.Fprintf(&b, "- Git repo: %s\n", gitRepo)
	return b.String()
}

// isGitRepo walks up from start looking for a `.git` entry (file or
// directory — submodules use a file). Returns false on any I/O error;
// the env note is best-effort and a wrong "no" is harmless.
func isGitRepo(start string) bool {
	dir := start
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false
		}
		dir = parent
	}
}
