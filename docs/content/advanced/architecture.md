---
title: Architecture
weight: 1
---

# Architecture

A single Go binary with subcommands `tui` (default), `run`, `daemon`,
`attach`, `config`, `trust`, `fork`, `export`, `stats`, `sandbox`.
~26,000 lines of Go (~32% test).

## Big picture

Inside one process the **agent goroutine** runs the chat→tool loop
(`internal/agent`), publishes typed events on a fan-out **bus**
(`internal/bus`), and persists state synchronously through a **session
writer** (`internal/session`) before the UI sees anything
(persist-before-render invariant). The **TUI** (`internal/tui` over
`tview`/`tcell`) subscribes for rendering, the **permissions checker**
(`internal/permissions`) gates tool calls with three-tier patterns,
**MCP** servers (`internal/mcp` via `mark3labs/mcp-go`) plug their
tools into the same registry as built-ins (`internal/tools`),
**subagents** (`spawn_agent` tool + `internal/workflow` runner)
construct child agents that share the parent's bus/provider/permissions
but get their own filtered tool registry. The **daemon**
(`internal/daemon`) hosts the same agent over a unix socket;
**attach** is a TUI variant that reads events from the socket and
proxies user input + permission decisions back. **Slash commands**
(`internal/slash`) layer built-ins + file-loaded skills on top of the
input handler. **LSP** (`internal/lsp`) is a hand-rolled JSON-RPC
client that exposes language-server features as `lsp_*` tools.
**Sandbox** (`internal/sandbox`) runs `bash` inside a per-project
podman/docker container.

## Package layout

```
cmd/enso/
  main.go                        cobra root, subcommands, flag wiring
  config_cmd.go                  enso config init|show
  trust.go                       enso trust [--list|--revoke]
  daemon_detach.go (!windows)    enso daemon --detach re-exec
  run.go                         enso run non-interactive + --detach + --workflow
  worktree.go                    --worktree git-worktree creation
  export.go                      enso export
  stats.go                       enso stats
  sandbox_cmd.go                 enso sandbox list|stop|rm|prune

internal/
  agent/                         chat→tool loop, compaction, spawn
  agents/                        declarative agent profiles (Spec, Builtins, Apply)
  bus/                           typed event fan-out hub
  config/                        layered TOML loader, AppendAllow, defaults, trust
  daemon/                        unix-socket server/client + protocol (POSIX)
  embed/                         //go:embed default_enso.md (system prompt)
  hooks/                         on_file_edit / on_session_end shell hooks
  instructions/                  three-tier system prompt loader + auto-memory
  llm/                           OpenAI-compatible streaming client
  lsp/                           LSP client (JSON-RPC + lifecycle + manager)
  mcp/                           MCP client + tools.Tool adapter
  permissions/                   matcher + allowlist + checker + DerivePattern + ignore
  picker/                        @-file picker walker + ranker
  sandbox/                       podman/docker manager, image lifecycle
  session/                       SQLite store, writer, resume, stats, fork
    migrations/0001_init.sql
                /0002_events.sql
                /0003_messages_agent_id.sql
  slash/                         slash command registry + skill loader
  tools/                         built-in tools + Registry + Transcripts
  tui/                           tview-based UI
  workflow/                      parse + parallel topo runner

examples/
  workflows/build-feature.md     planner → coder → reviewer
  skills/explain-this.md         read-only summariser
  agents/reviewer.md             read-only diff critic
```

## Build invariants

- `gofmt -l .` — empty.
- `CGO_ENABLED=0 go build ./...` — clean. SQLite via
  `modernc.org/sqlite` (pure Go).
- `go vet ./...` — clean.
- `go test ./...` — 19 internal packages (17 with tests; only `bus`
  and `embed` have none), ~300 test functions across the tree.
- `make check` — fmt + vet + test + build, the full pre-commit gate.
- Daemon path is POSIX-only via `//go:build !windows` tags. Windows
  build succeeds but `enso daemon` errors at runtime with a clear
  message.

## Persist-before-render invariant

The agent goroutine writes every user message, assistant reply, and
tool call to SQLite **before** publishing the corresponding bus event.
The TUI renders from the bus. So if ensō crashes mid-tool-call, the
user message and assistant tool-call are persisted, the tool result
is missing, and `Load` detects the unanswered tool-call and inserts a
synthetic "interrupted" tool message so the next turn has well-formed
state.

This is the only thing that lets `kill -9` mid-call resume cleanly.

## Auto-compaction

When a session's estimated token count crosses 60% of the provider's
`context_window`, ensō runs a one-shot non-streaming chat to summarise
the older history, replaces those messages with a single synthetic
summary, and continues. Compaction always splits at a tool-call
boundary so partial tool sequences don't get orphaned.

The trigger is conservative — 60% leaves headroom for the summary
itself plus the upcoming response. Adjust by tuning `context_window`
(setting it lower triggers compaction sooner).

## Soak-test risks

Honest "watch for it" items, not blockers:

- **Qwen3 chat-template tool-call extraction** is fragile across
  llama.cpp versions. Client-side guards (the `<think>` tag-state
  machine, empty-assistant-message protection) handle most breakage,
  but new template variants surface as model output going to the
  wrong lane or HTTP 400 on the *next* turn ("Assistant message must
  contain either 'content' or 'tool_calls'!"). Check
  `~/.enso/enso.log` first.
- **Workflow sibling parallelism** is goroutine-correct but not
  load-tested at large fan-outs. Three-role pipelines work; 10+
  siblings with shared output state under mutex are unexplored.
