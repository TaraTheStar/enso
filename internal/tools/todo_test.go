// SPDX-License-Identifier: AGPL-3.0-or-later

package tools

import (
	"context"
	"strings"
	"testing"
)

func TestTodoTool_AddListUpdateDone(t *testing.T) {
	tt := &TodoTool{}
	ac := newToolAC(t.TempDir())

	// Empty list initially.
	res, _ := tt.Run(context.Background(), map[string]any{"action": "list"}, ac)
	if !strings.Contains(res.LLMOutput, "no tasks") {
		t.Errorf("initial list = %q", res.LLMOutput)
	}

	// Add two items.
	res, _ = tt.Run(context.Background(),
		map[string]any{"action": "add", "title": "first"}, ac)
	if !strings.Contains(res.LLMOutput, "1 task") || !strings.Contains(res.LLMOutput, "first") {
		t.Errorf("after-add = %q", res.LLMOutput)
	}
	res, _ = tt.Run(context.Background(),
		map[string]any{"action": "add", "title": "second"}, ac)
	if !strings.Contains(res.LLMOutput, "2 task") {
		t.Errorf("after-add-2 = %q", res.LLMOutput)
	}

	// Move first to in_progress.
	res, _ = tt.Run(context.Background(), map[string]any{
		"action": "update",
		"id":     float64(1),
		"status": "in_progress",
	}, ac)
	if !strings.Contains(res.LLMOutput, "[>]") {
		t.Errorf("missing `[>]` icon for in_progress: %q", res.LLMOutput)
	}

	// Mark second done.
	res, _ = tt.Run(context.Background(), map[string]any{
		"action": "done",
		"id":     float64(2),
	}, ac)
	if !strings.Contains(res.LLMOutput, "[x]") {
		t.Errorf("missing `[x]` icon for completed: %q", res.LLMOutput)
	}
}

func TestTodoTool_AddRequiresTitle(t *testing.T) {
	tt := &TodoTool{}
	ac := newToolAC(t.TempDir())
	res, _ := tt.Run(context.Background(),
		map[string]any{"action": "add"}, ac)
	if !strings.Contains(res.LLMOutput, "title required") {
		t.Errorf("output = %q", res.LLMOutput)
	}
}

func TestTodoTool_UnknownAction(t *testing.T) {
	tt := &TodoTool{}
	ac := newToolAC(t.TempDir())
	res, _ := tt.Run(context.Background(),
		map[string]any{"action": "explode"}, ac)
	if !strings.Contains(res.LLMOutput, "unknown action") {
		t.Errorf("output = %q", res.LLMOutput)
	}
}
