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
| `Shift+Enter` / `Alt+Enter` / `Ctrl+J` | Insert a literal newline in the input. The input soft-wraps and scrolls up to three rows. |
| `Ctrl+C`                     | Cancel the in-flight turn while busy (prints `(cancelling turn — press Ctrl-C again to force quit)`); a second `Ctrl+C` within ~500 ms force-quits even if the cancel itself wedged. While idle, `Ctrl+C` quits immediately. |
| `Ctrl+D`                     | Quit. With non-empty input, clears the line first. |
| `Ctrl+Space` (= `Ctrl+@`)    | Open the alt-screen session inspector overlay; Esc returns.           |
| `Ctrl+R`                     | Open the recent-sessions overlay; Enter switches to that session (re-execs with `--session <id>`). |
| `←` / `→` / `Home` / `End`   | Horizontal cursor movement in the input.                              |
| `↑` / `↓`                    | Move the cursor up / down a visual row through multi-line / soft-wrapped input, keeping the column. On the top row `↑` jumps to the buffer start; on the bottom row `↓` jumps to the end. |
| `Ctrl+A` / `Ctrl+E`          | Move to start / end of the input line (readline-style).               |
| `Ctrl+←` / `Ctrl+→`          | Word back / forward.                                                  |
| `@` (at token start)         | Open the file picker — type to filter, Enter inserts the path.        |
| `y` / `n` / `a` / `t`        | Permission prompt: allow / deny / allow + remember / allow for this turn only. |
| `Esc`                        | Close any active overlay (file picker, inspector). On a permission prompt, Esc denies. Double-tap clears the input line. |

> Bubble Tea reports the same chord as either `ctrl+space` or
> `ctrl+@` depending on the terminal's keyboard protocol — both work.
> `Shift+Enter` for newline only reaches enso when the terminal speaks
> the Kitty keyboard protocol; `Alt+Enter` and `Ctrl+J` are reliable
> fallbacks for terminals that fold `Shift+Enter` into a bare `Enter`.

Terminal bracketed paste (`Ctrl-Shift-V` / `Cmd-V` / middle-click X11
PRIMARY) preserves newlines verbatim — `\r\n` and bare `\r` are
normalised to `\n`; `\n` is kept. Multi-line snippets paste as
multi-line; `Enter` submits the whole buffer. (Plain `Ctrl-V` is not a
raw-mode paste in terminals and intentionally does nothing.)

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
| `/sessions [--all]`                    | Recent sessions, inline (current directory by default; `--all` for every directory). `Ctrl-R` opens the overlay version. |
| `/rename [<label>]`                    | Show or override the session's display label (no arg prints the current label). |
| `/export [-o <file>]`                  | Export this session to markdown (stdout by default).            |
| `/fork`                                | Branch this session into a new one and print the new id.        |
| `/rewind`                              | Undo to an earlier turn — restore files and/or conversation (overlay turn picker). |
| `/stats [--days N]`                    | Token / message / tool aggregates across stored sessions.       |
| `/find [-e] <pattern>`                 | Search this session's transcript (substring; `-e` for regex).   |
| `/grep [--all] [--regex] <pattern>`    | Search past sessions in the local store (cwd-scoped by default). |
| `/permissions [remove <pattern>]`      | Inspect and remove project-local permission rules in `config.local.toml`. |
| `/model [<name>]`                      | List configured providers (no arg) or switch the active one (with arg). |
| `/compact`                             | Force a context-compaction pass. The model can also queue a compaction itself by calling the built-in `checkpoint` tool — typically right after a commit at a logical step boundary — which runs before the model's next response. |
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
- **Right**: `[provider] model · session-short · used/window`.
  Tokens are color-coded: yellow at ≥50% of the context window, red
  at ≥80%.

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
Enter inserts the path as an `@<path>` mention; Esc dismisses (and
restores any text you had typed after the `@`).

### Image input

If an `@<path>` mention points at an image file — PNG, JPEG, GIF, or
WebP, up to 10 MiB — its bytes are attached to the message and shown to
a vision-capable model, while the `@path` text stays as a human-readable
reference (e.g. `look at @diagram.png`). A `📎 attached` notice confirms
the attachment in scrollback. Images are resolved on the host, so
isolated (podman/lima) backends that can't see your filesystem still
receive them. The `read` tool reads images the same way.

## Rewind

`/rewind` opens an overlay to roll the session back to an earlier turn.
First pick the turn — each per-turn checkpoint shows its turn number, a
preview of that turn's message, and a relative timestamp (the most
recent is selected by default). Then choose what to restore:

- **[1]** code + conversation
- **[2]** conversation only (keep files)
- **[3]** code only (keep conversation)

Restoring code mirrors the workspace snapshot taken just before that
turn back over the working tree — reverting modified files and deleting
files added since — without ever touching `.git`. Before the
conversation is truncated the prior thread is preserved as a forked
session (the overlay prints how to resume it with `enso --session
<id>`), and the rewound-away message is pre-filled into the input so you
can re-send or edit it.

Per-turn checkpointing is on by default and configured under the
[`[checkpoints]`]({{< relref "../config/reference.md" >}}) block. `/rewind` is
unavailable under `--ephemeral`, and on isolated backends only
conversation rewind is available when no workspace overlay is in use.

## Theming

Drop a `~/.config/enso/theme.toml` to override the default color palette:

```toml
[colors]
yellow = "#ffd866"
teal   = "#78dce8"
gray   = "#727072"
red    = "#ff6188"
green  = "#a9dc76"
```

Each entry is a hex `#rrggbb`. Typos log a warning to
`~/.local/state/enso/enso.log` and fall back to defaults; they never block the TUI.
