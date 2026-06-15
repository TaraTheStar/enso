// SPDX-License-Identifier: AGPL-3.0-or-later

package tools

import (
	"context"
	"fmt"
	"io/fs"
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
func (t GlobTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"pattern": map[string]any{"type": "string"},
			"path":    map[string]any{"type": "string"},
		},
		"required": []string{"pattern"},
	}
}

func (t GlobTool) Run(ctx context.Context, args map[string]any, ac *AgentContext) (Result, error) {
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

	// Bound the walk (no user-set timeout, unlike bash) and make it
	// cancellable: GlobWalk lets the callback abort on ctx expiry, which
	// plain doublestar.Glob can't.
	ctx, cancel := context.WithTimeout(ctx, searchToolTimeout)
	defer cancel()

	var matches []string
	err := doublestar.GlobWalk(os.DirFS(root), pattern, func(p string, _ fs.DirEntry) error {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		matches = append(matches, p)
		return nil
	})
	if err != nil {
		if ctx.Err() != nil {
			return Result{}, fmt.Errorf("glob: %w", ctx.Err())
		}
		return Result{}, fmt.Errorf("glob: %w", err)
	}

	cacheKey := "glob:" + pattern + ":" + root
	if len(matches) == 0 {
		return Result{LLMOutput: "no matches found", FullOutput: "no matches found", Meta: ResultMeta{CacheKey: cacheKey}}, nil
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
	truncated, full := truncateWithRecovery(ac, "glob", output)

	display := fmt.Sprintf("%d match%s", len(matches), matchPlural(len(matches)))

	return Result{LLMOutput: truncated, FullOutput: full, DisplayOutput: display, Meta: ResultMeta{CacheKey: cacheKey}}, nil
}

// matchPlural returns "es" for n != 1 — "match" / "matches", not "matchs".
// Lives here for use by other tools (grep) that count matches too.
func matchPlural(n int) string {
	if n == 1 {
		return ""
	}
	return "es"
}
