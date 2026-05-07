---
title: Daemon
weight: 11
---

# Daemon mode

`enso daemon` runs a long-lived agent server on a unix socket
(`~/.enso/daemon.sock`). `enso run --detach` submits a
fire-and-forget prompt to it and exits with the new session id.
`enso attach <id>` opens an interactive TUI driven by the live event
stream.

The daemon is **POSIX-only** — Linux, macOS, BSD. Windows users run
via WSL.

## When to use the daemon

- **Long-running prompts** you want to leave going while you do other
  work. `enso run --detach "audit the whole repo for X"` returns
  immediately with a session id.
- **Agent operations from automation / cron / CI** that need to
  outlive the invoking script.

For interactive day-to-day work, `enso` (the in-process TUI) is the
right tool. The daemon path is intentionally narrower.

## Workflow

```bash
# Start the daemon (one-time, foreground).
enso daemon

# …or detached:
enso daemon --detach
# → prints child PID and socket path; returns immediately.
# → child writes to ~/.enso/enso.log.

# Submit a prompt — yolo by default (no UI to prompt).
session_id=$(enso run --detach "summarise README.md")
echo "$session_id"   # → 4d8b2e9a-…

# Tail it interactively whenever:
enso attach "$session_id"
```

`attach` is a TUI variant that reads events from the daemon's socket
and proxies your input + permission decisions back. If a permission
prompt fires while no client is attached, the daemon auto-denies
after **60 seconds** so the agent doesn't stall forever. Adjust via
the constant in `daemon/server.go` if it bites.

## Reconnecting

`attach` reconnects automatically on daemon restart — the events
cursor is preserved via `from_seq` so anything still in the ring
buffer replays.

## Locking and uniqueness

Only one daemon runs at a time. `enso daemon --detach` against an
already-running daemon prints "daemon already running" and exits
without starting a second.

`~/.enso/daemon.pid` holds the running daemon's PID; it's cleaned up
on graceful exit. A stale PID file from a crashed daemon is detected
and replaced.

## Limitations

The daemon path **does not currently expose**:

- `[bash] sandbox` — per-session cwd would need a multi-manager
  indirection that's not in v1 scope. Use `enso run` (in-process)
  if you need the sandbox.
- `lsp_*` tools — same per-session cwd issue.

These are intentional v1 scope decisions, not deferred bugs. See
the **Non-goals** section of `AGENTS.md` for the design context.

## Common debugging

```bash
# What's the daemon doing?
tail -f ~/.enso/enso.log

# Is the socket reachable?
ls -la ~/.enso/daemon.sock
# → srwxr-xr-x ...   (s = socket)

# Kill a stuck daemon.
cat ~/.enso/daemon.pid | xargs kill
rm -f ~/.enso/daemon.{sock,pid}
```

The daemon's PID file and socket are both under `~/.enso/`. If you
ever need to nuke and restart, those two files are everything.
