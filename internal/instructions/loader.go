// SPDX-License-Identifier: AGPL-3.0-or-later

package instructions

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/adrg/frontmatter"

	"github.com/TaraTheStar/enso/internal/embed"
	"github.com/TaraTheStar/enso/internal/paths"
)

// Layer is one slice of the assembled system prompt. Surfaced (rather than
// hidden inside BuildSystemPrompt) so /prompt and tests can show what
// each file contributed — including layers that were considered but
// discarded by a later `replace: true`.
type Layer struct {
	// Name identifies the layer in /prompt output (e.g. "default",
	// "~/.config/enso/ENSO.md", "./ENSO.md", "memories").
	Name string
	// Body is the text contributed by this layer, already including any
	// section header.
	Body string
	// Replace is true when this layer carried `replace: true` frontmatter,
	// which discarded every earlier layer.
	Replace bool
	// Discarded is true when a later layer's `replace: true` zeroed the
	// stack and dropped this one. Discarded layers do not contribute to
	// the assembled prompt — BuildSystemPrompt skips them — but they're
	// kept in the slice so /prompt can show the user what got dropped
	// and by whom.
	Discarded bool
}

// frontmatterSchema captures the per-file knobs supported in user/project
// ENSO.md and AGENTS.md frontmatter. Add fields here to extend per-file
// behaviour.
type frontmatterSchema struct {
	// Replace, when true, discards every earlier layer in the assembled
	// system prompt. Without it, the file's body is appended after
	// previous layers (the common case).
	Replace bool `yaml:"replace"`
}

// BuildSystemPrompt returns the assembled system prompt. The composition
// rules:
//
//   - Every prompt-content file is APPEND by default.
//   - A file with frontmatter `replace: true` discards every earlier layer.
//
// Append-by-default (uniform at every level) because the old "user
// ENSO.md replaces, project ENSO.md appends" split was backwards from
// intuition: most people reaching for a user-wide ENSO.md want to *add*
// a sentence ("prefer terse output", routing rules), not fork and lose
// future updates to the embedded default — while a team committing a
// precise repo-wide prompt previously had no way to start from a clean
// canvas. `replace: true` serves both: a personal full-custom prompt
// (user ENSO.md) or a team-canonical one (repo ENSO.md, which discards
// user prefs too — usually what "team standard" means).
//
// Layers, in order:
//
//  1. Default prompt (embedded).
//  2. Auto-rendered "## Available models" section (only when pc is
//     non-nil and ≥2 providers are configured). Slotted here so
//     user/project layers can reference it and a later `replace: true`
//     discards it too.
//  3. $XDG_CONFIG_HOME/enso/ENSO.md (user-wide).
//  4. Closest ENSO.md walking up from cwd.
//  5. Closest AGENTS.md walking up from cwd.
//  6. Memory files (user from $XDG_DATA_HOME/enso/memory/, then project
//     from <cwd>/.enso/memory/; project shadows user on name collision).
func BuildSystemPrompt(cwd string, pc *ProviderContext) (string, error) {
	layers, err := BuildSystemPromptLayered(cwd, pc)
	if err != nil {
		return "", err
	}
	parts := make([]string, 0, len(layers))
	for _, l := range layers {
		if l.Discarded {
			continue
		}
		parts = append(parts, l.Body)
	}
	return strings.Join(parts, "\n"), nil
}

// BuildSystemPromptLayered returns the active layers that compose the system
// prompt — the same content as BuildSystemPrompt, but addressable so /prompt
// and tests can show what came from where.
func BuildSystemPromptLayered(cwd string, pc *ProviderContext) ([]Layer, error) {
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("get cwd: %w", err)
		}
	}

	stack := []Layer{{Name: "default", Body: embed.DefaultSystemPrompt}}

	if section := renderProviderSection(pc); section != "" {
		stack = append(stack, Layer{Name: "providers", Body: section})
	}

	if l, ok, err := loadLayerFile(userPromptPath(), userPromptDisplayName(), ""); err != nil {
		return nil, err
	} else if ok {
		stack = applyLayer(stack, l)
	}

	ensoPath, agentsPath := findClosestPaths(cwd)
	if ensoPath != "" {
		if l, ok, err := loadLayerFile(ensoPath, displayPath(ensoPath), "Project Instructions (ENSO.md)"); err != nil {
			return nil, err
		} else if ok {
			stack = applyLayer(stack, l)
		}
	}
	if agentsPath != "" {
		if l, ok, err := loadLayerFile(agentsPath, displayPath(agentsPath), "Project Instructions (AGENTS.md)"); err != nil {
			return nil, err
		} else if ok {
			stack = applyLayer(stack, l)
		}
	}

	if mem := loadMemories(cwd); mem != "" {
		stack = append(stack, Layer{
			Name: "memories",
			Body: "\n\n# Auto-memory\n\nFacts and preferences saved during prior sessions. Trust unless current evidence contradicts.\n\n" + mem,
		})
	}

	return stack, nil
}

