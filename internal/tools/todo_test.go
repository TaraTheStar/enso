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

// TestTodoTool_PerAgentIsolation is the regression for cross-agent list
// bleed: two agents (two AgentContexts) sharing the one registered
// TodoTool must keep independent lists.
func TestTodoTool_PerAgentIsolation(t *testing.T) {
	tt := TodoTool{}
	a := newToolAC(t.TempDir())
	b := newToolAC(t.TempDir())

	_, _ = tt.Run(context.Background(), map[string]any{"action": "add", "title": "a-task"}, a)

	// b's list must NOT see a's task.
	res, _ := tt.Run(context.Background(), map[string]any{"action": "list"}, b)
	if !strings.Contains(res.LLMOutput, "no tasks") {
		t.Errorf("agent b saw agent a's todos: %q", res.LLMOutput)
	}
	// a still has its own.
	res, _ = tt.Run(context.Background(), map[string]any{"action": "list"}, a)
	if !strings.Contains(res.LLMOutput, "a-task") {
		t.Errorf("agent a lost its todo: %q", res.LLMOutput)
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
