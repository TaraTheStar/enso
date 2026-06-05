// SPDX-License-Identifier: AGPL-3.0-or-later

package tools

import (
	"sort"
	"sync"

	"github.com/TaraTheStar/enso/internal/llm"
)

// Registry holds the available tools.
//
// A registry is goroutine-safe: sibling workflow roles and spawned child
// agents share one registry and call ToolDefs concurrently per turn, so all
// access to tools and the memoized cache is guarded by mu.
type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool

	// defsCache memoizes the sorted ToolDefs slice. Nil means
	// "stale, recompute on next call." Cleared by Register.
	// Filter/Without build fresh registries so their caches start
	// stale naturally.
	defsCache []llm.ToolDef
}

// NewRegistry creates an empty registry.
func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]Tool)}
}

// Register adds a tool to the registry.
func (r *Registry) Register(t Tool) {
	r.mu.Lock()
	r.tools[t.Name()] = t
	r.defsCache = nil
	r.mu.Unlock()
}

// Get returns a tool by name, or nil if not found.
func (r *Registry) Get(name string) Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.tools[name]
}

// List returns all registered tools.
func (r *Registry) List() []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
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
	r.Register(BashOutputTool{})
	r.Register(BashKillTool{})
	r.Register(GrepTool{})
	r.Register(GlobTool{})
	r.Register(WebFetchTool{})
	r.Register(&TodoTool{})
	r.Register(MemoryTool{})
	r.Register(CheckpointTool{})
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
	r.mu.RLock()
	for name, t := range r.tools {
		if _, drop := excluded[name]; drop {
			continue
		}
		child.Register(t)
	}
	r.mu.RUnlock()
	return child
}

// ToolDefs returns the registry's tools as OpenAI-compatible llm.ToolDef entries.
//
// Sorted by name so the serialized prompt prefix is byte-stable across turns —
// otherwise Go's randomized map iteration shuffles the tools array each call
// and busts the llama.cpp prompt-prefix cache, forcing a full re-prefill.
//
// Memoized: registries are effectively immutable after BuildDefault /
// Filter / Without finish wiring them up, so the sort + alloc only
// runs once per registry. Register invalidates the cache.
func (r *Registry) ToolDefs() []llm.ToolDef {
	// Fast path: warm cache under a read lock. The cached slice is replaced
	// wholesale (never mutated in place) by Register, so returning it without
	// holding the lock is safe.
	r.mu.RLock()
	cached := r.defsCache
	r.mu.RUnlock()
	if cached != nil {
		return cached
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	// Re-check: another goroutine may have populated the cache between the
	// read-unlock above and the write-lock here.
	if r.defsCache != nil {
		return r.defsCache
	}
	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	defs := make([]llm.ToolDef, 0, len(names))
	for _, name := range names {
		t := r.tools[name]
		defs = append(defs, llm.ToolDef{
			Type: "function",
			Function: llm.ToolFunctionDef{
				Name:        t.Name(),
				Description: t.Description(),
				Parameters:  t.Parameters(),
			},
		})
	}
	r.defsCache = defs
	return defs
}
