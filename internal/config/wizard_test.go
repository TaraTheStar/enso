// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"bytes"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

// runWizardScripted feeds `input` (a sequence of lines, each ending
// in \n) through RunWizard and returns the result + rendered TOML.
// Centralises the test boilerplate.
func runWizardScripted(t *testing.T, input string) (WizardResult, string) {
	t.Helper()
	var out bytes.Buffer
	r, body, err := RunWizard(strings.NewReader(input), &out)
	if err != nil {
		t.Fatalf("RunWizard: %v", err)
	}
	return r, body
}

func TestRunWizard_DefaultsToLocalLlamaCpp(t *testing.T) {
	// User mashes Enter through every prompt — the no-input path
	// should produce a working local-llama.cpp config.
	r, body := runWizardScripted(t, "\n\n\n")
	if r.Preset != "llamacpp" {
		t.Errorf("preset=%q, want 'llamacpp'", r.Preset)
	}
	if r.Endpoint != "http://localhost:8080/v1" {
		t.Errorf("endpoint=%q, want llama.cpp default", r.Endpoint)
	}
	if r.APIKey != "" {
		t.Errorf("llamacpp preset should not collect an API key, got %q", r.APIKey)
	}
	// Result must parse back to a valid Config — that's the contract.
	mustParseValid(t, body, "llamacpp")
}

func TestRunWizard_PicksOllama(t *testing.T) {
	// Choice 2 = ollama; Enter through endpoint + model defaults.
	r, body := runWizardScripted(t, "2\n\n\n")
	if r.Preset != "ollama" {
		t.Errorf("preset=%q, want 'ollama'", r.Preset)
	}
	if !strings.Contains(r.Endpoint, "11434") {
		t.Errorf("endpoint=%q, want ollama default port", r.Endpoint)
	}
	mustParseValid(t, body, "ollama")
}

func TestRunWizard_OpenAIDefaultsToEnvVarRef(t *testing.T) {
	// Choice 3 = openai; Enter through endpoint + model + API-key
	// prompt → API key should default to "$ENSO_OPENAI_KEY".
	r, body := runWizardScripted(t, "3\n\n\n\n")
	if r.Preset != "openai" {
		t.Errorf("preset=%q, want 'openai'", r.Preset)
	}
	if r.APIKey != "$ENSO_OPENAI_KEY" {
		t.Errorf("APIKey=%q, want env-var ref default", r.APIKey)
	}
	if !strings.Contains(body, `api_key = "$ENSO_OPENAI_KEY"`) {
		t.Errorf("rendered TOML missing env-var ref: %s", body)
	}
	mustParseValid(t, body, "openai")
}

func TestRunWizard_OpenAILiteralKeyAcceptedButRecorded(t *testing.T) {
	// Choice 3 = openai; user pastes a literal key. We accept it but
	// the comment in wizard.go warns this is less secure.
	r, _ := runWizardScripted(t, "3\n\n\nsk-test-1234\n")
	if r.APIKey != "sk-test-1234" {
		t.Errorf("APIKey=%q, want literal key passthrough", r.APIKey)
	}
}

func TestRunWizard_OpenAINoneSkipsKey(t *testing.T) {
	// Some users run hosted models behind an open proxy with no auth.
	// "none" lets them skip the key entirely.
	r, body := runWizardScripted(t, "3\n\n\nnone\n")
	if r.APIKey != "" {
		t.Errorf("APIKey=%q, want empty after 'none'", r.APIKey)
	}
	if !strings.Contains(body, `api_key = ""`) {
		t.Errorf("rendered TOML should have empty api_key: %s", body)
	}
}

func TestRunWizard_CustomFullPath(t *testing.T) {
	// Choice 5 = custom (1 llamacpp / 2 ollama / 3 openai / 4 bedrock
	// / 5 custom). User types every value. Exercises the branch where
	// there are no preset defaults and the wizard asks for a section
	// name and context window.
	input := strings.Join([]string{
		"5",                       // preset choice = custom
		"https://example.test/v1", // endpoint
		"my-model",                // model
		"openrouter",              // section name
		"65536",                   // context window
		"$ENSO_OPENROUTER_KEY",    // API key (literal env-var ref)
	}, "\n") + "\n"
	r, body := runWizardScripted(t, input)
	if r.Preset != "custom" {
		t.Errorf("preset=%q, want 'custom'", r.Preset)
	}
	if r.Endpoint != "https://example.test/v1" {
		t.Errorf("endpoint=%q", r.Endpoint)
	}
	if !strings.Contains(body, "[providers.openrouter]") {
		t.Errorf("rendered TOML missing custom section name: %s", body)
	}
	if !strings.Contains(body, "context_window = 65536") {
		t.Errorf("rendered TOML missing custom context window: %s", body)
	}
	mustParseValid(t, body, "openrouter")
}

