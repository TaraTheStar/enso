---
title: Worktree mode
weight: 3
---

# Worktree mode

`--worktree` creates a fresh git worktree at
`~/.local/state/enso/worktrees/<repo>-<rand>` on a new `enso/<rand>` branch off
your current HEAD, chdirs into it, and runs the session from there.
The agent operates in the worktree; your main checkout stays
untouched.

Useful when:

- You want a sacrificial branch for a speculative refactor without
  contaminating your working tree.
- You're running multiple agents in parallel — each on their own
  worktree, so they can't step on each other's edits.
- You want to keep `git diff` and `git status` clean while the agent
  experiments.

## Usage

```bash
enso --worktree                     # in-process TUI
enso run --worktree --yolo "do thing"   # single-shot
```

Both `enso` (default `tui`) and `enso run` honour the flag. The
daemon and `attach` paths don't — they target an existing process
that might span multiple repos.

The flag is **persistent** at the root level, so `--worktree` works
alongside any other top-level flag.

## What it actually does

```bash
git worktree add -b enso/abc123 ~/.local/state/enso/worktrees/myrepo-abc123
cd ~/.local/state/enso/worktrees/myrepo-abc123
# enso runs from here
```

The 6-hex slug is random per invocation, so concurrent worktree
sessions don't collide.

After the session ends the worktree and branch are **left in place**.
Cleanup is intentional — you might want to keep, merge, or further
edit. To clean up:

```bash
git worktree remove ~/.local/state/enso/worktrees/myrepo-abc123
git branch -D enso/abc123
# or, batch:
git worktree prune        # removes worktrees whose dirs are gone
```

## Failure modes

- **Not in a git repo**: `--worktree` errors with "not in a git repo"
  and exits.
- **`git worktree add` fails**: ensō surfaces git's stderr and exits.
  Common cause: trying to create a worktree on a branch that already
  has a worktree somewhere else.
- **Disk space**: each worktree is a full checkout (hardlinked where
  the filesystem supports it). Cheap on most setups, but worth knowing
  if you spawn dozens.

## Combining with subagents

Worktree pairs well with `spawn_agent` and workflows for parallel
work:

```bash
enso --worktree
> /workflow build-feature add OAuth login
```

Inside the worktree, the workflow's planner / coder / reviewer all
edit the same checkout. Two ensō processes each on their own
worktree (`--worktree` each) can work in parallel without conflict.

## Combining with the sandbox

`--worktree` and an isolating `[backend] type` work together. The sandbox
container is per-cwd (the worktree's path), so each worktree gets its
own container. That means each worktree pays the image-pull and init
cost the first time. If you do a lot of parallel worktree sessions
on the same project, configure a richer base image and slim init
list, or accept the warmup cost.
