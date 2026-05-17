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
description     = "fast MoE, big context"   # optional capability hint
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
| `description`    | `""`                        | Short capability hint. When ≥2 providers are configured it's rendered into the auto "## Available models" prompt section so the model can route across endpoints (see `[instructions]`). |
| `context_window` | 32768                       | Used for compaction triggers and the status-bar tokens display.            |
| `concurrency`    | 1                           | Max in-flight chat completions when this provider is alone in its pool. Ignored once it shares a pool — set `[pools.<name>].concurrency` instead. |
| `pool`           | auto (by endpoint)          | Pool this provider belongs to. Unset = auto-grouped with every provider sharing its `endpoint`. See `[pools.<name>]`. |
| `api_key`        | `""`                        | Sent as `Authorization: Bearer <key>` if non-empty. Supports `$ENSO_FOO` / `${ENSO_FOO}` env-var indirection — see [Secrets]({{< relref "../docs/secrets.md" >}}). |
| `sampler.*`      | various                     | Sampler knobs. Sent in every completion request.                           |

## `[instructions]`

Tunes how the system prompt is assembled.

```toml
[instructions]
include_providers = true   # default; set false to suppress the section
```

When **two or more** `[providers.*]` are configured, ensō injects an
auto-rendered `## Available models` section into the system prompt
(between the embedded default and your `ENSO.md`). It names the model
the agent is running as and lists the others with their `description`,
pool membership and `swap-cost`, so the model can delegate via
`spawn_agent`'s `model:` arg (and avoid expensive same-pool swaps) or
you can `/model <name>`. When two providers share a pool the section
adds a one-line note that switching between pool-mates is costly. A
`replace: true` ENSO.md discards it along with the
default (see [prompt layering]({{< relref "../docs/prompt-layering.md" >}})).

The section is **static for the session** — a mid-session `/model`
swap does not rewrite the "running as" line; the provider list itself
never changes. Single-provider configs never see the section. Set
`include_providers = false` to suppress it everywhere (including
sub-agents) — useful if the ~80 tokens/turn matters to you.

| Field               | Default | Description                                                       |
| ------------------- | ------- | ----------------------------------------------------------------- |
| `include_providers` | `true`  | Auto "## Available models" section. `false` suppresses it entirely. Self-suppresses below 2 providers regardless. |

## `[pools.<name>]`

A pool bounds concurrency across **every provider assigned to it** —
the shared-hardware unit (e.g. one llama-swap behind one GPU). By
default providers are grouped by `endpoint` (one endpoint = one pool,
zero config), so parallel calls to two models on the same llama-swap
serialise instead of thrashing the GPU. Override grouping with a
per-provider `pool = "<name>"`.

```toml
[providers.qwen-fast]
endpoint = "http://latchkey:4000/v1"
model    = "qwen3.6-27b"
pool     = "latchkey-3090"   # explicit; otherwise auto-<host>-<port>

[providers.qwen-deep]
endpoint = "http://latchkey:4000/v1"
model    = "minimax-m2.7"
pool     = "latchkey-3090"   # shares one semaphore with qwen-fast

[pools.latchkey-3090]
concurrency   = 1            # one in-flight request across ALL members
queue_timeout = "300s"       # wait this long for a slot, then error
swap_cost     = "high"       # hint (rendered into the model prompt later)
```

| Field           | Default | Description                                                                 |
| --------------- | ------- | --------------------------------------------------------------------------- |
| `concurrency`   | 1       | Max in-flight requests across all members. A lone auto pool instead inherits its single provider's `concurrency`. |
| `queue_timeout` | `300s`  | Go duration. A request blocked on a full pool errors after this; invalid/unset → 300s. Ctrl-C cancels sooner. |
| `swap_cost`     | `""`    | Hint (`low`/`high`/…) shown next to the pool in the model's "## Available models" section, so it avoids costly same-pool model swaps. |
| `rpm` / `tpm` / `daily_budget` | unset | **Reserved** for cloud rate-limit-aware scheduling. Parsed but not enforced; setting one logs a one-time warning. |

Waiters are granted slots in FIFO arrival order. Coordination scope:
**every session hosted by one `enso daemon` shares its pools** — all
detached/attached sessions and their sub-agents contend on the same
semaphores, because the daemon runs every agent loop in-process. Within
a single standalone `enso` process the agent and its sub-agents also
share pools. The remaining gap: two *separate* standalone processes
(no daemon) on the same host don't coordinate and can thrash shared
hardware — run `enso daemon` and attach if you need cross-process
coordination.

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

## `[search]` and `[search.searxng]`

```toml
[search]
provider = ""                  # "" (auto) | "searxng" | "duckduckgo" | "none"

[search.searxng]
endpoint    = ""               # "http://localhost:8888" or "https://searx.be"
categories  = []               # ["general", "it", ...]
engines     = []               # ["google", "duckduckgo", ...]
max_results = 10               # ceiling; the model can ask for fewer
api_key     = ""               # optional — sent as Authorization: Bearer
timeout     = 15               # seconds
ca_cert     = ""               # path to PEM bundle to trust (self-hosted CA)
insecure_skip_verify = false   # disable TLS verification — last-resort escape hatch
```

