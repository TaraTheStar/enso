// SPDX-License-Identifier: AGPL-3.0-or-later

package tools

import (
	"fmt"
	"strings"
)

// HeadTail truncates a string to maxLines, keeping the head and tail.
func HeadTail(s string, maxLines int) (truncated string, full string) {
	full = s
	if maxLines <= 0 {
		maxLines = 2000
	}

	lines := strings.Split(s, "\n")
	if len(lines) <= maxLines {
		return s, s
	}

	half := maxLines / 2
	skipped := len(lines) - maxLines

	parts := make([]string, 0, maxLines+1)
	parts = append(parts, lines[:half]...)
	parts = append(parts, fmt.Sprintf("\n... %d lines truncated ...\n", skipped))
	parts = append(parts, lines[len(lines)-half:]...)

	truncated = strings.Join(parts, "\n")

	return truncated, s
}