func TestRunWizard_PreservesDocumentationTail(t *testing.T) {
	// Wizard output must include the comment-rich documentation tail
	// from the default template (permissions, bash sandbox, ui, mcp,
	// lsp, search) — losing those would degrade the post-onboarding
	// edit experience.
	_, body := runWizardScripted(t, "\n\n\n")
	for _, want := range []string{
		"[permissions]",
		"[backend]",
		"type = \"local\"",
		"[ui]",
		"# MCP servers.",
		"# LSP servers.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered TOML missing %q from default template tail", want)
		}
	}
}

func TestRunWizard_OutOfRangeChoiceFallsBackToDefault(t *testing.T) {
	// "99\n" for the preset prompt → falls back to default (local).
	// Same forgiving-of-fat-fingers behaviour the prompter applies
	// throughout.
	r, _ := runWizardScripted(t, "99\n\n\n")
	if r.Preset != "llamacpp" {
		t.Errorf("preset=%q, want 'llamacpp' default after out-of-range choice", r.Preset)
	}
}

// TestRunWizard_BedrockDefaults exercises the Bedrock branch with all
// defaults: choice 4, Enter through model + region. Confirms the
// branch skips the api_key prompt entirely and writes a Bedrock-shaped
// provider section (no endpoint, type = "bedrock", aws_region set).
func TestRunWizard_BedrockDefaults(t *testing.T) {
	r, body := runWizardScripted(t, "4\n\n\n")
	if r.Preset != "bedrock" {
		t.Errorf("preset=%q, want 'bedrock'", r.Preset)
	}
	if r.Type != "bedrock" {
		t.Errorf("type=%q, want 'bedrock'", r.Type)
	}
	if r.Endpoint != "" {
		t.Errorf("endpoint=%q, want empty (Bedrock has no user endpoint)", r.Endpoint)
	}
	if r.APIKey != "" {
		t.Errorf("Bedrock branch must NOT collect an API key, got %q", r.APIKey)
	}
	if r.AWSRegion != "us-east-1" {
		t.Errorf("AWSRegion=%q, want us-east-1 default", r.AWSRegion)
	}
	if !strings.Contains(body, `type = "bedrock"`) {
		t.Errorf("rendered TOML missing type=bedrock: %s", body)
	}
	if !strings.Contains(body, `aws_region = "us-east-1"`) {
		t.Errorf("rendered TOML missing aws_region: %s", body)
	}
	// The provider block itself must not carry an api_key (the
	// preserved doc tail mentions api_key in other contexts, e.g.
	// search.searxng — so a flat `Contains` is too strict).
	if pblock := extractProviderBlock(body, "bedrock"); strings.Contains(pblock, "api_key") {
		t.Errorf("Bedrock provider block must not include api_key: %s", pblock)
	}
	mustParseValid(t, body, "bedrock")
}

// TestRunWizard_BedrockCustomRegion verifies the user can override
// region + model — covers the typical flow for a team in a non-default
// region (e.g. us-west-2).
func TestRunWizard_BedrockCustomRegion(t *testing.T) {
	input := strings.Join([]string{
		"4", // preset = bedrock
		"anthropic.claude-3-5-haiku-20241022-v1:0", // model
		"us-west-2", // region
	}, "\n") + "\n"
	r, body := runWizardScripted(t, input)
	if r.Model != "anthropic.claude-3-5-haiku-20241022-v1:0" {
		t.Errorf("Model=%q", r.Model)
	}
	if r.AWSRegion != "us-west-2" {
		t.Errorf("AWSRegion=%q", r.AWSRegion)
	}
	if !strings.Contains(body, `aws_region = "us-west-2"`) {
		t.Errorf("rendered TOML missing custom region: %s", body)
	}
}

// extractProviderBlock returns the substring of `body` covering the
// `[providers.<name>]` section up to the next top-level header (or
// end of input). Useful for asserting *within-block* properties
// without snagging unrelated commentary further down.
func extractProviderBlock(body, name string) string {
	header := "[providers." + name + "]"
	start := strings.Index(body, header)
	if start < 0 {
		return ""
	}
	rest := body[start:]
	// Find the next bracket header at column 0 — that's where the
	// next section begins. Skip the first header itself.
	lines := strings.Split(rest, "\n")
	var b strings.Builder
	for i, line := range lines {
		if i > 0 && strings.HasPrefix(line, "[") {
			break
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// mustParseValid asserts the rendered TOML decodes into a Config and
// has the expected provider section populated. Locks in that the
// wizard never produces a corrupt or unreadable config.
//
// Endpoint is required for OpenAI-shape providers but omitted for
// type = "bedrock" (the AWS SDK resolves the regional URL itself),
// so the check is gated on the type.
func mustParseValid(t *testing.T, body, providerName string) {
	t.Helper()
	var c Config
	if err := toml.Unmarshal([]byte(body), &c); err != nil {
		t.Fatalf("rendered TOML doesn't parse: %v\n%s", err, body)
	}
	p, ok := c.Providers[providerName]
	if !ok {
		t.Fatalf("provider %q missing from parsed config: %+v", providerName, c.Providers)
	}
	if p.Type != "bedrock" && p.Endpoint == "" {
		t.Errorf("provider %q has empty endpoint", providerName)
	}
	if p.Model == "" {
		t.Errorf("provider %q has empty model", providerName)
	}
}
