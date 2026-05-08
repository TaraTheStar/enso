// SPDX-License-Identifier: AGPL-3.0-or-later

package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/TaraTheStar/enso/internal/tools"
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
// When sb is non-nil, the bash tool routes through a container. The note
// reports the in-container working directory (the path the model will
// see when it runs `pwd` or references files in shell commands) and
// flags the sandbox so the model knows shell userland is constrained to
// the configured image — not the host.
//
// restrictedRoots is the file-tool confinement list (typically
// `[cwd, ...permissions.additional_directories]`). Empty means
// confinement is off (`permissions.disable_file_confinement = true`)
// and file tools can touch any host path the agent process can read.
// File tools always run on the host process — sandbox routing only
// applies to bash — so this is independent of sb.
func environmentNote(cwd string, now time.Time, sb tools.SandboxRunner, restrictedRoots []string) string {
	if cwd == "" {
		return ""
	}
	gitRepo := "no"
	if isGitRepo(cwd) {
		gitRepo = "yes"
	}
	var b strings.Builder
	b.WriteString("# Environment\n\n")

	if sb != nil {
		mount := sb.WorkdirMount()
		fmt.Fprintf(&b, "- Working directory: %s (sandboxed; host path %s is bind-mounted here)\n", mount, cwd)
		fmt.Fprintf(&b, "- Sandbox: enabled — runtime %s, image %s, container %s\n", sb.Runtime(), sb.Image(), sb.ContainerName())
		b.WriteString("- Bash tool runs inside the sandbox and sees container paths; file-touching tools (read/write/edit/grep/glob) run on the host process and take host paths — they do NOT go through the sandbox.\n")
	} else {
		fmt.Fprintf(&b, "- Working directory: %s\n", cwd)
	}
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
