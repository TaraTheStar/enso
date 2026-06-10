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

	// Preserve an existing destination's permission bits (e.g. the exec bit
	// on a script); only a brand-new file gets the 0o644 default.
	mode := os.FileMode(0o644)
	if fi, statErr := os.Stat(abs); statErr == nil {
		if !ac.ReadSet[abs] {
			return Result{LLMOutput: fmt.Sprintf("write %s: file exists but was not read in this session", abs)}, nil
		}
		mode = fi.Mode().Perm()
	}

	ac.ReadSet[abs] = true

	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return Result{}, fmt.Errorf("write %s: mkdir: %w", abs, err)
	}

	if err := atomicWriteFile(abs, []byte(content), mode); err != nil {
		return Result{}, fmt.Errorf("write %s: %w", abs, err)
	}

	if ac.FileEditHook != nil {
		ac.FileEditHook.OnFileEdit(ac.Cwd, abs, "write")
	}

	llmOut := fmt.Sprintf("wrote %d bytes to %s", len(content), abs)
	if ac.LSPNotifier != nil {
		llmOut += ac.LSPNotifier.NotifyWrite(ctx, abs)
	}
	return Result{
		LLMOutput:  llmOut,
		FullOutput: fmt.Sprintf("wrote %d bytes to %s\n---\n%s", len(content), abs, content),
		Meta: ResultMeta{
			PathsWritten: []string{abs},
			CacheKey:     "write:" + abs,
		},
	}, nil
}
