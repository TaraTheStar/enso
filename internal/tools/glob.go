// SPDX-License-Identifier: AGPL-3.0-or-later

package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// GlobTool finds files by glob pattern.
type GlobTool struct{}

func (t GlobTool) Name() string { return "glob" }
func (t GlobTool) Description() string {
	return "Find files by glob pattern. Args: pattern (string), path (string)"
}
func (t GlobTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"pattern": map[string]interface{}{"type": "string"},
			"path":    map[string]interface{}{"type": "string"},
		},
		"required": []string{"pattern"},
	}
}

func (t GlobTool) Run(ctx context.Context, args map[string]interface{}, ac *AgentContext) (Result, error) {
	pattern, _ := args["pattern"].(string)
	root, _ := args["path"].(string)
	if root == "" {
		root = ac.Cwd
	}
	if abs, err := resolveRestricted(root, ac); err != nil {
		return Result{}, fmt.Errorf("glob: %w", err)
	} else {
		root = abs
	}

	matches, err := doublestar.Glob(os.DirFS(root), pattern)
	if err != nil {
		return Result{}, fmt.Errorf("glob: %w", err)
	}

	if len(matches) == 0 {
		return Result{LLMOutput: "no matches found", FullOutput: "no matches found"}, nil
	}

	for i, m := range matches {
		matches[i] = filepath.Join(root, m)
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("found %d match(es):\n", len(matches)))
	for _, m := range matches {
		sb.WriteString(m)
		sb.WriteString("\n")
	}

	output := sb.String()
	truncated, full := HeadTail(output, 2000)

	return Result{LLMOutput: truncated, FullOutput: full}, nil
}
