// SPDX-License-Identifier: AGPL-3.0-or-later

package slash

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/adrg/frontmatter"

	"github.com/TaraTheStar/enso/internal/paths"
)

// Skill is a user-defined slash command loaded from disk. Frontmatter fields
// are surfaced via Description(); the body is a `text/template` rendered
// against `{ Args }` and submitted as the next user message.
//
// The submitter receives both the rendered text AND the skill's
// `allowed-tools` list so the host can apply a per-turn registry
// restriction in addition to forwarding the message. Empty list = no
// restriction.
type Skill struct {
	name         string
	description  string
	allowedTools []string
	tmpl         *template.Template
	submit       func(text string, allowedTools []string)
}

// SkillFrontmatter is the YAML schema parsed from each skill file.
type SkillFrontmatter struct {
	Name         string   `yaml:"name"`
	Description  string   `yaml:"description"`
	AllowedTools []string `yaml:"allowed-tools"`
	Model        string   `yaml:"model"`
}

func (s *Skill) Name() string           { return s.name }
func (s *Skill) Description() string    { return s.description }
func (s *Skill) AllowedTools() []string { return s.allowedTools }

func (s *Skill) Run(ctx context.Context, args string) error {
	if s.submit == nil {
		return fmt.Errorf("skill %q: not bound to a submitter", s.name)
	}
	var buf bytes.Buffer
	data := map[string]any{"Args": args}
	if err := s.tmpl.Execute(&buf, data); err != nil {
		return fmt.Errorf("skill %q: render: %w", s.name, err)
	}
	s.submit(buf.String(), s.allowedTools)
	return nil
}

// SetSubmitter binds the skill to a callback that injects the rendered text
// as the next user message and, if non-empty, applies the skill's
// `allowed-tools` restriction for that turn. Returns the same skill for
// chaining.
func (s *Skill) SetSubmitter(submit func(text string, allowedTools []string)) *Skill {
	s.submit = submit
	return s
}

// LoadSkills scans the user dir (`$XDG_CONFIG_HOME/enso/skills`) and the
// project dir (`./.enso/skills`) for `*.md` files and returns parsed Skill
// values. Project skills shadow user skills on name collision.
func LoadSkills(projectCwd string) ([]*Skill, error) {
	dirs := []string{}
	if dir, err := paths.ConfigDir(); err == nil {
		dirs = append(dirs, filepath.Join(dir, "skills"))
	}
	if projectCwd != "" {
		dirs = append(dirs, filepath.Join(projectCwd, ".enso", "skills"))
	}

	byName := map[string]*Skill{}
	for _, dir := range dirs {
		skills, err := loadDir(dir)
		if err != nil {
			return nil, err
		}
		for _, s := range skills {
			byName[s.name] = s // later dirs (project) overwrite earlier (user)
		}
	}

	out := make([]*Skill, 0, len(byName))
	for _, s := range byName {
		out = append(out, s)
	}
	return out, nil
}

func loadDir(dir string) ([]*Skill, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	var skills []*Skill
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		s, err := loadFile(path)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		skills = append(skills, s)
	}
	return skills, nil
}

func loadFile(path string) (*Skill, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var meta SkillFrontmatter
	body, err := frontmatter.Parse(bytes.NewReader(data), &meta)
	if err != nil {
		return nil, fmt.Errorf("frontmatter: %w", err)
	}
	if meta.Name == "" {
		// Default to filename without extension.
		base := filepath.Base(path)
		meta.Name = strings.TrimSuffix(base, filepath.Ext(base))
	}
	tmpl, err := template.New(meta.Name).Parse(string(body))
	if err != nil {
		return nil, fmt.Errorf("template: %w", err)
	}
	return &Skill{
		name:         meta.Name,
		description:  meta.Description,
		allowedTools: meta.AllowedTools,
		tmpl:         tmpl,
	}, nil
}
