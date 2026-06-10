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
# Output-token cap for a single generation (max_tokens / n_predict). The
# hard backstop against runaway / repetition loops. 0 = derive a safe
# default: min(16384, context_window/2). Set explicitly to exploit a big
# context window (e.g. on a large-memory box) or to tighten further.
max_tokens = 0

[providers.local.sampler]
temperature = 0.6
top_k = 20
top_p = 0.95
min_p = 0.0
presence_penalty = 1.5

# Per-turn generation guards + auto-recovery. Defaults are safe; this
# block only needs editing to tune. By design these are hardware-
# independent (token counts and stall-on-silence, not wall-clock budgets),
# so the same values work whether the box is GPU-fast or big-and-slow.
[providers.local.generation]
# Abort a stream that emits no token for this long (Go duration). Fires on
# silence, not slowness — tolerates prompt-processing pauses and MTP/
# speculative bursts. "0s" disables. Default 60s.
stall_timeout = "60s"
# On a length-truncation, tripped loop guard, or stall: discard/continue
# and retry with a nudge instead of dropping the turn. Default true.
auto_recover = true
# Max automatic retries per turn before giving up. Default 2.
max_recover_attempts = 2
# Mid-stream repetition detection — abort a degeneration loop before it
# even reaches the max_tokens cap. Default true.
loop_guard = true

