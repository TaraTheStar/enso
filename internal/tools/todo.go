// SPDX-License-Identifier: AGPL-3.0-or-later

package tools

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// TodoStore holds one agent's in-session task list. It is per-AgentContext
// (constructed via NewTodoStore alongside the agent), so sibling and child
// agents — and unrelated daemon sessions that share the tool Registry —
// each keep their own list instead of bleeding into a single shared one.
// The mutex guards against concurrent tool calls within an agent.
type TodoStore struct {
	mu     sync.Mutex
	todos  []todoItem
	nextID int
}

// NewTodoStore returns an empty per-agent todo store.
func NewTodoStore() *TodoStore { return &TodoStore{} }

type todoItem struct {
	ID     int    `json:"id"`
	Title  string `json:"title"`
	Status string `json:"status"`
}

// TodoTool manages an in-session task list. It is stateless — the list
// lives on the per-agent TodoStore (AgentContext.Todos) — so the single
// registered instance is safe to share across agents.
type TodoTool struct{}

func (t TodoTool) Name() string { return "todo" }
func (t TodoTool) Description() string {
	return "Manage in-session task list. Args: action (list|add|update|done), title, id, status"
}
func (t TodoTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{"type": "string"},
			"title":  map[string]any{"type": "string"},
			"id":     map[string]any{"type": "integer"},
			"status": map[string]any{"type": "string"},
		},
		"required": []string{"action"},
	}
}

func (t TodoTool) Run(ctx context.Context, args map[string]any, ac *AgentContext) (Result, error) {
	// Fall back to a transient store if the context didn't supply one
	// (e.g. minimal test harnesses). Normal agents always have ac.Todos.
	var store *TodoStore
	if ac != nil && ac.Todos != nil {
		store = ac.Todos
	} else {
		store = NewTodoStore()
	}
	store.mu.Lock()
	defer store.mu.Unlock()

	action, _ := args["action"].(string)

	switch action {
	case "list":
		return store.list()
	case "add":
		title, _ := args["title"].(string)
		if title == "" {
			return Result{LLMOutput: "todo: title required for add"}, nil
		}
		store.nextID++
		store.todos = append(store.todos, todoItem{ID: store.nextID, Title: title, Status: "pending"})
		return store.list()
	case "update":
		id, _ := args["id"].(float64)
		status, _ := args["status"].(string)
		title, _ := args["title"].(string)
		for i, item := range store.todos {
			if item.ID == int(id) {
				if status != "" {
					store.todos[i].Status = status
				}
				if title != "" {
					store.todos[i].Title = title
				}
				break
			}
		}
		return store.list()
	case "done":
		id, _ := args["id"].(float64)
		for i, item := range store.todos {
			if item.ID == int(id) {
				store.todos[i].Status = "completed"
				break
			}
		}
		return store.list()
	default:
		return Result{LLMOutput: fmt.Sprintf("todo: unknown action %q", action)}, nil
	}
}

// list renders the current tasks. Callers must hold s.mu.
func (s *TodoStore) list() (Result, error) {
	if len(s.todos) == 0 {
		return Result{LLMOutput: "no tasks", FullOutput: "no tasks"}, nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("%d task(s):\n", len(s.todos)))
	for _, item := range s.todos {
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
