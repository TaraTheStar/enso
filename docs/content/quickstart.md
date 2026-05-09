---
title: Quickstart
weight: 2
---

# Quickstart

Five minutes from "I cloned the repo" to "the agent is editing my code."

## 1. Build

```bash
make build      # → ./bin/enso
```

If this is your very first run, you can also write a config
interactively instead of editing TOML by hand:

```bash
./bin/enso config init --wizard    # pick a provider preset, model, optional API key
```

The wizard writes to `$XDG_CONFIG_HOME/enso/config.toml` and clamps
the file to mode `0600`.

## 2. Start your model

If you're running `llama.cpp`'s `llama-server`:

```bash
llama-server -m <model.gguf> --port 8080
```

If you've configured a different backend, point your config there. See
[Install]({{< relref "install.md" >}}) for the full command line.

## 3. First run

```bash
./bin/enso
```

The first time ensō runs, it writes a default config to
`~/.config/enso/config.toml` and creates `~/.enso/enso.db` for session
storage. The TUI boots, talks to your model, and you can start typing.

Try something simple:

```
list the .go files in cmd/
```

You should see the agent call `glob` and stream back the results. Hit
**Enter** to send. **Ctrl-D** quits (or clears a non-empty input
line). **Ctrl-Space** opens the alt-screen session inspector;
**Ctrl-R** opens the recent-sessions overlay.

## 4. Try a non-interactive run

Single-shot mode prints to stdout and exits when the agent quiesces:

```bash
./bin/enso run --yolo "summarise README.md"
```

`--yolo` auto-allows every tool call. For scripted use you'll usually
want this. For day-to-day TUI work, leave it off and answer the
permission prompts as they come.

## 5. Add structured output

```bash
./bin/enso run --yolo --format json "show me the package layout" | jq .
```

Each event is one JSON object on its own line. Useful for piping into
other tools. See [JSON event schema]({{< relref "advanced/json-events.md" >}}).

## 6. Pick up where you left off

Every session is persisted automatically. Resume the most recent one:

```bash
./bin/enso --continue
```

Or pick by id (printed in `enso stats` and the TUI's `/sessions`):

```bash
./bin/enso --resume <id>
```

To branch a session into a new one (try a different direction without
losing the first):

```bash
new_id=$(./bin/enso fork <id>)
./bin/enso --session "$new_id"
```

## 7. Restrict the agent

Out of the box every tool call prompts you. You can pre-allow common
ones in `~/.config/enso/config.toml` (user) or
`<project>/.enso/config.toml` (project):

```toml
[permissions]
mode  = "prompt"
allow = ["bash(git *)", "read(**)", "grep(**)", "glob(**)"]
ask   = ["bash(git push *)"]    # always prompt, even when otherwise allowed
deny  = ["edit(./.env)", "bash(rm -rf *)"]
```

Drop a `.ensoignore` at the project root with one glob per line to
auto-deny `read`/`write`/`edit`/`grep`/`glob` for those paths and hide
them from the @-file picker:

```
secrets/**
*.pem
.env
```

## 8. Turn on the sandbox (recommended)

For real work, run bash inside a per-project container:

```toml
[bash]
sandbox = "auto"

[bash.sandbox_options]
image = "alpine:latest"
init  = ["apk add --no-cache git curl jq make"]
```

The agent's shell now sees only `/work` (your project) and the
container's rootfs. Host paths outside cwd are invisible. Manage
containers with `enso sandbox list / stop / rm / prune`.

Full details in [Sandbox]({{< relref "docs/sandbox.md" >}}).

## What next

- [TUI guide]({{< relref "docs/tui.md" >}}) — keybindings, slash commands, the status bar.
- [Permissions]({{< relref "docs/permissions.md" >}}) — three-tier rules and per-tool patterns.
- [Sessions]({{< relref "docs/sessions.md" >}}) — resume, fork, export, stats, worktree.
- [Sandbox]({{< relref "docs/sandbox.md" >}}) — per-project container with podman or docker.
- [LSP]({{< relref "docs/lsp.md" >}}) — language servers as agent tools.
