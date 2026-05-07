// SPDX-License-Identifier: AGPL-3.0-or-later

package agent

import (
	"fmt"
	"strings"
)

// additionalDirsNote returns a system-prompt addendum listing extra
// workspace directories the agent has access to, or "" if the list is
// empty. Always plural; one-dir lists still read fine.
func additionalDirsNote(dirs []string) string {
	if len(dirs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("# Additional workspace directories\n\nIn addition to the project cwd, you may operate on files in:\n")
	for _, d := range dirs {
		fmt.Fprintf(&b, "- %s\n", d)
	}
	b.WriteString("\nThese paths are explicitly trusted by the user; reads/edits there are expected. Permission rules in `~/.enso/config.toml` still gate destructive operations.")
	return b.String()
}

// gitAttributionNote returns a snippet to append to the system prompt
// instructing the model how to attribute itself in git commit messages.
// Returns the empty string when the user hasn't opted in (style is empty,
// "none", or unrecognized).
func gitAttributionNote(style, name string) string {
	style = strings.ToLower(strings.TrimSpace(style))
	if style == "" || style == "none" {
		return ""
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = "enso"
	}

	switch style {
	case "co-authored-by":
		return fmt.Sprintf(
			"# Git commits\n\nWhen you write a git commit message on the user's behalf, append this trailer:\n\n    Co-Authored-By: %s <noreply@enso.local>",
			name,
		)
	case "assisted-by":
		return fmt.Sprintf(
			"# Git commits\n\nWhen you write a git commit message on the user's behalf, append this trailer:\n\n    Assisted-by: %s",
			name,
		)
	default:
		return ""
	}
}
