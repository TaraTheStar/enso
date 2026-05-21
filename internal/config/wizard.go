// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// WizardPreset describes one of the curated provider templates the
// onboarding wizard offers. The mid-prompt defaults all come from the
// preset (endpoint + model + context window) so users hitting Enter
// land on a working starter config without having to know what a
// reasonable model name looks like for the chosen backend.
type WizardPreset struct {
	// Name is the section key in the resulting TOML (`[providers.<Name>]`)
	// — short, machine-friendly, lowercase.
	Name string

	// DisplayName is the line shown to the user in the choice prompt.
	// Includes a one-line context hint so users can pick without
	// having to read separate docs.
	DisplayName string

	// Type matches ProviderConfig.Type. Empty defaults to "openai"
	// for back-compat with the original wizard. Non-empty drives a
	// type-specific prompt flow in RunWizard (e.g. Bedrock skips the
	// endpoint + api_key prompts and asks for an AWS region instead).
	Type string

	Endpoint      string
	Model         string
	ContextWindow int

	// NeedsKey is true for hosted providers that won't talk without
	// auth. Drives whether the wizard prompts for an API key, and
	// what env-var name to suggest for the indirection pattern.
	NeedsKey  bool
	KeyEnvVar string

	// AWSRegion is the default region for Bedrock-flavored presets.
	// Only consulted when Type == "bedrock".
	AWSRegion string
}

// wizardPresets is the small curated list the onboarding wizard
// offers. Order matters — the first entry is the default selection
// (Enter past the choice prompt to pick it).
//
// Naming convention: presets are labelled by protocol/provider, not
// by location. "llama.cpp" covers both `localhost:8080` and a server
// across the LAN — the endpoint prompt is where the user steers that.
// Calling the first preset "local" was misleading: users running
// llama.cpp on a remote box reasonably picked "custom" thinking
// "local" implied localhost-only.
var wizardPresets = []WizardPreset{
	{
		Name:          "llamacpp",
		DisplayName:   "llama.cpp     (or any OpenAI-compat server)",
		Endpoint:      "http://localhost:8080/v1",
		Model:         "qwen3.6-35b-a3b",
		ContextWindow: 32768,
		NeedsKey:      false,
	},
	{
		Name:          "ollama",
		DisplayName:   "ollama        (local default :11434)",
		Endpoint:      "http://localhost:11434/v1",
		Model:         "qwen2.5-coder:14b",
		ContextWindow: 32768,
		NeedsKey:      false,
	},
	{
		Name:          "openai",
		DisplayName:   "openai        (hosted, needs API key)",
		Endpoint:      "https://api.openai.com/v1",
		Model:         "gpt-4o",
		ContextWindow: 128000,
		NeedsKey:      true,
		KeyEnvVar:     "ENSO_OPENAI_KEY",
	},
	{
		Name:          "bedrock",
		DisplayName:   "bedrock       (AWS — Claude / Nova / Llama / Mistral)",
		Type:          "bedrock",
		Model:         "anthropic.claude-3-5-sonnet-20241022-v2:0",
		ContextWindow: 200000,
		AWSRegion:     "us-east-1",
	},
}

// WizardResult is the structured output of RunWizard — what the user
// chose, before serialisation. Exposed so callers can log / test
// without re-parsing the generated TOML.
type WizardResult struct {
	Preset   string // "llamacpp" / "ollama" / "openai" / "bedrock" / "custom"
	Type     string // matches ProviderConfig.Type; "" for OpenAI-compat
	Endpoint string // empty for type="bedrock" (AWS SDK picks regional endpoint)
	Model    string
	APIKey   string // literal key OR "$ENV_VAR_NAME"; empty means no auth

	// AWSRegion is populated only when Type == "bedrock".
	AWSRegion string
}

