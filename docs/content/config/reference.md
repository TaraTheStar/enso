---
title: Reference
weight: 1
---

# Configuration reference

ensō reads layered TOML files. Lowest → highest precedence:

1. `/etc/enso/config.toml` — system.
2. `$XDG_CONFIG_HOME/enso/config.toml` (≈ `~/.config/enso/`) — user.
3. `<cwd>/.enso/config.toml` — project, committed.
4. `<cwd>/.enso/config.local.toml` — project, gitignored. The
   "Allow + Remember" target.
5. `-c <path>` — one-off override.

Each file is map-merged into the previous. Lists concatenate; nested
tables merge key-by-key.

```bash
enso config init             # write the default to the user path
enso config init --print     # dump to stdout
enso config init --force     # overwrite an existing file
enso config show             # list search paths and which exist
```

The default config below is also embedded in the binary — `enso
config init --print` prints it verbatim.

## `default_provider`

Top-level scalar; names which `[providers.X]` is active at session
start. **Must appear before any `[providers.X]` table** — TOML scopes
top-level scalars after a section header into that section.

```toml
default_provider = "qwen-fast"

[providers.qwen-fast]
# ...
```

If unset, the alphabetically-first key wins. `/model <name>` swaps
the active provider mid-session; workflow YAML's role `model:` and
`spawn_agent`'s `model` arg pick a different one per role/call.

## `[providers.<name>]`

LLM endpoints. Define one block per (endpoint, model) bundle. To
expose multiple models behind the same llama-swap or Ollama endpoint,
duplicate the block with a different `model` field.

```toml
[providers.local]
endpoint        = "http://localhost:8080/v1"
model           = "qwen3.6-35b-a3b"
context_window  = 32768
concurrency     = 1
api_key         = ""               # optional, sent as Bearer

[providers.local.sampler]
temperature      = 0.6
top_k            = 20
top_p            = 0.95
min_p            = 0.0
presence_penalty = 1.5
```

| Field            | Default                     | Description                                                                |
| ---------------- | --------------------------- | -------------------------------------------------------------------------- |
| `endpoint`       | required                    | OpenAI-compatible base URL (e.g. `http://localhost:8080/v1`).              |
| `model`          | required                    | Model id sent to the endpoint.                                             |
| `context_window` | 32768                       | Used for compaction triggers and the status-bar tokens display.            |
| `concurrency`    | 1                           | Max in-flight chat completions. Higher = parallel sub-agent calls.         |
| `api_key`        | `""`                        | Sent as `Authorization: Bearer <key>` if non-empty. Supports `$ENSO_FOO` / `${ENSO_FOO}` env-var indirection — see [Secrets]({{< relref "../docs/secrets.md" >}}). |
| `sampler.*`      | various                     | Sampler knobs. Sent in every completion request.                           |

## `[permissions]`

```toml
[permissions]
mode  = "prompt"                                # "prompt" | "allow" | "deny"
allow = []                                      # ["bash(git *)", ...]
ask   = []                                      # ["bash(git push *)", ...]
deny  = []                                      # ["bash(rm -rf *)", ...]
additional_directories = []                     # extra workspace dirs
disable_file_confinement = false                # see below
```

When `disable_file_confinement = false` (default), file-touching tools
(`read`, `write`, `edit`, `grep`, `glob`, and `lsp_*`) refuse any path
that resolves outside `cwd + additional_directories`. This is the
parallel guard that makes the bash sandbox meaningful — without it the
agent could exfiltrate via `read`. Set `true` only if you genuinely
want the model to roam the filesystem.

See [Permissions]({{< relref "../docs/permissions.md" >}}) for full
pattern syntax.

## `[web_fetch]`

```toml
[web_fetch]
allow_hosts = []                                # ["localhost:8080", "127.0.0.1:11434"]
```

The `web_fetch` tool refuses any URL that resolves to a loopback,
private, or link-local address by default — that blocks the agent
from probing instance metadata (`169.254.169.254`), your running dev
servers, or LAN hosts. Add entries to opt specific local services
back in.

Each entry matches the URL's `host` or `host:port` exactly
(case-insensitive on host). An entry without a port matches any port
for that host; with a port the port must match. The DNS-rebind
defence stays on regardless: the resolved IP is pinned for the actual
TCP dial.

## `[bash]` and `[bash.sandbox_options]`

```toml
[bash]
sandbox = "off"            # "off" | "auto" | "podman" | "docker"

[bash.sandbox_options]
image         = "alpine:latest"
init          = []                          # commands to run once after creation
network       = ""                          # "" inherits; "none" / "host" / named
extra_mounts  = []                          # ["src:dst[:opts]", ...]
env           = []                          # ["KEY=value", ...]
name          = ""                          # override auto-generated name
workdir_mount = "/work"                     # in-container path for cwd
uid           = ""                          # --user value (rarely needed)
```

See [Sandbox]({{< relref "../docs/sandbox.md" >}}).

