// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/TaraTheStar/enso/internal/config"
)

// errFirstRunWelcome is a sentinel returned by loadOrWelcome the first time
// ensō is invoked on a new machine. It signals that a welcome message has
// already been printed to stderr and the caller should bail out cleanly
// (exit 0) without attempting to talk to the LLM. main() suppresses it.
var errFirstRunWelcome = errors.New("first-run welcome printed; user needs to configure a provider")

// loadOrWelcome wraps config.LoadWithFirstRun for LLM-using subcommands.
// If the load succeeds and a fresh default config was just written, it
// prints the welcome and returns errFirstRunWelcome so the subcommand
// stops short of contacting any model.
func loadOrWelcome(cwd string) (*config.Config, error) {
	cfg, freshlyWritten, err := config.LoadWithFirstRun(cwd, flagConfig)
	if err != nil {
		return nil, err
	}
	if freshlyWritten {
		path, _ := config.UserConfigPath()
		printFirstRunWelcome(path)
		// Suppress cobra's own "Error: ..." trailer + auto usage dump
		// on top of the welcome we just printed. main() suppresses the
		// non-zero exit.
		rootCmd.SilenceErrors = true
		rootCmd.SilenceUsage = true
		return nil, errFirstRunWelcome
	}
	return cfg, nil
}

// printFirstRunWelcome writes a one-shot welcome to stderr explaining that
// a default config was just written, where it lives, what it points at,
// and how to reconfigure for the most common providers. Prefer stderr so
// `enso run` users who pipe stdout aren't surprised by setup chatter in
// their pipeline.
func printFirstRunWelcome(configPath string) {
	const msg = `
ensō — first run

A default config has been written to:

    %s

It points at a local llama.cpp-compatible server:

    base_url = "http://localhost:8080/v1"
    model    = "qwen3.6-35b-a3b"

Next step: pick one and edit the config.

  • Local llama.cpp (default)
      Start llama-server on :8080 with a Qwen3.6-35B-A3B GGUF, e.g.:
        llama-server -m Qwen3.6-35B-A3B.gguf --port 8080

  • OpenAI
      [providers.openai]
      base_url = "https://api.openai.com/v1"
      model    = "gpt-4o"
      api_key  = "$ENSO_OPENAI_KEY"   # then: export ENSO_OPENAI_KEY=sk-...

  • Anthropic-compatible (via an OpenAI-compat proxy / gateway)
      ensō speaks the OpenAI chat-completions wire format. Point base_url
      at any compatible endpoint (LiteLLM, OpenRouter, vLLM, etc.).

  • Groq, Together, Fireworks, OpenRouter, …
      Same pattern as OpenAI — set base_url, model, api_key.

Useful commands:

    enso config show               # see all config search paths
    enso config init --print       # dump the default template
    enso version                   # version + build info

Docs: https://tarathestar.github.io/enso/docs/
Secrets / env-var refs: https://tarathestar.github.io/enso/docs/secrets/

Re-run your command after editing the config.
`
	fmt.Fprintf(os.Stderr, msg, configPath)
}