// RunWizard reads choices from `in`, writes prompts to `out`, and
// returns a WizardResult plus the rendered TOML config. The TOML
// preserves the comment-rich tail of the default template (permissions,
// bash, ui, mcp, lsp, search) — only the providers section is
// substituted. So users who run the wizard get the same documentation
// inside their config that silent-default-write users get.
//
// Returns an error only on I/O failure on `in`. Empty / unrecognised
// input falls back to defaults at every prompt, so a user mashing
// Enter ends up with a working local-llama.cpp setup.
func RunWizard(in io.Reader, out io.Writer) (WizardResult, string, error) {
	p := &prompter{in: bufio.NewReader(in), out: out}

	fmt.Fprintln(out, "Welcome to enso. Let's set up your LLM provider.")
	fmt.Fprintln(out)

	options := make([]string, 0, len(wizardPresets)+1)
	for _, ps := range wizardPresets {
		options = append(options, ps.DisplayName)
	}
	options = append(options, "custom        (enter your own endpoint)")

	fmt.Fprintln(out, "Tip: 'llama.cpp' covers any OpenAI-compatible server — local or remote.")
	fmt.Fprintln(out, "     Pick it, then type your server's URL at the next prompt.")
	fmt.Fprintln(out)

	idx := p.askChoice("Which provider are you using?", options, 0)

	var preset WizardPreset
	if idx < len(wizardPresets) {
		preset = wizardPresets[idx]
	} else {
		// Custom: empty defaults so the user is forced to type real
		// values. We still ask in the standard order so the prompt
		// flow looks the same.
		preset = WizardPreset{Name: "custom"}
	}

	fmt.Fprintln(out)

	// Bedrock takes a completely different prompt flow: no endpoint
	// (the AWS SDK picks the regional URL), no API key (the AWS
	// credential chain), but it does need a region. Branch here
	// rather than inflating the shared flow with conditionals.
	if preset.Type == "bedrock" {
		return runBedrockBranch(p, out, preset)
	}

	endpoint := p.ask("Endpoint URL", preset.Endpoint)
	model := p.ask("Model name", preset.Model)

	// Custom presets without a known provider name need to choose a
	// section name. Default to "custom" — the user can rename later.
	name := preset.Name
	if name == "custom" {
		name = p.ask("Provider name (used as the [providers.<name>] section)", "custom")
	}

	contextWindow := preset.ContextWindow
	if contextWindow == 0 {
		raw := p.ask("Context window (tokens)", "32768")
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			contextWindow = n
		} else {
			contextWindow = 32768
		}
	}

	// API key prompt: only for known-paid presets, or when the user
	// hand-picked custom (since we can't tell). Recommend env-var
	// indirection by default — `$ENSO_*` references are expanded at
	// load time and keep secrets out of the on-disk config.
	apiKey := ""
	if preset.NeedsKey || preset.Name == "custom" {
		fmt.Fprintln(out)
		envVar := preset.KeyEnvVar
		if envVar == "" {
			envVar = "ENSO_" + strings.ToUpper(name) + "_KEY"
		}
		fmt.Fprintln(out, "This provider may need an API key.")
		fmt.Fprintln(out, "Recommended: store it in an environment variable and reference it from the config.")
		fmt.Fprintf(out, "  Add to your shell rc:  export %s=sk-...\n", envVar)
		fmt.Fprintln(out, "  Then leave the next prompt blank and we'll write the reference for you.")
		fmt.Fprintln(out, "  Or paste the key now to write it literally into the config (less secure).")
		fmt.Fprintln(out)
		raw := p.ask("API key (blank = use $"+envVar+", or 'none' to skip)", "")
		switch {
		case raw == "" || strings.EqualFold(raw, "$"+envVar):
			apiKey = "$" + envVar
		case strings.EqualFold(raw, "none"):
			apiKey = ""
		default:
			apiKey = raw
		}
	}

	result := WizardResult{
		Preset:   preset.Name,
		Endpoint: endpoint,
		Model:    model,
		APIKey:   apiKey,
	}
	tomlOut := buildWizardTOML(name, endpoint, model, contextWindow, apiKey)
	return result, tomlOut, nil
}

// runBedrockBranch handles the AWS Bedrock onboarding path. Distinct
// from the OpenAI-compat flow because Bedrock has no user-set
// endpoint (the AWS SDK resolves the regional URL) and no API key
// (auth comes from the AWS credential chain — env vars, shared
// config, instance role). The user picks a model + region.
func runBedrockBranch(p *prompter, out io.Writer, preset WizardPreset) (WizardResult, string, error) {
	fmt.Fprintln(out, "AWS Bedrock uses your AWS credential chain (environment, ")
	fmt.Fprintln(out, "~/.aws/credentials, EC2/ECS/EKS instance role). No API key")
	fmt.Fprintln(out, "is collected by this wizard.")
	fmt.Fprintln(out)

	model := p.ask("Bedrock model ID", preset.Model)
	region := p.ask("AWS region", preset.AWSRegion)

	result := WizardResult{
		Preset:    preset.Name,
		Type:      "bedrock",
		Model:     model,
		AWSRegion: region,
	}
	tomlOut := buildBedrockWizardTOML(preset.Name, model, region, preset.ContextWindow)
	return result, tomlOut, nil
}