// applyLayer composes l onto the stack. With replace=true, every previous
// layer is marked Discarded (kept in the slice so /prompt can show the
// user what was dropped) and l is appended. Otherwise l just appends.
func applyLayer(stack []Layer, l Layer) []Layer {
	if l.Replace {
		for i := range stack {
			stack[i].Discarded = true
		}
	}
	return append(stack, l)
}

// loadLayerFile reads path, parses optional frontmatter, and returns a
// Layer. Returns (zero, false, nil) when the file doesn't exist or is
// effectively empty. The header (e.g. "Project Instructions (ENSO.md)")
// is wrapped around the body when non-empty.
func loadLayerFile(path, name, header string) (Layer, bool, error) {
	if path == "" {
		return Layer{}, false, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Layer{}, false, nil
		}
		return Layer{}, false, fmt.Errorf("read %s: %w", path, err)
	}
	var meta frontmatterSchema
	body, err := frontmatter.Parse(bytes.NewReader(data), &meta)
	if err != nil {
		return Layer{}, false, fmt.Errorf("frontmatter %s: %w", path, err)
	}
	text := strings.TrimSpace(string(body))
	if text == "" {
		return Layer{}, false, nil
	}
	if header != "" {
		text = fmt.Sprintf("\n\n# %s\n\n%s", header, text)
	}
	return Layer{Name: name, Body: text, Replace: meta.Replace}, true, nil
}

// userPromptPath returns the user-wide ENSO.md path, or "" if HOME / the
// XDG config dir can't be resolved.
func userPromptPath() string {
	dir, err := paths.ConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "ENSO.md")
}

// userPromptDisplayName renders userPromptPath() with $HOME collapsed
// back to "~" for nicer display in /prompt output.
func userPromptDisplayName() string {
	return displayPath(userPromptPath())
}

// displayPath rewrites $HOME prefixes back to "~" for nicer display.
// Falls back to the input on any resolution failure.
func displayPath(p string) string {
	if p == "" {
		return p
	}
	if home, err := os.UserHomeDir(); err == nil {
		if rel, err := filepath.Rel(home, p); err == nil && !strings.HasPrefix(rel, "..") {
			return "~/" + rel
		}
	}
	return p
}

// loadMemories reads every `*.md` from $XDG_DATA_HOME/enso/memory and
// <cwd>/.enso/memory and concatenates them with a per-file header. Files
// with the same basename in both dirs collapse with the project version
// winning — so projects can override a user-global memory by name. Files
// are emitted in lex order for deterministic output across runs.
func loadMemories(cwd string) string {
	type entry struct {
		name string
		body string
	}
	byName := map[string]entry{}

	dirs := []string{}
	if dir, err := paths.DataDir(); err == nil {
		dirs = append(dirs, filepath.Join(dir, "memory"))
	}
	if cwd != "" {
		dirs = append(dirs, filepath.Join(cwd, ".enso", "memory"))
	}
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			data, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				continue
			}
			byName[e.Name()] = entry{name: strings.TrimSuffix(e.Name(), ".md"), body: strings.TrimSpace(string(data))}
		}
	}
	if len(byName) == 0 {
		return ""
	}

	names := make([]string, 0, len(byName))
	for k := range byName {
		names = append(names, k)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, k := range names {
		e := byName[k]
		fmt.Fprintf(&b, "## %s\n\n%s\n\n", e.name, e.body)
	}
	return strings.TrimRight(b.String(), "\n")
}

// findClosestPaths returns the absolute path to the closest ENSO.md and
// AGENTS.md walking up from dir. Either may be "" if not found.
func findClosestPaths(dir string) (string, string) {
	ensoPath := ""
	agentsPath := ""

	// Walk dir → its parent → … until we either find both files or
	// hit a fixpoint. filepath.Dir reaches a fixpoint at "/", "."
	// (relative-cwd), or a Windows drive root — in any of those, the
	// next call returns the same string, so the fixpoint check
	// terminates the loop cleanly without special-casing each form.
	for d := dir; d != ""; {
		if ensoPath == "" {
			p := filepath.Join(d, "ENSO.md")
			if _, err := os.Stat(p); err == nil {
				ensoPath = p
			}
		}
		if agentsPath == "" {
			p := filepath.Join(d, "AGENTS.md")
			if _, err := os.Stat(p); err == nil {
				agentsPath = p
			}
		}
		if ensoPath != "" && agentsPath != "" {
			break
		}
		parent := filepath.Dir(d)
		if parent == d {
			break
		}
		d = parent
	}

	return ensoPath, agentsPath
}
