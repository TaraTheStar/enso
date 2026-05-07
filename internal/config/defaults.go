// SPDX-License-Identifier: AGPL-3.0-or-later

package config

const defaultTOML = `# enso configuration
# Written on first run; edit as needed.

[providers.local]
endpoint = "http://localhost:8080/v1"
model = "qwen3.6-35b-a3b"
context_window = 32768
concurrency = 1
api_key = ""

[providers.local.sampler]
temperature = 0.6
top_k = 20
top_p = 0.95
min_p = 0.0
presence_penalty = 1.5

[permissions]
# Default permission mode for un-matched calls: "prompt" | "allow" | "deny".
mode = "prompt"
# Patterns to auto-allow without prompting (e.g., "bash(git *)").
allow = []
# Patterns to ALWAYS prompt for, even when the call would otherwise be
# allowed — useful for blast-radius commands (e.g., "bash(git push *)").
ask = []
# Patterns to auto-deny (e.g., "bash(rm -rf *)", "edit(./.env)").
# Deny rules win over both allow and ask.
deny = []
# Extra directories the agent should treat as part of its workspace,
# alongside the cwd. Surfaces in the system prompt and the @-file
# picker. Permission patterns still gate writes — this is informational.
additional_directories = []

[bash]
# How to run the bash tool. "off" = direct host exec (default).
# "auto" = run inside a per-project container (prefers podman, falls
# back to docker). Use "podman" or "docker" to pin a runtime.
sandbox = "off"

[bash.sandbox_options]
# Image to run when sandbox is enabled. Pick whatever toolchain you
# need; alpine is a small starting point.
image = "alpine:latest"
# Commands run once after container creation. Re-runs only when this
# list (or image / mounts / env) changes — tracked via a label.
init = []
# Container --network flag. Empty = runtime default.
# "none" = fully offline; "host" = share the host network.
network = ""
# Extra "-v src:dst[:opts]" mounts beyond the project cwd.
extra_mounts = []
# KEY=value entries injected into the container env.
env = []

[ui]
# Theme name (default: "dark")
theme = "dark"
# Editor mode for the input field: "default" or "vim".
editor_mode = "default"
# Custom right-side status bar — text/template syntax. Empty = built-in default.
# Variables: .Provider .Model .Session .Mode .Activity .Tokens .Window .TokensFmt
status_line = ""

[git]
# How the agent should attribute itself in git commit messages it writes:
#   "co-authored-by"   — adds a Co-Authored-By trailer
#   "assisted-by"      — adds an Assisted-by trailer
#   "none" / ""        — don't add any trailer (default)
attribution = "none"
attribution_name = "enso"

# MCP servers. One block per server. Tools are exposed to the agent as
# mcp__<server>__<tool>. Allow/deny patterns work against those names.
# $VAR / ${VAR} in args and headers values resolve against env vars
# whose name starts with ENSO_ — e.g. set ENSO_GITEA_TOKEN in your shell
# config and reference $ENSO_GITEA_TOKEN here. References to non-ENSO
# names collapse to empty (and log a warn) so a hostile config can't
# exfiltrate AWS_* / GITHUB_TOKEN / etc.
#
# Stdio transport (subprocess):
# [mcp.gitea]
# command = "gitea-mcp-server"
# args = ["--token", "$ENSO_GITEA_TOKEN"]
#
# HTTP transport (Streamable-HTTP, falls back to SSE):
# [mcp.notion]
# url = "https://mcp.notion.com/v1"
# headers = { Authorization = "Bearer $ENSO_NOTION_TOKEN" }

# LSP servers. One block per language server. When at least one is
# configured, the lsp_hover / lsp_definition / lsp_references /
# lsp_diagnostics tools become available. Servers are spawned lazily on
# the first request that touches a matching extension.
#
# [lsp.go]
# command = "gopls"
# extensions = [".go"]
# root_markers = ["go.mod", ".git"]
#
# [lsp.typescript]
# command = "typescript-language-server"
# args = ["--stdio"]
# extensions = [".ts", ".tsx", ".js", ".jsx"]
# root_markers = ["package.json", "tsconfig.json", ".git"]
#
# [lsp.python]
# command = "pyright-langserver"
# args = ["--stdio"]
# extensions = [".py"]
# root_markers = ["pyproject.toml", "setup.py", ".git"]
`
