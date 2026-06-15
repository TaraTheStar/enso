// SPDX-License-Identifier: AGPL-3.0-or-later

package slash

import (
	"context"
	"sort"
	"strings"
)

// Command is a slash-command handler. Run is called with the trimmed arg
// string (everything after `/<name>`).
type Command interface {
	Name() string
	Description() string
	Run(ctx context.Context, args string) error
}

// Registry maps names to commands. Built-ins are registered first via
// Register and always win: user/project skills are added with
// RegisterIfAbsent so a cloned repo's `./.enso/skills/quit.md` can't
// silently hijack the built-in `/quit` (a trust/safety concern).
type Registry struct {
	cmds map[string]Command
}

// NewRegistry constructs an empty registry.
func NewRegistry() *Registry { return &Registry{cmds: make(map[string]Command)} }

// Register adds (or replaces) a command by name. Use only for trusted,
// first-party commands (the built-ins); untrusted skills go through
// RegisterIfAbsent so they can't overwrite a built-in.
func (r *Registry) Register(c Command) { r.cmds[c.Name()] = c }

// RegisterIfAbsent adds c only if no command with its name already
// exists, returning false on collision (the existing command is kept).
// Used for user/project skills so they shadow nothing already
// registered — built-ins, registered first, take precedence.
func (r *Registry) RegisterIfAbsent(c Command) bool {
	if _, exists := r.cmds[c.Name()]; exists {
		return false
	}
	r.cmds[c.Name()] = c
	return true
}

// Get looks up a command by name (without leading slash).
func (r *Registry) Get(name string) Command { return r.cmds[name] }

// List returns all registered commands sorted by name.
func (r *Registry) List() []Command {
	out := make([]Command, 0, len(r.cmds))
	for _, c := range r.cmds {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

// Parse splits a `/foo bar baz` line into (name, rest). Returns ok=false if
// the line does not start with `/`.
func Parse(line string) (name, args string, ok bool) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "/") {
		return "", "", false
	}
	line = strings.TrimPrefix(line, "/")
	idx := strings.IndexByte(line, ' ')
	if idx < 0 {
		return line, "", true
	}
	return line[:idx], strings.TrimSpace(line[idx+1:]), true
}
