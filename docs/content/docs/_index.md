---
title: User guide
weight: 10
bookCollapseSection: true
---

# User guide

One page per major feature. Pick whichever's relevant.

| Topic                                                  | What it covers                                              |
| ------------------------------------------------------ | ----------------------------------------------------------- |
| [TUI]({{< relref "tui.md" >}})                         | Keybindings, slash commands, status bar, agents pane        |
| [Sessions]({{< relref "sessions.md" >}})               | Persistence, `--continue`, `--resume`, `fork`, `export`     |
| [Permissions]({{< relref "permissions.md" >}})         | Three-tier rules, per-tool patterns, `.ensoignore`          |
| [Sandbox]({{< relref "sandbox.md" >}})                 | Per-project podman / docker container for `bash`            |
| [LSP]({{< relref "lsp.md" >}})                         | Language servers as agent tools                             |
| [Agents]({{< relref "agents.md" >}})                   | Declarative agent profiles, plan mode, `--agent`            |
| [Prompt layering]({{< relref "prompt-layering.md" >}}) | `ENSO.md`/`AGENTS.md`, append vs `replace`, `/prompt`       |
| [Memory]({{< relref "memory.md" >}})                   | `memory_save` tool and auto-loaded sidecar files            |
| [Skills]({{< relref "skills.md" >}})                   | User-defined slash commands                                 |
| [Workflows]({{< relref "workflows.md" >}})             | Declarative pipelines (planner → coder → reviewer)          |
| [MCP]({{< relref "mcp.md" >}})                         | Model Context Protocol servers                              |
| [Daemon]({{< relref "daemon.md" >}})                   | Long-lived agent server + `enso attach`                     |
| [Pools & concurrency]({{< relref "pools.md" >}})       | Provider pools, `queue_timeout`, cross-session coordination |
| [Secrets]({{< relref "secrets.md" >}})                 | API keys, MCP tokens, per-repo secrets, `$ENSO_*` env       |
