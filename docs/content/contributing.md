---
title: Contributing
weight: 40
---

# Contributing

ensō is small enough that the contributor flow is mostly "read
`AGENTS.md`, run `make check`, send a patch."

## Operating instructions for agents (and humans)

`AGENTS.md` at the repo root is the source of truth. Highlights:

- **Always run `make check`** before sending a patch — `gofmt + vet +
  test + build`. CI runs the same target.
- **No CGO.** Every dependency must build with `CGO_ENABLED=0`. SQLite
  uses `modernc.org/sqlite`, not `mattn/go-sqlite3`.
- **No new dependencies without surfacing the choice.** ensō is
  deliberately minimal; we have a documented short list in
  `AGENTS.md`.
- **Follow `AGENTS.md`.** It's the architectural source of truth.
  Anything that contradicts its conventions or non-goals (package
  layout change, dep substitution, new abstraction) needs to be
  discussed first.
- **Don't `git push` or `git commit` unless asked.** Stage and report
  what's ready; let the reviewer commit.
- **Don't run destructive commands** (`rm -rf`, `git reset --hard`,
  `git push --force`) without confirmation in the same turn.

## Repo layout

See [Architecture]({{< relref "advanced/architecture.md" >}}) for the
full package map. Quick orientation:

| Path                    | What lives there                                  |
| ----------------------- | ------------------------------------------------- |
| `cmd/enso/`             | CLI entry points + per-subcommand wiring.         |
| `internal/agent/`       | Chat→tool loop, compaction, sub-agent spawn.      |
| `internal/tools/`       | Built-in tools and the registry.                  |
| `internal/ui/`          | Framework-agnostic UI surface (Run / RunAttached / Options) plus the `bubble/` Bubble Tea backend. Only imported from `cmd/enso`. |
| `internal/session/`     | SQLite store; persist-before-render writer.       |
| `internal/permissions/` | Allowlist + matcher + ignore loader.              |
| `internal/sandbox/`     | podman/docker manager.                            |
| `internal/lsp/`         | LSP client (JSON-RPC + lifecycle).                |
| `internal/mcp/`         | MCP client + tool adapter.                        |
| `internal/workflow/`    | Declarative workflow parser + topo runner.        |
| `examples/`             | Workflow / skill / agent examples.                |
| `docs/`                 | This site (Hugo + hugo-book).                     |

## Style

- **Imports**: stdlib first, third-party second, internal third —
  separated by blank lines. `gofmt` enforces.
- **Errors**: wrap with `fmt.Errorf("doing X: %w", err)`. Don't
  `panic` outside `cmd/enso/main.go`. Errors propagate up; the agent
  loop is the recovery point.
- **Logging**: `log/slog` with structured fields. Default text
  handler writing to `~/.enso/enso.log`. Stderr is never written
  from inside the TUI (it'd corrupt the screen).
- **Concurrency**: prefer `context.Context` for cancellation. No naked
  goroutines without a way to stop them. Channels for events; mutexes
  only when channels would be awkward.
- **Tests**: table-driven where it fits. The gnarly bits already have
  tests (see the [test coverage table in architecture]({{< relref
  "advanced/architecture.md" >}})); add tests for new gnarly logic
  but don't bother testing thin glue or Bubble Tea View output.
- **Comments**: write very few. Identifiers carry the meaning. A
  comment is for explaining a non-obvious *why* — a hidden constraint,
  a workaround for an upstream bug, an invariant. If removing it
  wouldn't confuse a reader, don't write it.

## Sending a patch

```bash
git checkout -b my-change
# … edit …
make check
git add -A && git commit -m "your message"
git push origin my-change
# open PR
```

CI runs `make check` on push and PR to `main`.

## Adding to the docs

This Hugo site lives at `docs/`. To preview locally:

```bash
cd docs
hugo mod get -u   # first run only
hugo server       # http://localhost:1313
```

Edit any `.md` under `content/`; Hugo hot-reloads. Page ordering in
the sidebar comes from frontmatter `weight`.

A push to `main` rebuilds and publishes via the GH Action at
`.github/workflows/docs.yml`.

## Bigger changes

Anything architectural — a new internal package, a public-API
change, a new abstraction layer — should start with a discussion
issue. The maintainers' default is "you ship the design first,
implementation second."
