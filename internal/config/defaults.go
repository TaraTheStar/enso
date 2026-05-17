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

[bash.sandbox_options]
# Per-project container settings, used WHEN [backend] type = "podman".
# Image to run. Pick whatever toolchain you
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

[backend]
# Where the agent core runs. This is the ONLY backend selector.
#   "local"  (default) = a host child process, no isolation.
#   "podman" = the core runs inside a rootless container (overlay
#              workspace, network-sealed, host-proxied inference).
#   "lima"   = the core runs inside a persistent per-project VM
#              (real-VM isolation); see [lima] below.
# Empty or unrecognized = "local" (fails safe).
type = "local"
# Container CLI for type = "podman": "auto" (default — prefer podman,
# fall back to docker), "podman", or "docker". Ignored otherwise.
# runtime = "auto"

# Tunes the persistent per-project VM when [backend] type = "lima".
# All optional; omitted fields use Lima defaults.
[lima]
# template     = "default"   # Lima template name, or a path/URL
# cpus         = 4
# memory       = "4GiB"
# disk         = "20GiB"
# extra_mounts = []           # extra host paths, mounted read-only

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

# Web search. The web_search tool is always available; by default it
# scrapes DuckDuckGo's html endpoint (no signup, no API key). For higher-
# quality multi-engine results, point [search.searxng] at a self-hosted
# SearXNG instance. Set [search] provider = "none" to suppress the tool
# entirely.
#
# [search]
# provider = "searxng"          # "" (auto) | "searxng" | "duckduckgo" | "none"
#
# [search.searxng]
# endpoint    = "http://localhost:8888"
# categories  = ["general"]
# engines     = []
# max_results = 10
# api_key     = "$ENSO_SEARXNG_KEY"   # optional
# timeout     = 15                     # seconds
# ca_cert     = "/etc/ssl/my-ca.pem"   # trust a self-hosted CA (appended to system roots)
# insecure_skip_verify = false         # last-resort: skip TLS verification for ad-hoc self-signed

# Context pruning. Stubs old tool-result payloads in conversation
# history once they're stale, dedupes repeated reads/commands, and
# invalidates pre-edit reads of files that were later written. The
# defaults are conservative; tighten via tool_retention if you run on
# a hybrid-attention model (Qwen3.6, etc.) that pays full prefix cost
# every turn.
#
# [context_prune]
# enabled = true                       # set false to revert to verbatim retention
# stale_after = 5                      # default user-turn threshold for stubbing
# pinned_paths = ["PLAN.md"]           # suffix-matched against absolute paths;
#                                      # reads of these survive stubbing + compaction
# smart_truncate = false               # B2: relevance-based truncation when output exceeds cap
#
# [context_prune.tool_retention]
# read = 8
# bash = 3
# grep = 2
# glob = 2
# edit = 1
# write = 1
#
# [context_prune.output_caps]
# default = 2000                       # global cap (lines) for HeadTail
# bash = 500
# read = 1000
`