# --- cloud providers (uncomment to enable) ----------------------------
#
# 'type' selects the vendor adapter. Omit (or set "openai") for any
# OpenAI-compatible endpoint — llama.cpp, vLLM, Ollama, Groq, OpenAI
# proper, OpenRouter, Together, Fireworks. Other types reach hosted
# providers that don't speak the OpenAI wire format.
#
# All hosted endpoints below take their key via $ENSO_* env-var
# indirection — references resolve at config load, so no secrets land
# on disk.
#
# [providers.openai]
# type     = "openai"
# endpoint = "https://api.openai.com/v1"
# model    = "gpt-4o"
# api_key  = "$ENSO_OPENAI_KEY"
#
# [providers.groq]
# type     = "openai"
# endpoint = "https://api.groq.com/openai/v1"
# model    = "llama-3.3-70b-versatile"
# api_key  = "$ENSO_GROQ_KEY"
#
# [providers.openrouter]
# type     = "openai"
# endpoint = "https://openrouter.ai/api/v1"
# model    = "anthropic/claude-3.5-sonnet"
# api_key  = "$ENSO_OPENROUTER_KEY"
#
# [providers.together]
# type     = "openai"
# endpoint = "https://api.together.xyz/v1"
# model    = "meta-llama/Llama-3.3-70B-Instruct-Turbo"
# api_key  = "$ENSO_TOGETHER_KEY"
#
# AWS Bedrock — multi-vendor. Claude, Nova, Llama, Mistral, Cohere,
# and AI21 are all reachable through this one adapter; the model id
# picks which. Auth follows the standard AWS credential chain (env
# vars, ~/.aws/credentials, EC2/ECS/EKS instance role); aws_region
# and aws_profile override it. Model ids are Bedrock's (often a region
# prefix or version suffix, distinct from api.anthropic.com) or an
# inference-profile ARN.
#
# [providers.bedrock-claude]
# type       = "bedrock"
# model      = "anthropic.claude-3-5-sonnet-20241022-v2:0"
# aws_region = "us-east-1"
# # Optional: enable Claude's extended-thinking blocks. Reasoning
# # surfaces through the same channel the TUI already renders for
# # OpenAI reasoning models. Anthropic-only — the API rejects this
# # flag for non-Claude Bedrock models.
# extended_thinking        = true
# extended_thinking_budget = 8000
# # Optional: apply an AWS Bedrock Guardrail. Same keys work on
# # type = "anthropic-bedrock" — see below.
# bedrock_guardrail_id      = "gr-abc123"
# bedrock_guardrail_version = "DRAFT"          # or a numeric version
# bedrock_guardrail_trace   = "enabled"        # enabled | disabled | enabled_full
#
# [providers.bedrock-nova]
# type       = "bedrock"
# model      = "amazon.nova-pro-v1:0"
# aws_region = "us-east-1"
#
# [providers.bedrock-llama]
# type       = "bedrock"
# model      = "meta.llama3-1-70b-instruct-v1:0"
# aws_region = "us-east-1"
#
# GCP Vertex AI — Gemini family. Auth follows Google Application
# Default Credentials (GOOGLE_APPLICATION_CREDENTIALS, gcloud auth
# application-default login, or GCE/GKE metadata). gcp_project is
# required (or set GOOGLE_CLOUD_PROJECT in the environment). Claude
# on Vertex uses a different :rawPredict shape and isn't covered by
# this adapter — track its arrival under the parked anthropic types.
#
# [providers.vertex-gemini]
# type         = "vertex"
# model        = "gemini-2.5-pro"
# gcp_project  = "my-gcp-project"
# gcp_location = "us-central1"
# # Optional: enable Gemini 2.5's thinking output. Routes through the
# # same channel the TUI already renders for OpenAI reasoning models.
# # Budget = 0 leaves Gemini's dynamic mode in effect.
# extended_thinking        = true
# extended_thinking_budget = 0
# # Optional: per-category Gemini safety thresholds. Empty leaves
# # Gemini's defaults in effect. Categories: hate_speech, harassment,
# # dangerous_content, sexually_explicit, civic_integrity. Values:
# # BLOCK_NONE | BLOCK_LOW_AND_ABOVE | BLOCK_MEDIUM_AND_ABOVE |
# # BLOCK_ONLY_HIGH | OFF. Unknown keys/values fail at first Chat.
# [providers.vertex-gemini.vertex_safety]
# hate_speech       = "BLOCK_NONE"
# harassment        = "BLOCK_MEDIUM_AND_ABOVE"
# dangerous_content = "BLOCK_ONLY_HIGH"
#
# [providers.vertex-flash]
# type         = "vertex"
# model        = "gemini-2.5-flash"
# gcp_project  = "my-gcp-project"
# gcp_location = "us-central1"
#
# Anthropic-native paths (opt-in). The bedrock/vertex blocks above route
# Claude through the multi-vendor Converse / generateContent shapes —
# good defaults, good enough for most users. If you need Claude features
# that only the Anthropic Messages API exposes (prompt caching control,
# computer-use blocks, server tools, citations), the three blocks below
# route through anthropic-sdk-go instead. Same translator, different
# transport + auth per block.
#
# [providers.anthropic]
# type     = "anthropic"
# model    = "claude-sonnet-4-5"
# api_key  = "$ENSO_ANTHROPIC_KEY"
# # endpoint = "https://api.anthropic.com"   # only set to route through a proxy
# extended_thinking        = true
# extended_thinking_budget = 8000
# # Optional: opt into Anthropic's ephemeral prompt cache. cache_control
# # markers land on the last system block and the last tool, so the
# # system + tool prefix becomes a stable cacheable block reused across
# # turns. Same flag works on type = "bedrock", "anthropic-bedrock", and
# # "anthropic-vertex". No-op on local / openai-compat providers.
# prompt_caching = true
#
# [providers.anthropic-bedrock]
# type       = "anthropic-bedrock"
# model      = "anthropic.claude-3-5-sonnet-20241022-v2:0"
# aws_region = "us-east-1"
# # aws_profile = "default"                    # optional override of the AWS credential chain
# # bedrock_guardrail_id      = "gr-abc123"    # applied via X-Amzn-Bedrock-Guardrail* headers
# # bedrock_guardrail_version = "DRAFT"
# # bedrock_guardrail_trace   = "enabled"      # enabled | disabled (enabled_full collapses to enabled here)
#
# [providers.anthropic-vertex]
# type         = "anthropic-vertex"
# model        = "claude-3-5-sonnet-v2@20241022"
# gcp_project  = "my-gcp-project"
# gcp_location = "us-east5"
# # Note: Vertex's per-request safety settings only attach to the
# # Gemini path (type = "vertex"). Anthropic-on-Vertex uses :rawPredict
# # which doesn't expose them — Vertex applies its platform-level
# # Model Armor / safety policy in the infrastructure instead.

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

