// SPDX-License-Identifier: AGPL-3.0-or-later

package slash

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// setUserHome points HOME at dir and clears the XDG_* vars so user-scoped
// config resolves under dir (see internal/paths). CI runners set
// XDG_CONFIG_HOME, which otherwise takes precedence over HOME and makes
// these fixtures invisible to the loaders.
func setUserHome(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("XDG_RUNTIME_DIR", "")
}

func TestLoadSkills_BasicFrontmatter(t *testing.T) {
	tmp := t.TempDir()
	setUserHome(t, tmp) // ~/.config/enso/skills resolves under tmp

	mustWriteSkill(t, filepath.Join(tmp, ".config", "enso", "skills", "explain.md"), `---
name: explain
description: explain a file
allowed-tools: [read, grep]
model: qwen
---
Explain {{ .Args }} like I'm a junior dev.
`)

	skills, err := LoadSkills(tmp)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(skills) != 1 {
		t.Fatalf("got %d skills, want 1", len(skills))
	}
	s := skills[0]
	if s.Name() != "explain" {
		t.Errorf("name = %q", s.Name())
	}
	if s.Description() != "explain a file" {
		t.Errorf("description = %q", s.Description())
	}
	allowed := s.AllowedTools()
	sort.Strings(allowed)
	if len(allowed) != 2 || allowed[0] != "grep" || allowed[1] != "read" {
		t.Errorf("allowed-tools = %v", s.AllowedTools())
	}
}

func TestSkillRun_RendersTemplateAndCallsSubmit(t *testing.T) {
	tmp := t.TempDir()
	mustWriteSkill(t, filepath.Join(tmp, "explain.md"), `---
name: explain
allowed-tools: [read]
---
Explain {{ .Args }} like I'm a junior dev.
`)
	setUserHome(t, tmp+"/empty-home") // no user skills
	skills, err := loadDir(tmp)
	if err != nil {
		t.Fatalf("loadDir: %v", err)
	}
	if len(skills) != 1 {
		t.Fatalf("got %d skills, want 1", len(skills))
	}

	var (
		submittedText  string
		submittedTools []string
	)
	skills[0].SetSubmitter(func(text string, allowed []string) {
		submittedText = text
		submittedTools = allowed
	})
	if err := skills[0].Run(context.Background(), "main.go"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(submittedText, "Explain main.go like") {
		t.Errorf("rendered text = %q", submittedText)
	}
	if len(submittedTools) != 1 || submittedTools[0] != "read" {
		t.Errorf("submitted tools = %v", submittedTools)
	}
}

func TestSkillRun_NoSubmitterError(t *testing.T) {
	tmp := t.TempDir()
	mustWriteSkill(t, filepath.Join(tmp, "x.md"), `---
name: x
---
hi
`)
	skills, err := loadDir(tmp)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// No SetSubmitter called.
	err = skills[0].Run(context.Background(), "")
	if err == nil {
		t.Fatal("want error when submitter not bound")
	}
	if !strings.Contains(err.Error(), "submitter") {
		t.Errorf("err = %v", err)
	}
}

func TestLoadSkills_ProjectShadowsUser(t *testing.T) {
	tmp := t.TempDir()
	setUserHome(t, tmp)

	// User skill: ~/.config/enso/skills/explain.md → "user" body
	mustWriteSkill(t, filepath.Join(tmp, ".config", "enso", "skills", "explain.md"), `---
name: explain
description: USER VERSION
---
user body
`)

	// Project skill: <cwd>/.enso/skills/explain.md → "project" body
	cwd := filepath.Join(tmp, "proj")
	mustWriteSkill(t, filepath.Join(cwd, ".enso", "skills", "explain.md"), `---
name: explain
description: PROJECT VERSION
---
project body
`)

	skills, err := LoadSkills(cwd)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(skills) != 1 {
		t.Fatalf("got %d skills, want 1", len(skills))
	}
	if skills[0].Description() != "PROJECT VERSION" {
		t.Errorf("project did not shadow user: %q", skills[0].Description())
	}
}

func TestLoadSkills_NameDefaultsToFilename(t *testing.T) {
	tmp := t.TempDir()
	mustWriteSkill(t, filepath.Join(tmp, "fix-bug.md"), `---
description: no name field
---
hi
`)
	skills, err := loadDir(tmp)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(skills) != 1 {
		t.Fatalf("got %d, want 1", len(skills))
	}
	if skills[0].Name() != "fix-bug" {
		t.Errorf("name = %q, want fix-bug (from filename)", skills[0].Name())
	}
}

func TestLoadSkills_MissingDirIsNotAnError(t *testing.T) {
	tmp := t.TempDir()
	setUserHome(t, tmp+"/no-such-home")
	cwd := filepath.Join(tmp, "no-such-cwd")

	skills, err := LoadSkills(cwd)
	if err != nil {
		t.Fatalf("missing dirs should be silently skipped: %v", err)
	}
	if len(skills) != 0 {
		t.Errorf("got %d, want 0", len(skills))
	}
}

func TestLoadSkills_BadTemplateIsAnError(t *testing.T) {
	tmp := t.TempDir()
	mustWriteSkill(t, filepath.Join(tmp, "broken.md"), `---
name: broken
---
{{ this is not a closed action
`)
	_, err := loadDir(tmp)
	if err == nil {
		t.Fatal("expected template parse error")
	}
}

// helpers

func mustWriteSkill(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}
