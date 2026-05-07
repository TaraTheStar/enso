// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"strings"
)

// RenderDiff renders a unified diff string with tview color tags.
func RenderDiff(diff string) string {
	var sb strings.Builder
	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "+"):
			sb.WriteString("[green::b]" + escapeTags(line) + "[reset]")
		case strings.HasPrefix(line, "-"):
			sb.WriteString("[red::b]" + escapeTags(line) + "[reset]")
		case strings.HasPrefix(line, "@@"):
			sb.WriteString("[lavender::b]" + escapeTags(line) + "[reset]")
		case strings.HasPrefix(line, "---"), strings.HasPrefix(line, "+++"):
			sb.WriteString("[::b]" + escapeTags(line) + "[reset]")
		default:
			sb.WriteString(escapeTags(line))
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

func escapeTags(s string) string {
	s = strings.ReplaceAll(s, "[", "[[")
	s = strings.ReplaceAll(s, "]", "]]")
	return s
}
