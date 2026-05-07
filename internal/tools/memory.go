// SPDX-License-Identifier: AGPL-3.0-or-later

package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

// MemoryTool writes a markdown memory file under <cwd>/.enso/memory/.
// On the next session start, instructions/loader.go reads every .md file
// under that directory (and ~/.enso/memory/) and appends them to the
// system prompt — so saved facts persist across sessions without the
// user having to repeat them.
//
// We deliberately keep the surface tiny: one tool, one scope (project).
// Listing or deleting memories is a `read` / `bash rm` away.
type MemoryTool struct{}

func (t MemoryTool) Name() string { return "memory_save" }
func (t MemoryTool) Description() string {
	return "Save a persistent memory that future sessions in this project will read. Use this when the user shares a stable preference, project fact, or correction that's worth remembering across runs (\"don't mock the database\", \"the staging endpoint is X\", \"this team prefers Y\"). Args: name (short slug — becomes the filename), content (markdown body — what to remember and *why*). Calling memory_save with the same name overwrites; pick descriptive names so updates stay coherent. Don't save ephemeral state, in-progress work, or anything already in code/git history."
}
func (t MemoryTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"name": map[string]interface{}{
				"type":        "string",
				"description": "Short, descriptive slug used as the filename (e.g. \"db-test-policy\", \"staging-endpoint\"). Slashes and unsafe characters are stripped.",
			},
			"content": map[string]interface{}{
				"type":        "string",
				"description": "Markdown body describing the fact or preference. Lead with the rule itself, then *why* (one line) and *when to apply* (one line) — the why lets future sessions judge edge cases.",
			},
		},
		"required": []string{"name", "content"},
	}
}

func (t MemoryTool) Run(ctx context.Context, args map[string]interface{}, ac *AgentContext) (Result, error) {
	name, _ := args["name"].(string)
	content, _ := args["content"].(string)

	slug := slugify(name)
	if slug == "" {
		return Result{}, fmt.Errorf("memory_save: name slugifies to empty (got %q)", name)
	}
	if strings.TrimSpace(content) == "" {
		return Result{}, fmt.Errorf("memory_save: content is empty")
	}

	dir := filepath.Join(ac.Cwd, ".enso", "memory")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Result{}, fmt.Errorf("memory_save: mkdir: %w", err)
	}
	path := filepath.Join(dir, slug+".md")

	body := strings.TrimRight(content, "\n") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return Result{}, fmt.Errorf("memory_save: write: %w", err)
	}

	return Result{
		LLMOutput:  fmt.Sprintf("memory saved to %s (will be loaded into the system prompt on next session)", path),
		FullOutput: fmt.Sprintf("memory saved to %s\n---\n%s", path, body),
	}, nil
}

// slugify lowercases the name and replaces every run of non-alphanumeric
// characters with a single dash, trimming dashes at both ends. Avoids
// path-traversal entirely — the result has no slashes by construction.
func slugify(s string) string {
	var b strings.Builder
	prevDash := true
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(unicode.ToLower(r))
			prevDash = false
			continue
		}
		if !prevDash {
			b.WriteByte('-')
			prevDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}
