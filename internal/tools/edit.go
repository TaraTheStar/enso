// SPDX-License-Identifier: AGPL-3.0-or-later

package tools

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/pmezard/go-difflib/difflib"
)

// EditTool performs exact string-replacement edit.
type EditTool struct{}

func (t EditTool) Name() string { return "edit" }
func (t EditTool) Description() string {
	return "Edit a file by exact find-and-replace. Args: path (string), old_string (string), new_string (string), replace_all (bool)"
}
func (t EditTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"path":        map[string]interface{}{"type": "string"},
			"old_string":  map[string]interface{}{"type": "string"},
			"new_string":  map[string]interface{}{"type": "string"},
			"replace_all": map[string]interface{}{"type": "boolean"},
		},
		"required": []string{"path", "old_string", "new_string"},
	}
}

func (t EditTool) Run(ctx context.Context, args map[string]interface{}, ac *AgentContext) (Result, error) {
	path, _ := args["path"].(string)
	oldStr, _ := args["old_string"].(string)
	newStr, _ := args["new_string"].(string)
	replaceAll, _ := args["replace_all"].(bool)

	abs, err := resolveRestricted(path, ac)
	if err != nil {
		return Result{}, fmt.Errorf("edit: %w", err)
	}

	data, err := os.ReadFile(abs)
	if err != nil {
		return Result{}, fmt.Errorf("edit %s: read: %w", abs, err)
	}

	content := string(data)
	ac.ReadSet[abs] = true

	count := strings.Count(content, oldStr)
	if count == 0 {
		return Result{LLMOutput: fmt.Sprintf("edit %s: old_string not found", abs)}, nil
	}
	if !replaceAll && count > 1 {
		return Result{LLMOutput: fmt.Sprintf("edit %s: old_string appears %d times (set replace_all=true)", abs, count)}, nil
	}

	var updated string
	if replaceAll {
		updated = strings.ReplaceAll(content, oldStr, newStr)
	} else {
		updated = strings.Replace(content, oldStr, newStr, 1)
	}

	diff := difflib.UnifiedDiff{
		A:        difflib.SplitLines(content),
		B:        difflib.SplitLines(updated),
		FromFile: abs,
		ToFile:   abs,
		Context:  3,
	}
	unified, _ := difflib.GetUnifiedDiffString(diff)

	if err := atomicWriteFile(abs, []byte(updated), 0o644); err != nil {
		return Result{}, fmt.Errorf("edit %s: write: %w", abs, err)
	}

	if ac.FileEditHook != nil {
		ac.FileEditHook.OnFileEdit(ac.Cwd, abs, "edit")
	}

	return Result{
		LLMOutput:  fmt.Sprintf("edited %s (%d replacement%s)\n---\n%s", abs, count, plural(count), unified),
		FullOutput: fmt.Sprintf("edited %s\n---\n%s", abs, updated),
		Display:    unified,
	}, nil
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
