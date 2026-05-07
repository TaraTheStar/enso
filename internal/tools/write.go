// SPDX-License-Identifier: AGPL-3.0-or-later

package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// WriteTool overwrites a file.
type WriteTool struct{}

func (t WriteTool) Name() string { return "write" }
func (t WriteTool) Description() string {
	return "Overwrite a file with content. Args: path (string), content (string)"
}
func (t WriteTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"path":    map[string]interface{}{"type": "string"},
			"content": map[string]interface{}{"type": "string"},
		},
		"required": []string{"path", "content"},
	}
}

func (t WriteTool) Run(ctx context.Context, args map[string]interface{}, ac *AgentContext) (Result, error) {
	path, _ := args["path"].(string)
	content, _ := args["content"].(string)

	abs, err := resolveRestricted(path, ac)
	if err != nil {
		return Result{}, fmt.Errorf("write: %w", err)
	}

	if _, statErr := os.Stat(abs); statErr == nil {
		if !ac.ReadSet[abs] {
			return Result{LLMOutput: fmt.Sprintf("write %s: file exists but was not read in this session", abs)}, nil
		}
	}

	ac.ReadSet[abs] = true

	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return Result{}, fmt.Errorf("write %s: mkdir: %w", abs, err)
	}

	if err := atomicWriteFile(abs, []byte(content), 0o644); err != nil {
		return Result{}, fmt.Errorf("write %s: %w", abs, err)
	}

	if ac.FileEditHook != nil {
		ac.FileEditHook.OnFileEdit(ac.Cwd, abs, "write")
	}

	return Result{
		LLMOutput:  fmt.Sprintf("wrote %d bytes to %s", len(content), abs),
		FullOutput: fmt.Sprintf("wrote %d bytes to %s\n---\n%s", len(content), abs, content),
	}, nil
}
