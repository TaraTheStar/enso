---
title: TUI
weight: 1
---

# TUI

The default `enso` command launches a tview-based terminal UI: a chat
pane, an input area at the bottom, an optional agents pane on the
right, and a status bar.

## Keybindings

| Key                          | Action                                                                |
| ---------------------------- | --------------------------------------------------------------------- |
| `Enter`                      | Submit the current input.                                             |
| `Shift+Enter` / `Alt+Enter` / `Ctrl+J` | Newline. `Ctrl+J` is the cross-terminal-reliable one.       |
| `Ctrl+C`                     | Cancel the current turn (no-op when idle).                            |
| `Ctrl+D`                     | Quit (cancels in-flight turn first).                                  |
| `Ctrl+A`                     | Toggle the right-side agents pane.                                    |
| `Ctrl+T`                     | Toggle visibility of completed reasoning blocks.                      |
| `Ctrl+R`                     | Open the recent-sessions overlay (Enter switches via `syscall.Exec`). |
| `@` (at token start)         | Open the file picker — type to filter, Enter inserts the path.        |
| `Esc`                        | Close any modal (= **Deny** on a permission prompt).                  |

## Slash commands

| Command                        | What it does                                                    |
| ------------------------------ | --------------------------------------------------------------- |
| `/help`                        | List all available commands.                                    |
| `/yolo on\|off`                | Toggle auto-allow for the rest of the session.                  |
| `/tools`                       | List tools currently registered (built-ins + MCP + LSP).        |
| `/sessions`                    | Recent sessions, inline. Ctrl-R opens the overlay version.      |
| `/grep <pattern>`              | One-shot project-wide grep, results inline.                     |
| `/permissions`                 | Inspect and remove project-local permission rules in `config.local.toml`. |
| `/model [<name>]`              | List configured providers (no arg) or switch the active one (with arg). |
| `/compact`                     | Force a context-compaction pass.                                |
| `/init [target]`               | Survey the project and write `ENSO.md` (or any other filename). |
| `/agents`                      | List declarative agent profiles (built-in + user + project).    |
| `/loop <interval> <prompt>`    | Re-submit a prompt every interval (≥5s); `/loop off` stops.     |
| `/workflow <name> <args>`      | Run a declarative workflow.                                     |
| `/<skill-name> <args>`         | Any user-defined skill (project shadows user).                  |
| `/quit`                        | Exit.                                                           |

## Status bar

Two halves:

- **Left**: `MODE · activity`. `MODE` is `PROMPT` (default), `AUTO`
  when yolo is on, with vim-mode appending `· NORMAL` / `· INSERT`
  when `[ui] editor_mode = "vim"`.
- **Right**: by default `[provider] model · session-short · used/window`.
  Tokens are color-coded: yellow at ≥50% of the context window, red
  at ≥80%. Customisable — set `[ui] status_line` to a Go
  `text/template` string with the variables `.Provider .Model
  .Session .Mode .Activity .Tokens .Window .TokensFmt`.

## Chat lanes

Different message types render with distinct prefixes:

- Yellow `▌` — your messages.
- Plain text — assistant replies.
- Teal `▌` — tool calls.
- Gray `▎` — reasoning blocks (`<think>` content). Collapse with
  `Ctrl+T`; collapsed blocks show as `thought for N.Ns`.
- Red `✘` — errors.
- Teal parentheticals — system notes (cancelled, compacted, connect,
  disconnect).

## Agents pane

`Ctrl+A` toggles a right-side tree showing every spawned subagent and
workflow role for the current session. Click a node to open a
read-only transcript overlay of that agent's history.

## File picker

Typing `@` at a token boundary (start of input, after a space, or
after a newline) pops a fuzzy file picker. The walker covers the
project cwd plus any directories listed under
`[permissions] additional_directories`. `.ensoignore` filters apply.
Enter inserts the path; Esc dismisses.

## Theming

Drop a `~/.enso/theme.toml` to override the default color palette:

```toml
[colors]
yellow = "#ffd866"
teal   = "#78dce8"
gray   = "#727072"
red    = "#ff6188"
green  = "#a9dc76"
```

Each entry is a hex `#rrggbb`. Typos log a warning to
`~/.enso/enso.log` and fall back to defaults; they never block the TUI.
