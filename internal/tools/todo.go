// SPDX-License-Identifier: AGPL-3.0-or-later

package tools

import (
	"context"
	"fmt"
	"strings"
)

// TodoTool manages an in-session task list.
type TodoTool struct {
	todos  []todoItem
	nextID int
}

type todoItem struct {
	ID     int    `json:"id"`
	Title  string `json:"title"`
	Status string `json:"status"`
}

func (t TodoTool) Name() string { return "todo" }
func (t TodoTool) Description() string {
	return "Manage in-session task list. Args: action (list|add|update|done), title, id, status"
}
func (t TodoTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"action": map[string]interface{}{"type": "string"},
			"title":  map[string]interface{}{"type": "string"},
			"id":     map[string]interface{}{"type": "integer"},
			"status": map[string]interface{}{"type": "string"},
		},
		"required": []string{"action"},
	}
}

func (t *TodoTool) Run(ctx context.Context, args map[string]interface{}, ac *AgentContext) (Result, error) {
	action, _ := args["action"].(string)

	switch action {
	case "list":
		return t.list()
	case "add":
		title, _ := args["title"].(string)
		if title == "" {
			return Result{LLMOutput: "todo: title required for add"}, nil
		}
		t.nextID++
		t.todos = append(t.todos, todoItem{ID: t.nextID, Title: title, Status: "pending"})
		return t.list()
	case "update":
		id, _ := args["id"].(float64)
		status, _ := args["status"].(string)
		title, _ := args["title"].(string)
		for i, item := range t.todos {
			if item.ID == int(id) {
				if status != "" {
					t.todos[i].Status = status
				}
				if title != "" {
					t.todos[i].Title = title
				}
				break
			}
		}
		return t.list()
	case "done":
		id, _ := args["id"].(float64)
		for i, item := range t.todos {
			if item.ID == int(id) {
				t.todos[i].Status = "completed"
				break
			}
		}
		return t.list()
	default:
		return Result{LLMOutput: fmt.Sprintf("todo: unknown action %q", action)}, nil
	}
}

func (t *TodoTool) list() (Result, error) {
	if len(t.todos) == 0 {
		return Result{LLMOutput: "no tasks", FullOutput: "no tasks"}, nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("%d task(s):\n", len(t.todos)))
	for _, item := range t.todos {
		icon := "[ ]"
		switch item.Status {
		case "in_progress":
			icon = "[>]"
		case "completed":
			icon = "[x]"
		}
		sb.WriteString(fmt.Sprintf("  %s #%d: %s (%s)\n", icon, item.ID, item.Title, item.Status))
	}

	output := sb.String()
	return Result{LLMOutput: output, FullOutput: output}, nil
}
