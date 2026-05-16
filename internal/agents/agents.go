// SPDX-License-Identifier: AGPL-3.0-or-later

// Package agents implements declarative agent profiles. An agent is a
// reusable bundle of (system-prompt addition, tool restrictions, sampler
// overrides, max-turns) that can replace the default top-level configuration
// for a session.
//
// Specs come from two places:
//
//  1. Built-ins compiled into the binary (currently `default` and `plan`).
//  2. Markdown files at `$XDG_CONFIG_HOME/enso/agents/<name>.md` (user) and
//     `<cwd>/.enso/agents/<name>.md` (project). Project shadows user; user
//     shadows built-in on name collision.
//
// Switching at runtime is currently startup-only: pass `--agent <name>` on
// the CLI. The TUI's `/agents` slash lists what's available.
package agents

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/adrg/frontmatter"

	"github.com/TaraTheStar/enso/internal/paths"
)

// Spec is a declarative agent profile. Zero values mean "leave the host's
// default in place" — `nil` for sampler-override pointers, empty slices for
// tool lists.
type Spec struct {
	Name        string
	Description string
	// PromptAppend is appended to the base system prompt with a separating
	// blank line. It does not replace the base prompt — the agent still
	// inherits the binary's default operating instructions.
	PromptAppend string
	// AllowedTools, if non-empty, restricts the registry to this set
	// (intersected with the registry's contents).
	AllowedTools []string
	// DeniedTools, if non-empty, removes these tools from the registry.
	// Applied after AllowedTools.
	DeniedTools []string
	// Sampler overrides. nil = unchanged.
	Temperature *float64
	TopP        *float64
	TopK        *int
	// MaxTurns overrides the default per-user-message turn cap. 0 = unchanged.
	MaxTurns int
}

// frontmatterSchema is the on-disk YAML schema. We use pointers + custom
// fields here so the loader can distinguish "unset" from "zero".
type frontmatterSchema struct {
	Name         string   `yaml:"name"`
	Description  string   `yaml:"description"`
	AllowedTools []string `yaml:"allowed-tools"`
	DeniedTools  []string `yaml:"denied-tools"`
	Temperature  *float64 `yaml:"temperature"`
	TopP         *float64 `yaml:"top_p"`
	TopK         *int     `yaml:"top_k"`
	MaxTurns     int      `yaml:"max_turns"`
}

// Find returns the Spec named `name`. Lookup order: project agents → user
// agents → built-ins. Returns nil, nil when the name is empty or "default"
// (the absence of an agent is itself the default — no overrides).
func Find(projectCwd, name string) (*Spec, error) {
	name = strings.TrimSpace(name)
	if name == "" || name == "default" {
		return nil, nil
	}
	all, err := LoadAll(projectCwd)
	if err != nil {
		return nil, err
	}
	for _, s := range all {
		if s.Name == name {
			return s, nil
		}
	}
	return nil, fmt.Errorf("agent %q not found (try /agents to list)", name)
}

// LoadAll returns every agent visible from `projectCwd`: built-ins, plus any
// `*.md` files in the user / project agents directories. Project shadows
// user; user shadows built-in.
func LoadAll(projectCwd string) ([]*Spec, error) {
	byName := map[string]*Spec{}
	for _, b := range Builtins() {
		byName[b.Name] = b
	}

	dirs := []string{}
	if dir, err := paths.ConfigDir(); err == nil {
		dirs = append(dirs, filepath.Join(dir, "agents"))
	}
	if projectCwd != "" {
		dirs = append(dirs, filepath.Join(projectCwd, ".enso", "agents"))
	}
	for _, dir := range dirs {
		specs, err := loadDir(dir)
		if err != nil {
			return nil, err
		}
		for _, s := range specs {
			byName[s.Name] = s
		}
	}

	out := make([]*Spec, 0, len(byName))
	for _, s := range byName {
		out = append(out, s)
	}
	return out, nil
}

func loadDir(dir string) ([]*Spec, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	var specs []*Spec
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		s, err := loadFile(path)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		specs = append(specs, s)
	}
	return specs, nil
}

func loadFile(path string) (*Spec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var meta frontmatterSchema
	body, err := frontmatter.Parse(bytes.NewReader(data), &meta)
	if err != nil {
		return nil, fmt.Errorf("frontmatter: %w", err)
	}
	if meta.Name == "" {
		base := filepath.Base(path)
		meta.Name = strings.TrimSuffix(base, filepath.Ext(base))
	}
	return &Spec{
		Name:         meta.Name,
		Description:  meta.Description,
		PromptAppend: strings.TrimSpace(string(body)),
		AllowedTools: meta.AllowedTools,
		DeniedTools:  meta.DeniedTools,
		Temperature:  meta.Temperature,
		TopP:         meta.TopP,
		TopK:         meta.TopK,
		MaxTurns:     meta.MaxTurns,
	}, nil
}