// buildBedrockWizardTOML produces the final config text for the
// Bedrock onboarding path. Same tail-preservation rule as
// buildWizardTOML; just a different provider block on top — no
// endpoint, no api_key, and the optional extended-thinking field
// pre-baked as a commented hint so Claude users see it without
// hunting through docs.
func buildBedrockWizardTOML(name, model, region string, ctxWindow int) string {
	var b strings.Builder
	b.WriteString("# enso configuration\n# Written on first run; edit as needed.\n\n")
	fmt.Fprintf(&b, "[providers.%s]\n", name)
	b.WriteString("type = \"bedrock\"\n")
	fmt.Fprintf(&b, "model = %q\n", model)
	fmt.Fprintf(&b, "aws_region = %q\n", region)
	if ctxWindow == 0 {
		ctxWindow = 200000
	}
	fmt.Fprintf(&b, "context_window = %d\n", ctxWindow)
	b.WriteString("concurrency = 1\n")
	b.WriteString("# AWS auth follows the standard credential chain (env vars,\n")
	b.WriteString("# ~/.aws/credentials, instance role). Override with aws_profile.\n")
	b.WriteString("# aws_profile = \"default\"\n")
	b.WriteString("\n")
	b.WriteString("# Optional: route Claude's reasoning through the same channel\n")
	b.WriteString("# the TUI already renders for OpenAI reasoning models. Claude\n")
	b.WriteString("# models only — the API rejects this for Nova / Llama / etc.\n")
	b.WriteString("# extended_thinking        = true\n")
	b.WriteString("# extended_thinking_budget = 8000\n\n")

	if idx := strings.Index(defaultTOML, "[permissions]"); idx >= 0 {
		b.WriteString(defaultTOML[idx:])
	}
	return b.String()
}

// buildWizardTOML produces the final config text by substituting the
// wizard's provider section into the default template. Everything
// after `[permissions]` is preserved verbatim so users still get the
// commented documentation for permissions, bash sandbox, UI, MCP,
// LSP, search.
func buildWizardTOML(name, endpoint, model string, ctxWindow int, apiKey string) string {
	var b strings.Builder
	b.WriteString("# enso configuration\n# Written on first run; edit as needed.\n\n")
	fmt.Fprintf(&b, "[providers.%s]\n", name)
	fmt.Fprintf(&b, "endpoint = %q\n", endpoint)
	fmt.Fprintf(&b, "model = %q\n", model)
	fmt.Fprintf(&b, "context_window = %d\n", ctxWindow)
	b.WriteString("concurrency = 1\n")
	fmt.Fprintf(&b, "api_key = %q\n", apiKey)
	b.WriteString("\n")
	fmt.Fprintf(&b, "[providers.%s.sampler]\n", name)
	b.WriteString("temperature = 0.6\n")
	b.WriteString("top_k = 20\n")
	b.WriteString("top_p = 0.95\n")
	b.WriteString("min_p = 0.0\n")
	b.WriteString("presence_penalty = 1.5\n\n")

	// Tail of the default template — preserves all the documentation
	// blocks for permissions / bash / ui / git / mcp / lsp / search.
	if idx := strings.Index(defaultTOML, "[permissions]"); idx >= 0 {
		b.WriteString(defaultTOML[idx:])
	}
	return b.String()
}

// prompter wraps a buffered Reader + Writer with single-line prompt
// helpers. Lifted out of RunWizard so test-side scripted-input
// scenarios stay readable.
type prompter struct {
	in  *bufio.Reader
	out io.Writer
}

// ask emits "<question> [<default>]: " (or "<question>: " when there
// is no default), reads one line, and returns trimmed input. Empty
// input or a read error returns the default — that's why a user
// mashing Enter never hits an error path.
func (p *prompter) ask(question, defaultVal string) string {
	if defaultVal != "" {
		fmt.Fprintf(p.out, "%s [%s]: ", question, defaultVal)
	} else {
		fmt.Fprintf(p.out, "%s: ", question)
	}
	line, err := p.in.ReadString('\n')
	line = strings.TrimSpace(line)
	if err != nil && line == "" {
		return defaultVal
	}
	if line == "" {
		return defaultVal
	}
	return line
}

// askChoice prints a numbered list and reads a 1-based index. Out-of-
// range or unparseable input returns defaultIdx — same forgiving-of-
// Enter philosophy as ask().
func (p *prompter) askChoice(question string, options []string, defaultIdx int) int {
	fmt.Fprintln(p.out, question)
	for i, opt := range options {
		fmt.Fprintf(p.out, "  %d) %s\n", i+1, opt)
	}
	raw := p.ask("Choice", strconv.Itoa(defaultIdx+1))
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > len(options) {
		return defaultIdx
	}
	return n - 1
}
