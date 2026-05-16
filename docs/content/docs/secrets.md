---
title: Secrets
weight: 13
---

# Secrets

ensō is local-first; there's no central secret store. The patterns
below cover provider API keys, MCP server tokens, and per-repo
secrets without paying for an OS keyring integration nobody asked
for.

## The three surfaces

| Where                                    | What                                | How to set                                              |
| ---------------------------------------- | ----------------------------------- | ------------------------------------------------------- |
| `[providers.<name>] api_key`             | LLM endpoint bearer token           | TOML literal *or* `$ENSO_FOO` reference                 |
| `[mcp.<name>] headers` / `args`          | Per-server auth (token, header)     | `$ENSO_FOO` reference (literal also works)              |
| `<cwd>/.enso/config.local.toml`          | Per-repo overrides (gitignored)     | Free-form TOML; same expansion rules apply              |

## `$ENSO_*` env-var indirection

Any `$VAR` or `${VAR}` reference inside the fields above is
expanded against the process environment, **but only for variables
whose name starts with `ENSO_`**. A reference to anything else
(e.g. `$AWS_SECRET_ACCESS_KEY`) collapses to the empty string and
logs one `WARN` to `~/.local/state/enso/enso.log`.

```toml
# ~/.config/enso/config.toml — committable; no actual secret in the file.
[providers.cloud]
endpoint = "https://api.example.com/v1"
model    = "some-model"
api_key  = "$ENSO_CLOUD_KEY"
```

```bash
# ~/.bashrc / ~/.zshrc / a systemd EnvironmentFile / direnv .envrc
export ENSO_CLOUD_KEY="sk-..."
```

The empty-on-miss behaviour is deliberate: a misconfigured token
fails loudly with HTTP 401 instead of silently falling through to
some other secret in your shell env.

### Why the prefix?

Threat: `git clone <hostile-repo> && cd <hostile-repo> && enso`. The
[trust prompt]({{< relref "../docs/permissions.md" >}}) blocks
auto-loading of a committed `.enso/config.toml` until you accept it.
But once you've trusted a project, nothing stops a *later* commit
from adding `api_key = "$AWS_SECRET_ACCESS_KEY"` and shipping the
value to a hostile endpoint on the next `enso` run. The `ENSO_`
prefix means you have to opt every secret in by re-exporting:

```bash
export ENSO_GITHUB_TOKEN="$GITHUB_TOKEN"   # explicit opt-in
```

Implementation: [`internal/config/env.go`](https://github.com/TaraTheStar/enso/blob/main/internal/config/env.go).

## Per-repo secrets

There's no first-class "repo secret" type. The pattern uses three
files you already have:

1. **`<cwd>/.enso/config.toml`** — committed. Reference secrets by
   `$ENSO_FOO`; never paste literals here. Trust-gated on first
   load.
2. **`<cwd>/.enso/config.local.toml`** — gitignored. Free for
   project-specific overrides; trust gating doesn't apply (it's
   rewritten by enso itself for "Allow + Remember" rules).
3. **Your shell** — exports the actual `ENSO_FOO` values. `direnv`
   per-project `.envrc` files pair well: ensō inherits whatever
   shell it was launched from.

Concretely, a project-local provider with a per-project key:

```toml
# .enso/config.toml — committed
[providers.team-cloud]
endpoint = "https://team-llm.example.com/v1"
model    = "internal-7b"
api_key  = "$ENSO_TEAMCLOUD_KEY"
```

```bash
# .envrc (direnv) — gitignored
export ENSO_TEAMCLOUD_KEY="sk-..."
```

Anyone cloning the repo gets the wiring; their own `.envrc`
provides the actual value.

## File permissions

When ensō writes the user config (`enso config init`) it clamps the
parent directory to `0700` and the file to `0600`. If you wrote your
own config by hand, double-check:

```bash
chmod 700 ~/.config/enso
chmod 600 ~/.config/enso/config.toml
```

## What is *not* in scope

- **OS keyring integration** (Secret Service / macOS Keychain /
  Windows Credential Manager). Adds a CGO-adjacent dependency and a
  new failure mode (locked keyring on headless / SSH boxes) that
  isn't justified for the local-first single-user case. File a
  request if your setup actually needs it.
- **Encrypted-at-rest config files.** The `0600` file permission is
  the same protection your `~/.ssh/id_*` keys rely on; layering
  symmetric encryption on top would just move the password problem
  to a different file.
- **Per-session secret scoping.** Every session in a process sees
  the same expanded env. Run separate `enso` invocations with
  different `ENSO_*` exports if you need per-session isolation.
