---
title: ensō
type: docs
---

# ensō

> ensō (円相) — the Zen circle, drawn in one breath. Imperfect by
> design; the irregularities are the point.

A small, single-binary, terminal-first agentic coding agent in Go
(binary: `enso`).

ensō talks to any OpenAI-compatible chat endpoint and gives it the
tools to read and edit your project, run shell commands, search the
codebase, fetch URLs, and orchestrate sub-agents. Sessions persist to
SQLite and resume across crashes. Opt into `[backend] type = "podman"`
(or `"lima"`) and the whole agent runs inside a per-project container
or VM so it can't escape the project directory.

## Why ensō

- **Local-first by design.** Built around `llama.cpp`'s `llama-server`
  running Qwen3.6-35B-A3B on a single RTX 3090. Anything that speaks
  the OpenAI Chat Completions wire format works.
- **Single static binary, no CGO.** SQLite via `modernc.org/sqlite`,
  pure-Go LSP and JSON-RPC. Builds for Linux, macOS, and Windows
  without toolchain gymnastics.
- **Persistent everything.** Every user message, assistant reply, tool
  call, and event is written to SQLite *before* the UI renders it.
  `kill -9` mid-tool-call and the session resumes with the interrupted
  call surfaced as a synthetic tool result.
- **Container / VM isolation, opt-in.** Set `[backend] type =
  "podman"` (or `"lima"`) and the whole agent runs in a per-project
  container/VM with the cwd bind-mounted at its real path. File tools
  (`read`/`write`/`edit`/`grep`/`glob`) refuse paths outside cwd
  regardless. Off by default (`"local"`).
- **Real LSP.** Configure any language server under `[lsp.<name>]` and
  the agent gets `lsp_hover`, `lsp_definition`, `lsp_references`, and
  `lsp_diagnostics` tools.

## Where to start

- [Install]({{< relref "install.md" >}}) — get a binary on your machine.
- [Quickstart]({{< relref "quickstart.md" >}}) — first run end-to-end
  in five minutes.
- [User guide]({{< relref "docs/_index.md" >}}) — one page per feature.
- [Config reference]({{< relref "config/reference.md" >}}) — every
  field in `config.toml`.
- [Architecture]({{< relref "advanced/architecture.md" >}}) — what's
  inside the binary.

## Project status

v2 is shipped — the TUI was migrated from `tview` onto Bubble Tea v2
+ Lipgloss + Glamour for scrollback-native rendering. The binary is
in daily use; per-release notes live in
[`CHANGELOG.md`](https://github.com/TaraTheStar/enso/blob/main/CHANGELOG.md),
soak-test risks are documented in the [architecture page]({{< relref
"advanced/architecture.md" >}}), and the non-goals — what's
intentionally not built and why — live in `AGENTS.md` at the repo
root.
