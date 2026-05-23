// SPDX-License-Identifier: AGPL-3.0-or-later

package lsp

import (
	"os/exec"

	"github.com/TaraTheStar/enso/internal/config"
)

// builtinLSPs is the table of "if the binary is on PATH, just use it"
// language servers. Entries are merged into the user's [lsp.*] configs
// at Manager construction time so first-launch experience works without
// any TOML.
//
// User config always wins: a user-supplied `[lsp.<name>]` block under
// the same name overrides the builtin entirely. Setting `command = ""`
// in user config disables a specific builtin. Globally disable all
// builtins with the top-level `lsp_builtins_disabled = true` setting.
var builtinLSPs = map[string]config.LSPConfig{
	"go": {
		Command:     "gopls",
		Extensions:  []string{".go"},
		RootMarkers: []string{"go.mod", ".git"},
	},
	"typescript": {
		Command:     "typescript-language-server",
		Args:        []string{"--stdio"},
		Extensions:  []string{".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs"},
		RootMarkers: []string{"package.json", "tsconfig.json", "jsconfig.json", ".git"},
	},
	"python": {
		Command:     "pyright-langserver",
		Args:        []string{"--stdio"},
		Extensions:  []string{".py", ".pyi"},
		RootMarkers: []string{"pyproject.toml", "setup.py", "setup.cfg", ".git"},
	},
	"rust": {
		Command:     "rust-analyzer",
		Extensions:  []string{".rs"},
		RootMarkers: []string{"Cargo.toml", ".git"},
	},
}

// lookPath is exec.LookPath via an indirection so tests can stub PATH
// resolution without touching the process environment.
var lookPath = exec.LookPath

// mergeBuiltinLSPs returns user with auto-detected builtins folded in.
// Skips any builtin whose binary isn't on PATH, and any whose name is
// already present in user (user always wins, including the explicit
// `command = ""` disable form). When disabled is true, returns user
// unchanged.
//
// Pure function — easy to test, no Manager state involved. Manager's
// constructor calls this to produce the effective configs map.
func mergeBuiltinLSPs(user map[string]config.LSPConfig, disabled bool) map[string]config.LSPConfig {
	out := make(map[string]config.LSPConfig, len(user)+len(builtinLSPs))
	// User entries first. An explicit empty Command is the disable
	// form — drop the slot entirely so we neither merge a builtin
	// nor later try to exec "".
	for name, cfg := range user {
		if cfg.Command == "" {
			continue
		}
		out[name] = cfg
	}
	if disabled {
		return out
	}
	for name, cfg := range builtinLSPs {
		// User wins for the same name — including the disable form,
		// which dropped the slot above and must therefore stay dropped.
		if _, taken := out[name]; taken {
			continue
		}
		if _, ok := user[name]; ok {
			// User explicitly disabled (Command == ""); don't re-add
			// the builtin under their nose.
			continue
		}
		if _, err := lookPath(cfg.Command); err != nil {
			// Binary not on PATH; quietly skip. No log here — first-
			// launch noise about a missing rust-analyzer in a Go-only
			// project would be unhelpful.
			continue
		}
		out[name] = cfg
	}
	return out
}
