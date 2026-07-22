// SPDX-License-Identifier: AGPL-3.0-or-later

package tools

import (
	aztools "github.com/TaraTheStar/azoth/tools"
)

// Registry is enso's tool registry: an azoth/tools.Registry bound to enso's
// AgentContext. The registry mechanics — goroutine-safe map, memoized
// name-sorted ToolDefs (prompt-prefix-cache stability), Filter/Without child
// registries — live in azoth/tools and are shared with the sibling apps. Enso
// keeps its own AgentContext, the built-in tool impls, and BuildDefault.
//
// Method set: Register / Unregister(...string) / Get(name) (Tool, bool) /
// List / Filter(...string) / Without(...string) / ToolDefs.
type Registry = aztools.Registry[AgentContext]

// NewRegistry creates an empty registry.
func NewRegistry() *Registry {
	return aztools.NewRegistry[AgentContext]()
}

// BuildDefault creates a registry with all built-in tools.
func BuildDefault() *Registry {
	r := NewRegistry()
	r.Register(ReadTool{})
	r.Register(WriteTool{})
	r.Register(EditTool{})
	r.Register(BashTool{})
	r.Register(BashOutputTool{})
	r.Register(BashKillTool{})
	r.Register(GrepTool{})
	r.Register(GlobTool{})
	r.Register(WebFetchTool{})
	r.Register(TodoTool{})
	r.Register(MemoryTool{})
	r.Register(CheckpointTool{})
	return r
}
