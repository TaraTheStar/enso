---
title: Permissions
weight: 3
---

# Permissions

The permission system gates every tool call by matching it against
patterns in `[permissions]`. Three rule lists, evaluated in
**deny → ask → allow → mode default** order.

## Modes

```toml
[permissions]
mode = "prompt"   # "prompt" | "allow" | "deny"
```

- `"prompt"` — for unmatched calls, ask the user via a modal.
- `"allow"` — for unmatched calls, auto-allow.
- `"deny"` — for unmatched calls, auto-deny.

`--yolo` (or `/yolo on`) overrides the mode and auto-allows everything
except patterns explicitly listed in `deny`. Use it for unattended
runs.

## Pattern syntax

All patterns have the shape `tool(arg-pattern)`:

```toml
allow = ["bash(git *)", "edit(./src/**)", "web_fetch(domain:example.com)"]
ask   = ["bash(git push *)"]                 # always prompt, even when otherwise allowed
deny  = ["bash(rm -rf *)", "edit(./.env)"]
```

Per-tool argument matching:

| Tool                                       | Match against            | Example                                 |
| ------------------------------------------ | ------------------------ | --------------------------------------- |
| `bash`                                     | The shell command        | `bash(git *)`, `bash(git push *)`       |
| `read` / `write` / `edit` / `grep`         | The path arg             | `edit(./src/**)`, `read(**/*.md)`       |
| `glob`                                     | The pattern arg          | `glob(**/*.go)`                         |
| `web_fetch`                                | The URL                  | `web_fetch(domain:example.com)`         |
| `web_search`                               | The query                | `web_search(*)`, `web_search(rust *)`   |
| `spawn_agent`                              | The role arg             | `spawn_agent(reviewer)`                 |
| anything else (MCP, custom)                | All args as `k=v` string | `mcp__github__list_issues(repo=foo)`    |

**Bash patterns**:

- A single token (`bash(git)`) matches the command's first word only.
- Multi-word patterns (`bash(git push *)`) match the whole command
  with `*` crossing spaces and slashes.

**Path patterns** (read/write/edit/grep/glob) use doublestar globs.
`./src/**` matches everything under `./src/` recursively.

**`web_fetch(domain:...)`** matches the URL's host (case-insensitive).
The bare-host pattern `domain:example.com` also matches subdomains
like `api.example.com`.

## The `ask` tier

`ask` rules force a prompt even when the call would otherwise be
auto-allowed. Useful for blast-radius commands you've broadly
permitted:

```toml
allow = ["bash(*)"]                    # let the agent run anything…
ask   = ["bash(git push *)",           # …but always confirm a push
         "bash(rm -rf *)"]             #    or a recursive delete
```

The modal still shows up; declining still works.

## .ensoignore

A first-class file at the project root, gitignore-style:

```
# .ensoignore
secrets/**
*.pem
.env
config/credentials.toml
```

Each non-empty, non-comment line is added as a deny pattern for
`read`, `write`, `edit`, `grep`, and `glob`. Patterns are also fed to
the @-file picker so ignored files don't appear there.

`!` negation is *not* supported — use explicit `[permissions] allow`
rules for exceptions.

## "Allow + Remember"

When a permission prompt fires, the modal offers three buttons:
**(a)llow** (allow this call only), **(r)emember** (allow + persist
the rule), **(d)eny**. Esc is a shortcut for deny.

**(r)emember** writes the pattern to
`<cwd>/.enso/config.local.toml` (project-scoped, gitignored). The
pattern derivation:

| Tool             | Generalisation                                       |
| ---------------- | ---------------------------------------------------- |
| `bash`           | First word + `*` (so `git status` becomes `bash(git *)`). |
| `read`/`grep`    | `<tool>(**)` — read-only, broadly safe.              |
| `write`/`edit`   | Exact path: `write(src/x.go)` or `edit(.env)`.       |
| `web_fetch`      | Exact URL.                                           |
| anything else    | `<tool>(*)`.                                         |

You can also write rules manually anywhere in the layered config —
project, user, or system level. See [Config reference]({{< relref
"../config/reference.md" >}}) for the layering rules.

## additional_directories

Workspace-extension setting — tell the agent (and the @-picker) about
directories it can operate on alongside cwd:

```toml
[permissions]
additional_directories = ["~/notes/projects/alpha"]
```

The directories are mentioned in the system prompt so the model knows
they exist. The @-picker walks them. Combined with the
[sandbox]({{< relref "sandbox.md" >}}), file tools are confined to
cwd + these directories.

## Layered config and where rules live

Permission patterns merge across these files in order (lowest →
highest precedence):

1. `/etc/enso/config.toml` — system-wide.
2. `$XDG_CONFIG_HOME/enso/config.toml` (≈ `~/.config/enso/`) — user.
3. `<cwd>/.enso/config.toml` — project, committed.
4. `<cwd>/.enso/config.local.toml` — project, gitignored. The
   "Allow + Remember" target.
5. `-c <path>` on the command line — one-off override.

Lists concatenate; one project remembering `bash(make *)` doesn't
leak the rule to another project.
