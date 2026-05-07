// SPDX-License-Identifier: AGPL-3.0-or-later

package tools

import "github.com/TaraTheStar/enso/internal/llm"

// Registry holds the available tools.
type Registry struct {
	tools map[string]Tool
}

// NewRegistry creates an empty registry.
func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]Tool)}
}

// Register adds a tool to the registry.
func (r *Registry) Register(t Tool) {
	r.tools[t.Name()] = t
}

// Get returns a tool by name, or nil if not found.
func (r *Registry) Get(name string) Tool {
	return r.tools[name]
}

// List returns all registered tools.
func (r *Registry) List() []Tool {
	result := make([]Tool, 0, len(r.tools))
	for _, t := range r.tools {
		result = append(result, t)
	}
	return result
}

// BuildDefault creates a registry with all built-in tools.
func BuildDefault() *Registry {
	r := NewRegistry()
	r.Register(ReadTool{})
	r.Register(WriteTool{})
	r.Register(EditTool{})
	r.Register(BashTool{})
	r.Register(GrepTool{})
	r.Register(GlobTool{})
	r.Register(WebFetchTool{})
	r.Register(&TodoTool{})
	r.Register(MemoryTool{})
	return r
}

// Filter returns a new Registry containing only the named tools from r.
// Names not present in r are silently skipped. Used by skills'
// allowed-tools restriction, by spawn_agent's per-child tool subset, and
// by workflow per-role tool restriction.
func (r *Registry) Filter(names []string) *Registry {
	child := NewRegistry()
	for _, n := range names {
		if t := r.Get(n); t != nil {
			child.Register(t)
		}
	}
	return child
}

// Without returns a new Registry with the named tools removed. Names not
// present in r are silently skipped. Used by declarative agents'
// `denied-tools` list.
func (r *Registry) Without(names []string) *Registry {
	excluded := make(map[string]struct{}, len(names))
	for _, n := range names {
		excluded[n] = struct{}{}
	}
	child := NewRegistry()
	for name, t := range r.tools {
		if _, drop := excluded[name]; drop {
			continue
		}
		child.Register(t)
	}
	return child
}

// ToolDefs returns the registry's tools as OpenAI-compatible llm.ToolDef entries.
func (r *Registry) ToolDefs() []llm.ToolDef {
	defs := make([]llm.ToolDef, 0, len(r.tools))
	for _, t := range r.tools {
		defs = append(defs, llm.ToolDef{
			Type: "function",
			Function: llm.ToolFunctionDef{
				Name:        t.Name(),
				Description: t.Description(),
				Parameters:  t.Parameters(),
			},
		})
	}
	return defs
}