## `[git]`

Commit-attribution trailers the model adds when writing commit
messages on your behalf.

```toml
[git]
attribution      = "none"          # "co-authored-by" | "assisted-by" | "none"
attribution_name = "enso"
```

When non-`none`, the system prompt instructs the model to append the
matching trailer (`Co-Authored-By: <name> <noreply@enso.local>` or
`Assisted-by: <name>`) to commit messages. Default is `none` — opt in.

## `[ui]`

```toml
[ui]
theme       = "dark"
editor_mode = "default"      # "default" | "vim"
status_line = ""             # text/template; empty = built-in default
```

`status_line` template variables: `.Provider .Model .Session .Mode
.Activity .Tokens .Window .TokensFmt .TokensPerSec`. The default
template is

```text
[{{.Provider}}] {{.Model}} · {{.Session}} · {{.TokensFmt}}{{if .TokensPerSec}} · {{.TokensPerSec}} t/s{{end}}
```

`{{.TokensPerSec}}` is non-zero only while a turn is actively
streaming, so `{{if .TokensPerSec}}…{{end}}` keeps the segment from
appearing when idle. Example custom template:

```toml
status_line = "{{.Mode}} | {{.Model}} | {{.TokensFmt}}"
```

See [TUI]({{< relref "../docs/tui.md" >}}) and themes (drop a
`~/.enso/theme.toml`).

## `[hooks]`

Shell commands run at well-known lifecycle moments. Empty strings
disable the corresponding event. `sh -c` invocation; 10s timeout;
templates use Go `text/template` syntax against per-event variables.
Timeouts and template errors surface as a yellow chat line and a
slog warning; non-zero exit codes from your command are silent.

```toml
[hooks]
on_file_edit   = "gofmt -w {{.Path}}"
on_session_end = "notify-send 'enso done'"
```

| Event            | Fires when                                                          | Template vars              |
| ---------------- | ------------------------------------------------------------------- | -------------------------- |
| `on_file_edit`   | After the `edit` or `write` tool succeeds; before the result returns to the agent. Synchronous, so subsequent reads in the same turn see the post-hook file. | `.Path`, `.Tool` (`"edit"` / `"write"`) |
| `on_session_end` | When the top-level `agent.Run` loop returns (any cause). Subagents and workflow roles do not fire this.                                                       | `.SessionID`, `.Cwd`        |

## `[mcp.<name>]`

```toml
[mcp.gitea]
command = "gitea-mcp-server"
args    = ["--token", "$TOKEN"]   # $VAR expansion at startup

[mcp.notion]
url     = "https://mcp.notion.com/v1"
headers = { Authorization = "Bearer $NOTION_TOKEN" }   # HTTP-only; $VAR expanded
```

See [MCP servers]({{< relref "../docs/mcp.md" >}}). `command` and
`url` are mutually exclusive; `headers` is HTTP-only.

## `[lsp.<name>]`

```toml
[lsp.go]
command      = "gopls"
extensions   = [".go"]
root_markers = ["go.mod", ".git"]
init_options = {}                   # opaque; passed verbatim
env          = []
language_id  = ""                   # defaults to <name>
```

See [LSP]({{< relref "../docs/lsp.md" >}}) for examples per language.

## State directories

| Path                                | Purpose                                                       |
| ----------------------------------- | ------------------------------------------------------------- |
| `~/.enso/enso.db`                   | SQLite session store.                                         |
| `~/.enso/enso.log`                  | slog text output (info+).                                     |
| `~/.enso/debug.log`                 | Raw SSE chunks and request bodies when `--debug`.             |
| `~/.enso/daemon.sock` / `daemon.pid`| Daemon socket and PID file (POSIX only).                      |
| `~/.enso/skills/`                   | User-defined slash commands.                                  |
| `~/.enso/agents/`                   | User-defined agent profiles.                                  |
| `~/.enso/workflows/`                | User-defined workflow pipelines.                              |
| `~/.enso/memory/`                   | User-global auto-memory files.                                |
| `~/.enso/worktrees/`                | Auto-created git worktrees from `--worktree`.                 |
| `~/.enso/theme.toml`                | TUI palette overrides.                                        |
| `<cwd>/.enso/config.toml`           | Project-committed config.                                     |
| `<cwd>/.enso/config.local.toml`     | Project-local config (gitignored).                            |
| `<cwd>/.enso/skills/`               | Project-scoped skills.                                        |
| `<cwd>/.enso/agents/`               | Project-scoped agents.                                        |
| `<cwd>/.enso/workflows/`            | Project-scoped workflows.                                     |
| `<cwd>/.enso/memory/`               | Project-scoped memories. The `memory_save` tool writes here.  |
| `<cwd>/.ensoignore`                 | First-class ignore file (gitignore-style).                    |
| `<cwd>/ENSO.md` and `<cwd>/AGENTS.md` | Per-project system-prompt extensions, walked up from cwd.   |
