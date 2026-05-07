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
func environmentNote(cwd string, now time.Time) string {
	if cwd == "" {
		return ""
	}
	gitRepo := "no"
	if isGitRepo(cwd) {
		gitRepo = "yes"
	}
	var b strings.Builder
	b.WriteString("# Environment\n\n")
	fmt.Fprintf(&b, "- Working directory: %s\n", cwd)
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
