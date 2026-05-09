---
title: TUI
weight: 1
---

# TUI

The default `enso` command launches a scrollback-native Bubble Tea
UI: completed messages live in your real terminal scrollback (so
highlight + middle-click copy works exactly like in any tmux pane),
with a small live region at the bottom for the streaming message,
status line, and input. `Ctrl-Space` summons a full-screen alt-screen
session inspector for at-a-glance state.

## Keybindings

| Key                          | Action                                                                |
| ---------------------------- | --------------------------------------------------------------------- |
| `Enter`                      | Submit the current input (or run a `/`-prefixed command).             |
| `Ctrl+C` / `Ctrl+D`          | Quit. (`Ctrl+D` with non-empty input clears the line first; `Ctrl+C` quits unconditionally for now — turn-cancel is on the roadmap.) |
| `Ctrl+Space` (= `Ctrl+@`)    | Open the alt-screen session inspector overlay; Esc returns.           |
| `Ctrl+R`                     | Open the recent-sessions overlay; Enter switches to that session (re-execs with `--session <id>`). |
| `←` / `→` / `Home` / `End`   | Cursor movement in the input.                                         |
| `Ctrl+A` / `Ctrl+E`          | Move to start / end of the input line (readline-style).               |
| `Ctrl+←` / `Ctrl+→`          | Word back / forward.                                                  |
| `@` (at token start)         | Open the file picker — type to filter, Enter inserts the path.        |
| `y` / `n` / `a` / `t`        | Permission prompt: allow / deny / allow + remember / allow for this turn only. |
| `Esc`                        | Close any active overlay (file picker, inspector). On a permission prompt, Esc denies. Double-tap clears the input line. |

> Bubble Tea reports the same chord as either `ctrl+space` or
> `ctrl+@` depending on the terminal's keyboard protocol — both work.

When `[ui] editor_mode = "vim"` is set in config, the input runs the
single-line vim subset: `Esc` enters NORMAL, `i` / `a` / `A` re-enter
INSERT, `h l 0 $ w b x` for navigation and edit.

## Slash commands

| Command                                | What it does                                                    |
| -------------------------------------- | --------------------------------------------------------------- |
| `/help`                                | List all available commands.                                    |
| `/yolo on\|off`                        | Toggle auto-allow for the rest of the session.                  |
| `/tools`                               | List tools currently registered (built-ins + MCP + LSP).        |
| `/info`                                | Print active provider / model / session id / cwd / config search paths. |
| `/sessions`                            | Recent sessions, inline. `Ctrl-R` opens the overlay version.    |
| `/rename [<label>]`                    | Show or override the session's display label (no arg prints the current label). |
| `/export [-o <file>]`                  | Export this session to markdown (stdout by default).            |
| `/fork`                                | Branch this session into a new one and print the new id.        |
| `/stats [--days N]`                    | Token / message / tool aggregates across stored sessions.       |
| `/find [-e] <pattern>`                 | Search this session's transcript (substring; `-e` for regex).   |
| `/grep [--all] [--regex] <pattern>`    | Search past sessions in the local store (cwd-scoped by default). |
| `/permissions [remove <pattern>]`      | Inspect and remove project-local permission rules in `config.local.toml`. |
| `/model [<name>]`                      | List configured providers (no arg) or switch the active one (with arg). |
| `/compact`                             | Force a context-compaction pass.                                |
| `/init [target]`                       | Survey the project and write `ENSO.md` (or any other filename). |
| `/agents`                              | List declarative agent profiles (built-in + user + project).    |
| `/loop <interval> <prompt>`            | Re-submit a prompt every interval (≥5s); `/loop off` stops.     |
| `/workflow <name> <args>`              | Run a declarative workflow.                                     |
| `/lsp`                                 | Configured language servers and their state.                    |
| `/mcp`                                 | Configured MCP servers, state, and tool counts.                 |
| `/git`                                 | Current branch + working-tree status.                           |
| `/cost`                                | Cumulative token totals for this session.                       |
| `/transcript [<id-or-prefix>]`         | List captured subagent transcripts; show one by id-or-prefix.   |
| `/<skill-name> <args>`                 | Any user-defined skill (project shadows user).                  |
| `/quit`                                | Exit.                                                           |

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
- Plain text + `enso ›` prefix — assistant replies (markdown rendered
  via Glamour once a block finalises; raw text while streaming so
  half-fenced blocks don't flicker).
- Teal `▌` — tool calls. Tool output comes back rendered inline; diff-
  shaped output is colour-coded per line (`+` sage, `-` red, `@@`
  teal).
- Gray recede — reasoning blocks (`<think>` content). Once a reasoning
  block closes it shows a `thought for N.Ns` footer.
- Red `✘` — errors.
- Teal parentheticals — system notes (cancelled, compacted, connect,
  disconnect, discarded queued messages).

## Subagent transcripts

Subagents (from `spawn_agent` or workflow roles) write their
transcripts to an in-memory store. List them with `/transcript` and
view a specific one with `/transcript <id-or-prefix>`. The
session-inspector overlay (`Ctrl+Space`) also surfaces in-flight agent
state.

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
