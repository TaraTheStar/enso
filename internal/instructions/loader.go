// SPDX-License-Identifier: AGPL-3.0-or-later

package instructions

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/TaraTheStar/enso/internal/embed"
)

// BuildSystemPrompt constructs the system prompt from three tiers, plus
// any auto-memory files. Order:
//
//  1. Default prompt (or ~/.enso/ENSO.md if present — replaces).
//  2. Closest ENSO.md walking up from cwd (appended).
//  3. Closest AGENTS.md walking up from cwd (appended).
//  4. User memory files (~/.enso/memory/*.md, appended in lex order).
//  5. Project memory files (<cwd>/.enso/memory/*.md, appended in lex
//     order). Same-named files in the project dir shadow user files —
//     project memories typically describe project-specific facts.
func BuildSystemPrompt(cwd string) (string, error) {
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return "", fmt.Errorf("get cwd: %w", err)
		}
	}

	var parts []string

	prompt := embed.DefaultSystemPrompt
	userPrompt, err := loadUserPrompt()
	if err == nil && userPrompt != "" {
		prompt = userPrompt
	}
	parts = append(parts, prompt)

	ensoContent, agentsContent := findClosest(cwd)

	if ensoContent != "" {
		parts = append(parts, fmt.Sprintf("\n\n# Project Instructions (ENSO.md)\n\n%s", ensoContent))
	}
	if agentsContent != "" {
		parts = append(parts, fmt.Sprintf("\n\n# Project Instructions (AGENTS.md)\n\n%s", agentsContent))
	}

	if mem := loadMemories(cwd); mem != "" {
		parts = append(parts, "\n\n# Auto-memory\n\nFacts and preferences saved during prior sessions. Trust unless current evidence contradicts.\n\n"+mem)
	}

	return strings.Join(parts, "\n"), nil
}

func loadUserPrompt() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}

	path := filepath.Join(home, ".enso", "ENSO.md")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("read user ENSO.md: %w", err)
	}

	return string(data), nil
}

// loadMemories reads every `*.md` from ~/.enso/memory and
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
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".enso", "memory"))
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

func findClosest(dir string) (string, string) {
	ensoContent := ""
	agentsContent := ""

	// Walk dir → its parent → … until we either find both files or
	// hit a fixpoint. filepath.Dir reaches a fixpoint at "/", "."
	// (relative-cwd), or a Windows drive root — in any of those, the
	// next call returns the same string, so the fixpoint check
	// terminates the loop cleanly without special-casing each form.
	for d := dir; d != ""; {
		if ensoContent == "" {
			if data, err := os.ReadFile(filepath.Join(d, "ENSO.md")); err == nil {
				ensoContent = string(data)
			}
		}
		if agentsContent == "" {
			if data, err := os.ReadFile(filepath.Join(d, "AGENTS.md")); err == nil {
				agentsContent = string(data)
			}
		}
		if ensoContent != "" && agentsContent != "" {
			break
		}
		parent := filepath.Dir(d)
		if parent == d {
			break
		}
		d = parent
	}

	return ensoContent, agentsContent
}
