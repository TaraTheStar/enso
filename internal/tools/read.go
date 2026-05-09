// SPDX-License-Identifier: AGPL-3.0-or-later

package tools

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// ReadTool reads a file or a line range.
type ReadTool struct{}

func (t ReadTool) Name() string { return "read" }
func (t ReadTool) Description() string {
	return "Read a file or a line range. Args: path (string), first_line (int), last_line (int)"
}
func (t ReadTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"path":       map[string]interface{}{"type": "string"},
			"first_line": map[string]interface{}{"type": "integer"},
			"last_line":  map[string]interface{}{"type": "integer"},
		},
		"required": []string{"path"},
	}
}

func (t ReadTool) Run(ctx context.Context, args map[string]interface{}, ac *AgentContext) (Result, error) {
	path, _ := args["path"].(string)
	abs, err := resolveRestricted(path, ac)
	if err != nil {
		return Result{}, fmt.Errorf("read: %w", err)
	}

	data, err := os.ReadFile(abs)
	if err != nil {
		return Result{}, fmt.Errorf("read %s: %w", abs, err)
	}

	ac.ReadSet[abs] = true

	lines := strings.Split(string(data), "\n")

	first := 1
	if fl, ok := args["first_line"].(float64); ok {
		first = int(fl)
	}
	last := len(lines)
	if ll, ok := args["last_line"].(float64); ok {
		last = int(ll)
	}

	if first > last {
		first, last = last, first
	}
	if first < 1 {
		first = 1
	}
	if last > len(lines) {
		last = len(lines)
	}

	var sb strings.Builder
	for i := first - 1; i < last; i++ {
		sb.WriteString(fmt.Sprintf("%6d  %s\n", i+1, lines[i]))
	}

	content := sb.String()
	truncated, full := HeadTail(content, 2000)

	// File contents are huge; the call signature already shows the path
	// (and any range args). Scrollback gets a count instead of pages of
	// numbered source. The model still receives `truncated` as before.
	totalLines := len(lines)
	returned := last - first + 1
	var display string
	if first == 1 && last == totalLines {
		display = fmt.Sprintf("%d line%s", returned, plural(returned))
	} else {
		display = fmt.Sprintf("lines %d-%d (of %d)", first, last, totalLines)
	}

	return Result{LLMOutput: truncated, FullOutput: full, DisplayOutput: display}, nil
}