[backend]
# Where the agent core runs. This is the ONLY backend selector — flip
# the type to switch.
#   "local"  (default) = a host child process, no isolation.
#   "podman" = the core runs inside a rootless container (overlay
#              workspace, network-sealed, host-proxied inference).
#   "lima"   = the core runs inside a persistent per-project VM
#              (real-VM isolation).
# Empty or unrecognized = "local" (fails safe).
type = "local"
# Throwaway-overlay toggle (backend-agnostic): "overlay" = the agent
# writes to an ephemeral copy reconciled at task end; "" / "direct" =
# writes hit the project in place.
# workspace = "overlay"
#
# type and workspace above are the only backend keys that belong in
# this (user) config — they are a personal/machine preference.
#
# Each backend's ENVIRONMENT (image, packages/init, lima template,
# mounts, egress allowlist, hardening) is a property of the PROJECT and
# must be reproducible from the repo, so it lives in that repo's
# <repo>/.enso/config.toml — NOT here. Set there, e.g.:
#
#   [backend.podman]
#   image = "golang:1.22"
#   init  = ["apk add --no-cache git"]
#
#   [backend.lima]
#   template = "alpine"   # guest image distro (default); host $HOME is
#                         # never mounted into the VM
#   init     = ["apk add --no-cache git"]
#
#   [backend.egress]            # shared by podman + lima
#   allow       = ["proxy.golang.org", "github.com"]
#   credentials = { GITHUB_TOKEN = "$ENSO_GITHUB_TOKEN" }
#
# These sub-tables are IGNORED (with a warning) if set in this user
# config or the system config — see docs/config reference.

[ui]
# Theme name (default: "dark")
theme = "dark"
# Editor mode for the input field: "default" or "vim".
editor_mode = "default"

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
# configured (or a builtin auto-activates), the lsp_hover /
# lsp_definition / lsp_references / lsp_diagnostics tools become
# available AND the write/edit tools surface live diagnostics for the
# file just edited. Servers are spawned lazily on the first request
# that touches a matching extension.
#
# Builtin defaults auto-activate when the binary is on PATH; you do
# NOT need to declare these to use them. Override or replace by
# adding a [lsp.<name>] block with the same name (your config wins
# entirely). Disable a single builtin by setting command = "" in its
# block; disable them all with the top-level
#   lsp_builtins_disabled = true
#
#   builtin name  | command (must be on PATH)
#   --------------|--------------------------------
#   go            | gopls
#   typescript    | typescript-language-server --stdio
#   python        | pyright-langserver --stdio
#   rust          | rust-analyzer
#
# Example override (force a specific gopls path):
#
# [lsp.go]
# command = "/opt/gopls/bin/gopls-experimental"
#
# Example custom server entirely outside the builtin set:
#
# [lsp.zig]
# command = "zls"
# extensions = [".zig"]
# root_markers = ["build.zig", ".git"]
#
# lsp_builtins_disabled = false   # set true to suppress auto-activation

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
# compress = true                      # command-aware + structural output compression:
#                                      #   declarative per-command filters strip passing-test /
#                                      #   progress / lockfile-diff noise BEFORE the output caps.
#                                      #   Defaults ship in-binary; add or override with
#                                      #   *.toml files under $XDG_CONFIG_HOME/enso/filters/.
#                                      #   The raw output is always spilled to disk, so nothing
#                                      #   compression drops is unrecoverable. Set false to revert
#                                      #   to plain head/tail truncation.
#                                      #   The /discover command ranks recorded bash commands by
#                                      #   output size and flags which ones still lack a filter.
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
