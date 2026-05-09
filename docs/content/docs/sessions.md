---
title: Sessions
weight: 2
---

# Sessions

Every conversation is a *session* — a row in `~/.enso/enso.db` with
its messages, tool calls, and bus events. Sessions persist
automatically (use `--ephemeral` to skip), survive crashes, and can be
resumed, branched, or exported.

## Persist-before-render

Every user message, assistant reply, and tool call is written to
SQLite *before* the UI renders it. If you `kill -9` enso mid-tool-call
and resume, the interrupted call comes back as a synthetic tool result
with the message "tool call interrupted (process exited before
completion); user has resumed the session" so the model can react.

## Resuming

```bash
enso --continue              # most recently updated session
enso --resume <id>           # by id (alias for --session)
enso --session <id>          # original form, identical
```

The id is a UUID. Discover them with:

```bash
enso stats                   # summary across all sessions
```

…or from the `/sessions` slash command in the TUI, or `Ctrl-R`.

## Labels

Each session has a short display label rendered in the recent-sessions
overlay so you don't have to memorise UUIDs. The label is auto-derived
from the first user message; override it with `/rename <text>` (no
arg prints the current label). Labels live alongside the messages in
SQLite — they survive resume and are visible in `/sessions` and
`Ctrl-R`.

## Forking

Branch a session into a new one — useful when you want to try a
different direction without losing the first:

```bash
new_id=$(enso fork <id>)
enso --session "$new_id"
```

Fork copies messages only; sub-agent transcripts and tool-call
metadata are dropped. The fork inherits the source's model, provider,
and cwd.

## Exporting

Dump a session as Markdown — useful for sharing or reviewing offline:

```bash
enso export <id>                       # to stdout
enso export <id> -o session.md         # to a file
```

The export inlines tool results under their matching assistant
message and pretty-prints JSON arguments.

## Stats

```bash
enso stats                       # all sessions
enso stats --days 7              # last week only
```

Reports session counts, messages by role, models used, tool calls by
name (with ok/error/denied splits), and approximate total tokens
(4-char heuristic).

## Worktree-per-task

Spin up a fresh git worktree on a new branch and run the session
there:

```bash
enso --worktree
```

Creates `~/.enso/worktrees/<repo>-<rand>` on a fresh `enso/<rand>`
branch off your current HEAD, chdirs into it, and runs from there.
Useful with subagents (run several in parallel without stepping on
each other) or when you want a sacrificial branch for an
exploration.

Cleanup is manual: `git worktree remove <path>` or `git worktree
prune` when you're done. The branch is plain — merge it, delete it,
rebase it, whatever.

## Detached / fire-and-forget

For long-running prompts you want to leave running:

```bash
enso daemon --detach                       # start the daemon
session_id=$(enso run --detach "do thing") # submit a prompt; print id; exit
enso attach "$session_id"                  # later: tail it interactively
```

The daemon runs as a separate process with a unix socket at
`~/.enso/daemon.sock`. `attach` is a TUI variant that reads events
from the socket and proxies user input + permission decisions back. If
no client is attached when a permission prompt fires, the daemon
auto-denies after 60s.

## Limitations

- Fork copies only top-level messages. Sub-agent transcripts (from
  workflows or `spawn_agent`) are not duplicated. Re-running a
  workflow on the fork is a clean way to regenerate them.
- `--continue` doesn't filter by cwd. If you have sessions across
  multiple projects, the most recent wins regardless of where you run
  enso. Use `enso stats` or the sessions overlay to pick by project.
- The daemon path is POSIX-only and doesn't currently expose `lsp_*`
  tools or `[bash] sandbox`. Use the in-process path for those.