The `web_search` tool is registered by default. With no configuration it
scrapes DuckDuckGo's HTML endpoint (`html.duckduckgo.com/html/`) — no
signup, works anywhere with internet, but no engine attribution and no
publishedDate. Point `[search.searxng] endpoint` at a self-hosted (or
public) SearXNG instance for higher-quality, multi-engine results with
attribution.

`provider` selects the backend explicitly:

- `""` (unset) — auto: SearXNG when `endpoint` is set, DuckDuckGo otherwise.
- `"searxng"` — force SearXNG; if `endpoint` is empty, logs a warning and falls back to DuckDuckGo.
- `"duckduckgo"` / `"ddg"` — force the DuckDuckGo fallback.
- `"none"` / `"off"` / `"disabled"` — suppress `web_search` entirely.

`api_key` accepts `$ENSO_*` env-var references (same gated expansion as
`providers.*.api_key`). Only ENSO\_-prefixed names expand; anything else
collapses to empty.

For self-hosted SearXNG behind a private CA, set `ca_cert` to a PEM
bundle. It's *appended* to the system roots, so public CAs still
verify normally. If `ca_cert` fails to load (bad path, no PEM blocks)
the backend logs once to stderr and falls back to default trust — it
won't crash startup. `insecure_skip_verify = true` disables TLS
verification entirely; reach for it only when you can't get the CA on
disk, and prefer `ca_cert` for anything long-lived.

Permission patterns match against the query string:

```toml
[permissions]
allow = ["web_search(*)"]              # any query
ask   = ["web_search(* exploit *)"]    # prompt for queries containing "exploit"
```

## `[backend]`, `[lima]`, and `[bash.sandbox_options]`

```toml
[backend]
type    = "local"          # "local" (default) | "podman" | "lima"
runtime = "auto"           # type=podman only: "auto" | "podman" | "docker"

[lima]                      # type = "lima" only; all optional
template     = "default"   # Lima template name, or a path/URL
cpus         = 4
memory       = "4GiB"
disk         = "20GiB"
extra_mounts = []           # extra host paths, mounted read-only

[bash.sandbox_options]      # type = "podman" only
image         = "alpine:latest"
init          = []                          # commands to run once after creation
network       = ""                          # "" inherits; "none" / "host" / named
extra_mounts  = []                          # ["src:dst[:opts]", ...]
env           = []                          # ["KEY=value", ...]
name          = ""                          # override auto-generated name
uid           = ""                          # --user value (rarely needed)
workspace     = ""                          # "overlay" = throwaway copy + resolve
hardening     = ""                          # "gvisor" / "runsc"
```

`[backend] type` is the sole backend selector. `[bash] sandbox` is
**removed** (breaking) — delete it from old configs; the key is now
silently ignored. See the CHANGELOG and
[Sandbox]({{< relref "../docs/sandbox.md" >}}).

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
`~/.config/enso/theme.toml`).

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

ensō follows the XDG Base Directory layout. Each helper honours the
matching `XDG_*` env var first and falls back to the path shown.

| Path                                                 | Purpose                                                       |
| ---------------------------------------------------- | ------------------------------------------------------------- |
| `~/.config/enso/config.toml`                         | User config (see search-path order above).                    |
| `~/.config/enso/ENSO.md`                             | User-wide system-prompt layer (appended; `replace: true` to override). |
| `~/.config/enso/theme.toml`                          | TUI palette overrides.                                        |
| `~/.config/enso/skills/`                             | User-defined slash commands.                                  |
| `~/.config/enso/agents/`                             | User-defined agent profiles.                                  |
| `~/.config/enso/workflows/`                          | User-defined workflow pipelines.                              |
| `~/.local/share/enso/enso.db`                        | SQLite session store.                                         |
| `~/.local/share/enso/memory/`                        | User-global auto-memory files.                                |
| `~/.local/state/enso/enso.log`                       | slog text output (info+).                                     |
| `~/.local/state/enso/debug.log`                      | Raw SSE chunks and request bodies when `--debug`.             |
| `~/.local/state/enso/trust.json`                     | Trust-store hashes for project `.enso/config.toml`.           |
| `~/.local/state/enso/worktrees/`                     | Auto-created git worktrees from `--worktree`.                 |
| `$XDG_RUNTIME_DIR/enso/daemon.sock` / `daemon.pid`   | Daemon socket and PID file (POSIX only).                      |
| `<cwd>/.enso/config.toml`                            | Project-committed config.                                     |
| `<cwd>/.enso/config.local.toml`                      | Project-local config (gitignored).                            |
| `<cwd>/.enso/skills/`                                | Project-scoped skills.                                        |
| `<cwd>/.enso/agents/`                                | Project-scoped agents.                                        |
| `<cwd>/.enso/workflows/`                             | Project-scoped workflows.                                     |
| `<cwd>/.enso/memory/`                                | Project-scoped memories. The `memory_save` tool writes here.  |
| `<cwd>/.ensoignore`                                  | First-class ignore file (gitignore-style).                    |
| `<cwd>/ENSO.md` and `<cwd>/AGENTS.md`                | Per-project system-prompt extensions, walked up from cwd.     |
