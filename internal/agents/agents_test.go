// SPDX-License-Identifier: AGPL-3.0-or-later

package agents

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/TaraTheStar/enso/internal/config"
	"github.com/TaraTheStar/enso/internal/llm"
	"github.com/TaraTheStar/enso/internal/tools"
)

func TestBuiltinPlanAgent(t *testing.T) {
	specs := Builtins()
	var plan *Spec
	for _, s := range specs {
		if s.Name == "plan" {
			plan = s
		}
	}
	if plan == nil {
		t.Fatalf("expected built-in 'plan' agent")
	}
	if plan.PromptAppend == "" {
		t.Errorf("plan agent should have a prompt body")
	}
	if len(plan.AllowedTools) == 0 {
		t.Errorf("plan agent should restrict tools")
	}
	for _, banned := range []string{"bash", "write", "edit"} {
		for _, allowed := range plan.AllowedTools {
			if banned == allowed {
				t.Errorf("plan agent should NOT allow %q", banned)
			}
		}
	}
}

func TestLoadAllProjectShadowsUser(t *testing.T) {
	// Two file dirs: a fake "user" dir under HOME, and a project dir.
	home := t.TempDir()
	t.Setenv("HOME", home)

	userDir := filepath.Join(home, ".enso", "agents")
	if err := os.MkdirAll(userDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeAgent(t, filepath.Join(userDir, "shared.md"), "shared", "user-version", []string{"read"})
	writeAgent(t, filepath.Join(userDir, "user-only.md"), "user-only", "from user", nil)

	project := t.TempDir()
	projectDir := filepath.Join(project, ".enso", "agents")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeAgent(t, filepath.Join(projectDir, "shared.md"), "shared", "project-version", []string{"glob"})

	specs, err := LoadAll(project)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*Spec{}
	for _, s := range specs {
		byName[s.Name] = s
	}

	if got := byName["shared"]; got == nil || got.Description != "project-version" {
		t.Errorf("project should shadow user for 'shared'; got %+v", got)
	}
	if byName["user-only"] == nil {
		t.Errorf("user-only agent should still load")
	}
	if byName["plan"] == nil {
		t.Errorf("built-in 'plan' should remain visible")
	}
}

func TestFind(t *testing.T) {
	if got, _ := Find("", ""); got != nil {
		t.Errorf("empty name → expect nil, got %+v", got)
	}
	if got, _ := Find("", "default"); got != nil {
		t.Errorf("'default' → expect nil (no override), got %+v", got)
	}
	if _, err := Find("", "no-such-agent-xxxx"); err == nil {
		t.Errorf("missing agent should return an error")
	}
	got, err := Find("", "plan")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Name != "plan" {
		t.Errorf("expected plan spec, got %+v", got)
	}
}

func TestApplyNilSpec(t *testing.T) {
	provider := &llm.Provider{Name: "p", Model: "m"}
	registry := tools.BuildDefault()
	out := Apply(nil, provider, registry)
	if out.Provider != provider || out.Registry != registry {
		t.Errorf("nil spec should pass through unchanged")
	}
	if out.PromptAppend != "" || out.MaxTurns != 0 {
		t.Errorf("nil spec should leave PromptAppend/MaxTurns zero")
	}
}

func TestApplyFiltersAndOverrides(t *testing.T) {
	provider := &llm.Provider{
		Name:    "p",
		Model:   "m",
		Sampler: config.SamplerConfig{Temperature: 0.6, TopP: 0.95, TopK: 20},
	}
	registry := tools.BuildDefault()

	temp := 0.1
	topP := 0.5
	spec := &Spec{
		Name:         "plan",
		AllowedTools: []string{"read", "grep"},
		Temperature:  &temp,
		TopP:         &topP,
		PromptAppend: "be careful",
		MaxTurns:     7,
	}
	out := Apply(spec, provider, registry)

	if out.Provider == provider {
		t.Errorf("sampler override should clone the provider")
	}
	if out.Provider.Sampler.Temperature != 0.1 || out.Provider.Sampler.TopP != 0.5 {
		t.Errorf("sampler not overridden: %+v", out.Provider.Sampler)
	}
	if out.Provider.Sampler.TopK != 20 {
		t.Errorf("TopK should be untouched (no override): %d", out.Provider.Sampler.TopK)
	}
	if provider.Sampler.Temperature != 0.6 {
		t.Errorf("original provider's sampler must not be mutated")
	}

	if out.Registry.Get("read") == nil || out.Registry.Get("grep") == nil {
		t.Errorf("allowed tools missing from filtered registry")
	}
	if out.Registry.Get("bash") != nil {
		t.Errorf("bash should not be in a plan-style restricted registry")
	}

	if out.PromptAppend != "be careful" {
		t.Errorf("PromptAppend not propagated")
	}
	if out.MaxTurns != 7 {
		t.Errorf("MaxTurns not propagated, got %d", out.MaxTurns)
	}
}

func TestApplyDeniedTools(t *testing.T) {
	registry := tools.BuildDefault()
	spec := &Spec{
		Name:        "no-bash",
		DeniedTools: []string{"bash", "edit"},
	}
	out := Apply(spec, &llm.Provider{}, registry)
	if out.Registry.Get("bash") != nil {
		t.Errorf("denied tool 'bash' should be excluded")
	}
	if out.Registry.Get("edit") != nil {
		t.Errorf("denied tool 'edit' should be excluded")
	}
	if out.Registry.Get("read") == nil {
		t.Errorf("undenied tool 'read' should remain")
	}
}

func writeAgent(t *testing.T, path, name, desc string, allowed []string) {
	t.Helper()
	body := "---\nname: " + name + "\ndescription: " + desc + "\n"
	if len(allowed) > 0 {
		body += "allowed-tools:\n"
		for _, a := range allowed {
			body += "  - " + a + "\n"
		}
	}
	body += "---\n\nbody.\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
