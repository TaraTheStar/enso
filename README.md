<p align="center">
  <img src="docs/static/logo.png" alt="ensō" width="220">
</p>

# ensō

[![CI](https://github.com/TaraTheStar/enso/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/TaraTheStar/enso/actions/workflows/ci.yml)
[![License: AGPL v3+](https://img.shields.io/badge/License-AGPL_v3%2B-blue.svg)](LICENSE)

> ensō (円相) — the Zen circle, drawn in one breath. Imperfect by
> design; the irregularities are the point.

A small TUI agentic coding agent in Go (binary: `enso`). Talks to
any OpenAI-compatible chat endpoint (designed against `llama.cpp`'s
`llama-server` running Qwen3.6-35B-A3B, but the LLM client doesn't
care). Built-in tools: `read`, `write`, `edit` (with diff prompt),
`bash`, `grep`, `glob`, `web_fetch`, `web_search` (DuckDuckGo by default;
opt into SearXNG via `[search.searxng] endpoint = "..."`), `todo`,
`memory_save`. Sessions persist to SQLite and resume across crashes.

📖 **Full documentation:** <https://tarathestar.github.io/enso/>

(also available offline as Markdown in `docs/content/`).

## Quickstart

### 1. Start `llama-server`

The default config (written on first run to
`$XDG_CONFIG_HOME/enso/config.toml`, ≈ `~/.config/enso/config.toml`)
assumes a server on `http://localhost:8080/v1` serving model id
`qwen3.6-35b-a3b`. On a single RTX 3090:

```bash
llama-server \
  -m unsloth/Qwen3.6-35B-A3B-GGUF/UD-Q4_K_XL.gguf \
  -ngl 999 -c 32768 -fa on --no-mmap \
  -ctk q8_0 -ctv q8_0 \
  --jinja --reasoning-budget 4096 --reasoning-budget-message \
  --temp 0.6 --top-k 20 --top-p 0.95 --min-p 0.0 --presence-penalty 1.5 \
  --port 8080
```

Any other OpenAI-compatible endpoint will work; edit
`~/.config/enso/config.toml` to point elsewhere.

### 2. Build

```bash
make build           # produces ./bin/enso
# or
go build -o enso ./cmd/enso
```

Common Makefile targets: `make run` (builds + launches the TUI),
`make daemon`, `make test`, `make check` (gofmt + vet + test + build), and
`make help` for the full list.

### 3. Run

Interactive TUI:

```bash
./enso                  # or ./enso tui
./enso --yolo           # auto-allow all tool calls (no permission prompts)
./enso --session <id>   # resume a prior session (alias: --resume <id>)
./enso --continue       # resume the most recently updated session
```

Non-interactive (one-shot, streams to stdout):

```bash
./enso run --yolo "list the .go files in cmd/"
echo "summarise README.md" | ./enso run --yolo
./enso run --yolo --format json "..."   # newline-delimited bus events on stdout
```

Export a finished session to markdown:

```bash
./enso export <session-id>          # to stdout
./enso export <session-id> -o session.md
```

## Configuration

Config files are layered. From lowest to highest priority:

1. `/etc/enso/config.toml` (system)
2. `$XDG_CONFIG_HOME/enso/config.toml` (user; falls back to `~/.config/enso/config.toml`)
3. `<cwd>/.enso/config.toml` (project, committed)
4. `<cwd>/.enso/config.local.toml` (project, gitignored — "Remember" rules land here)
5. The path passed via `-c <file>` (highest)

Each file is merged key-by-key — later layers override individual keys
without replacing whole tables. If no config file exists anywhere, the
default is written to the user path on first run.

```bash
enso config init           # write the default to the user path
enso config init --print   # dump the default to stdout
enso config init --force   # overwrite an existing file
enso config show           # list the search paths and which exist
enso trust                 # trust ./.enso/config.toml (records sha256 in ~/.enso/trust.json)
enso trust --list          # show every trusted entry
enso trust --revoke        # forget the entry for ./.enso/config.toml
```

Key sections of the config:

```toml
# Top-level scalar — picks which [providers.X] is active at session
# start. Must appear before any [providers.X] table; if unset, the
# alphabetically-first provider name wins. Switch mid-session with
# /model <name>.
default_provider = "local"

[providers.local]
endpoint = "http://localhost:8080/v1"
model = "qwen3.6-35b-a3b"
context_window = 32768
concurrency = 1

[providers.local.sampler]
temperature = 0.6
top_k = 20
top_p = 0.95
min_p = 0.0
presence_penalty = 1.5

[permissions]
mode  = "prompt"   # "prompt" | "allow" | "deny"  (fallback for un-matched calls)
allow = []         # e.g. ["bash(git *)", "edit(./src/**)", "web_fetch(domain:example.com)"]
ask   = []         # e.g. ["bash(git push *)"] — always prompt even when otherwise allowed
deny  = []         # e.g. ["bash(rm -rf *)", "edit(./.env)"]
additional_directories = []   # extra workspace dirs beyond cwd; surfaced in
                              # the system prompt + the @-file picker
disable_file_confinement = false  # when false (default), file tools (read/
                              # write/edit/grep/glob/lsp_*) refuse any path
                              # outside cwd + additional_directories. Set
                              # true to let the model roam the filesystem.

[git]
attribution = "none"          # "co-authored-by" | "assisted-by" | "none"
attribution_name = "enso"

[ui]
theme       = "dark"
editor_mode = "default"       # "default" | "vim" — vim adds normal-mode hjkl, w/b, x, i/a/A/o/O, Enter to submit
status_line = ""              # text/template; replaces the right-side bar. Vars:
                              #   .Provider .Model .Session .Mode .Activity .Tokens .Window .TokensFmt .TokensPerSec
                              # Example: "{{.Mode}} | {{.Model}} | {{.TokensFmt}}"

[hooks]
on_file_edit   = ""           # shell command run after edit/write succeeds; vars: .Path .Tool
on_session_end = ""           # shell command run when the agent loop returns; vars: .SessionID .Cwd

[web_fetch]
allow_hosts = []              # opt local hosts back through the SSRF guard
                              # (web_fetch refuses loopback / private / link-local IPs by default).
                              # Entries are exact host or host:port matches; a host without a port
                              # matches any port. Example: ["localhost:8080", "127.0.0.1:11434"].

[search]
provider = ""                 # "" (auto) | "searxng" | "duckduckgo" | "none".
                              # Auto: SearXNG when [search.searxng] endpoint is set, DDG otherwise.
                              # "none" suppresses the web_search tool entirely.

[search.searxng]
endpoint    = ""              # e.g. "http://localhost:8888" or "https://searx.be"
categories  = []              # ["general", "it", ...] — empty leaves SearXNG default
engines     = []              # ["google", "duckduckgo", ...] — empty leaves SearXNG default
max_results = 10              # ceiling; the model can ask for fewer
api_key     = ""              # optional — sent as Authorization: Bearer; "$ENSO_*" refs expanded
timeout     = 15              # seconds
```

Patterns are `tool(arg-pattern)`. Per-tool matching:

- `bash(git *)` — first-word match for single tokens, full-glob for multi-word (`bash(git push *)` properly scopes to `git push`). **Allow rules gate on shell metacharacters**: any of `;` `&` `|` `<` `>` `$` `` ` `` `(` `)` `\` newline present in the command must also appear in the pattern, so `bash(git *)` will NOT auto-allow `git status; rm -rf ~`. Opt in explicitly with patterns like `bash(git * | *)` if you need pipes. **Deny rules are segment-aware**: `bash(rm -rf *)` blocks chained variants like `do_evil; rm -rf /`, `cd / && rm -rf *`, and `ls | rm -rf *` by splitting on top-level shell separators. *Deny rules are guardrails, not walls* — they don't recurse into command substitution (`$(...)`, backticks) or `eval`. For real isolation against a hostile model or hostile codebase, set `bash.sandbox = "auto"`.
- `read(**)` / `write(./src/**)` / `edit(./.env)` / `grep(...)` / `glob(...)` — strict doublestar path globs against the absolute path. **No basename fallback**: `read(*.md)` does NOT match `/anywhere/foo.md` (that would let a bare extension pattern exfiltrate any file with that suffix). Use `read(**/*.md)` for "any .md file" or `read(./**/*.md)` to scope it to the project.
- `web_fetch(domain:example.com)` — match by URL host (subdomains included).
- Anything else — generic doublestar match.

Precedence: `deny` → `ask` → `allow` → `mode` default. **Ask wins over allow**, so a broad `bash(*)` allow plus an `ask` on `bash(git push *)` still prompts before pushing.

Drop a `.ensoignore` at the project root with one glob per line (gitignore-style, no `!` negation) to auto-deny `read`/`write`/`edit`/`grep`/`glob` for those paths and hide them from the @-picker. Lines starting with `#` are comments.

When `[git] attribution` is non-`none`, the system prompt instructs the
model to append a matching trailer (`Co-Authored-By: <name>` /
`Assisted-by: <name>`) to any commit message it writes on your behalf.

## Secrets

Provider `api_key` and MCP `headers` / `args` accept `$ENSO_FOO` /
`${ENSO_FOO}` env-var references. **Only `ENSO_`-prefixed names
expand**; anything else collapses to `""` (logged once). Keep a
committable config that references the names, set the values from
your shell:

```toml
# ~/.config/enso/config.toml — committable; no actual secret here.
[providers.cloud]
endpoint = "https://api.example.com/v1"
model    = "some-model"
api_key  = "$ENSO_CLOUD_KEY"
```

```bash
export ENSO_CLOUD_KEY="sk-..."
```

The prefix gate exists because trusting a project once shouldn't
let a later commit ship `api_key = "$AWS_SECRET_ACCESS_KEY"` to a
hostile endpoint. Per-repo secrets follow the same pattern: commit
`.enso/config.toml` with `$ENSO_*` refs, set the values via
`direnv` or your shell. See
[docs/secrets](https://tarathestar.github.io/enso/docs/secrets/)
for the full story.

## Project instructions (`ENSO.md` / `AGENTS.md`)

The system prompt is built from three tiers:

1. The default prompt embedded in the binary.
2. `~/.enso/ENSO.md` (if present) — replaces the default.
3. The closest `ENSO.md` walking up from the cwd — appended.
4. The closest `AGENTS.md` walking up from the cwd — appended.

## TUI keybindings

| Key | Action |
|---|---|
| Enter | Submit |
| Shift-Enter / Alt-Enter | Newline |
| Ctrl-C | Cancel current turn (idle = no-op) |
| Ctrl-D | Quit |
| Ctrl-A | Toggle agents pane |
| Ctrl-T | Toggle visibility of completed thinking blocks |
| Ctrl-R | Open recent-sessions overlay (Enter switches session — re-execs with `--session <id>`) |
| `@` (at token start) | Open file picker — type to filter, Enter inserts the path |
| Esc | Close modal (= Deny on permission prompt) |

## Slash commands

| Command | Description |
|---|---|
| `/help` | List available commands |
| `/yolo on\|off` | Toggle auto-allow mode |
| `/tools` | List registered tools |
| `/sessions` | List recent sessions (resume with `--session <id>`) |
| `/grep <pattern>` | Run a one-shot grep against the project |
| `/permissions` | List & remove project-local permission rules |
| `/model [<name>]` | List configured providers, or switch the active one |
| `/compact` | Force a context-compaction pass |
| `/init [target]` | Survey the project and write `ENSO.md` (or a chosen filename) |
| `/agents` | List declarative agent profiles |
| `/loop <interval> <prompt>` | Re-submit a prompt every interval (≥5s); `/loop off` stops |
| `/workflow <name>` | Run a declarative workflow |
| `/quit` | Exit |

## Sessions

Sessions live in `~/.enso/enso.db` (SQLite, pure-Go via `modernc.org/sqlite`).
Every user message, assistant reply, and tool result is persisted before the UI
sees it — kill the process mid-tool-call and the session resumes with the
interrupted call surfaced as a synthetic tool result.

Use `--ephemeral` to skip persistence.

## Status

All v1 phases are implemented:

- Interactive TUI chat with streaming and tool-calling.
- Sessions + crash resume + auto-compaction at 60% of the context window.
- Permission allowlist with prompt-on-miss; `--yolo` for unattended runs.
- `enso run` non-interactive mode (with `--detach` to submit to a daemon, `--format json` for streaming structured events).
- `enso export <id>` to dump a session as markdown.
- `enso stats [--days N]` for token / message / tool aggregates across sessions.
- `enso fork <id>` to branch an existing session into a fresh one.
- `--continue` / `--resume <id>` for picking up where you left off.
- `--worktree` to spin up a fresh git worktree (`~/.enso/worktrees/<repo>-<rand>` on `enso/<rand>`) and run the session there.
- `--agent <name>` to pick a declarative profile (built-in `plan`, plus user / project agents).
- Multiple `[providers.X]` blocks; `default_provider = "..."` picks the active one and `/model <name>` swaps it mid-session.
- `[lsp.<name>]` config to surface `lsp_hover`/`lsp_definition`/`lsp_references`/`lsp_diagnostics` tools (any language server).
- `[git]` config block to opt into commit attribution trailers.
- `spawn_agent` tool for subagents (depth ≤3, global cap 16).
- MCP client (stdio + Streamable-HTTP) auto-registers remote tools.
- Slash commands: `/help`, `/yolo`, `/tools`, `/sessions`, `/grep`, `/permissions`, `/model`, `/compact`, `/init`, `/agents`, `/loop`, `/workflow`, `/quit`, plus user-defined skills.
- Declarative workflows (planner→coder→reviewer style).
- `enso daemon` + `enso attach` for long-running detached sessions.

**Subagents** — `spawn_agent` tool. Depth ≤3, global cap 16; child shares
parent's provider/bus/permissions. Toggle the right-side agents pane with
Ctrl-A.

**MCP** — servers (stdio or Streamable-HTTP) are configured under
`[mcp.<name>]` in `config.toml`. Their tools surface as `mcp__<server>__<tool>`
in `/tools` and the permission matcher.

**Bash sandboxing** — `[bash] sandbox = "auto"` (or `"podman"` /
`"docker"`) routes the `bash` tool through a per-project container.
Project cwd is bind-mounted at `/work`; the agent's shell can't see
`~`, `~/.ssh`, sibling repos, or anything else outside cwd. File
tools (`read`/`write`/`edit`/`grep`/`glob`) get a parallel
cwd-confinement guard so they can't bypass the sandbox via path
arguments. The container is named per-project (`enso-<basename>-<6hex>`)
and persists across ensō runs — first start pays the image-pull +
init cost, subsequent runs `podman start` instantly. Manage with
`enso sandbox list / stop / rm / prune`.

```toml
[bash]
sandbox = "auto"            # "off" | "auto" | "podman" | "docker"

[bash.sandbox_options]
image = "alpine:latest"
init  = ["apk add --no-cache git curl jq make"]
network = ""                # "" inherits runtime default; "none" = offline
extra_mounts = ["~/.cache/go-build:/root/.cache/go-build:rw"]
```

The init list re-runs only when this config (image / init / mounts /
env) changes — tracked via a label on the container. Manual edits to
the container's contents (e.g. `apk add` from inside) survive across
enso runs but are lost when the config changes.

**LSP** — configure language servers under `[lsp.<name>]` to surface
`lsp_hover`, `lsp_definition`, `lsp_references`, and `lsp_diagnostics`
tools. Servers are spawned lazily on first use, scoped by file
extension, and reused for the rest of the session. Works with any
LSP-compliant server (gopls, rust-analyzer, typescript-language-server,
pyright, clangd, ruby-lsp, …) — the config is fully language-agnostic.
See `enso config init --print` for example blocks. Daemon-mode sessions
do not currently expose these tools.

```toml
[lsp.go]
command = "gopls"
extensions = [".go"]
root_markers = ["go.mod", ".git"]

[lsp.typescript]
command = "typescript-language-server"
args = ["--stdio"]
extensions = [".ts", ".tsx", ".js", ".jsx"]
root_markers = ["package.json", "tsconfig.json", ".git"]
init_options = { preferences = { quotePreference = "single" } }
```

This repo ships its own `<repo>/.enso/config.toml` with `[lsp.gopls]`
pre-wired, so contributors get definition/references/hover for free —
provided `gopls` is on `PATH`. If it isn't:

```bash
go install golang.org/x/tools/gopls@latest
```

The first launch in any repo with a committed `.enso/config.toml`
prompts to trust the file (one-time, recorded in `~/.enso/trust.json`).

**Auto-memory** — call the `memory_save` tool to persist a fact across
sessions. Files land at `<cwd>/.enso/memory/<slug>.md` (project) and are
auto-loaded into the system prompt at the start of every future session.
User-global memories at `~/.enso/memory/<slug>.md` work the same way;
project files shadow user files on name collision. Save things that are
*non-obvious and stable* — preferences, project facts, prior corrections
("don't mock the database in integration tests"), not in-progress work
or anything already in code/git history. Inspect with
`ls ~/.enso/memory/` or `ls .enso/memory/`; delete with `rm`.

**Agents** — declarative profiles select a different system-prompt
appendix, tool restriction, and sampler for the session. Built-in:
`plan` (read-only investigation; `bash`/`write`/`edit` removed). Drop a
frontmatter-headed `~/.enso/agents/<name>.md` or
`./.enso/agents/<name>.md` to add your own; project shadows user, user
shadows built-in. Frontmatter fields: `name`, `description`,
`allowed-tools`, `denied-tools`, `temperature`, `top_p`, `top_k`,
`max_turns`. The body is the prompt appended to the base system prompt.
Pick at startup with `--agent <name>`; list available agents in the TUI
with `/agents`. (Mid-session switching is not yet supported. Per-agent
`model:` is also not yet wired — pick a different provider per session
with `/model`, per workflow-role with the role's `model:` field, or
per `spawn_agent` call with the tool's `model` arg.)

**Skills** — drop a frontmatter-headed markdown file at `~/.enso/skills/<name>.md`
or `./.enso/skills/<name>.md` and `/<name>` becomes a slash command that
expands the body as the next user message. Frontmatter fields: `name`,
`description`, `allowed-tools`, `model`. Body is a `text/template` with
`{{ .Args }}`.

**Workflows** — declarative agent pipelines in
`~/.enso/workflows/<name>.md` (or project-local `./.enso/workflows/<name>.md`).
Frontmatter declares roles + edges; the body has one `## <role>` section per
agent with a `text/template` prompt. Run via `/workflow <name> <args>` in the
TUI or `enso run --workflow <name> "<args>"` from the CLI. See
`examples/workflows/build-feature.md` for a planner→coder→reviewer pipeline.

**Theme** — drop a `~/.enso/theme.toml` to override the default colour
palette. Each entry is a hex `#rrggbb`, mapped onto tcell's named colours
which all the chat / modal / overlay code uses (`[yellow]`, `[teal]`,
`[gray]`, `[red]`, `[green]`, plus `white` / `black` for high-contrast
modal buttons):

```toml
[colors]
yellow = "#ffd866"
teal   = "#78dce8"
gray   = "#727072"
red    = "#ff6188"
green  = "#a9dc76"
```

A typo in this file logs a warning to `~/.enso/enso.log` and falls back
to defaults; it never blocks the TUI.

**Daemon mode** — POSIX-only (Linux/macOS/BSD; Windows users run via WSL).
The daemon path is intentionally narrower than the in-process path:
**`lsp_*` tools and the `[bash] sandbox` are not registered for daemon
sessions** — each `enso run --detach` can target a different cwd, but
the registry is shared across sessions, and per-session LSP / sandbox
managers are out of v1 scope. Use `enso run` or `enso tui` (in-process)
when you need those tools.

`enso daemon` runs a long-lived agent server on a unix
socket at `~/.enso/daemon.sock`. Pass `--detach` to fork into the
background and return immediately (the parent prints the child PID and
the socket path; running `--detach` again while a daemon is up just says
"daemon already running"). `enso run --detach "<prompt>"` submits a
fire-and-forget job (yolo by default — no UI to prompt) and prints the
session id. `enso attach <id>` opens a TUI driven by the live event stream
from the daemon; permission prompts proxy back through the socket so you
can answer them locally. If no client is attached the daemon denies after
a 60s timeout. Attach reconnects automatically on daemon restart — the
events cursor is preserved via `from_seq` so any events still in the
ring buffer replay.

See `AGENTS.md` for the maintainer's reference (operating conventions,
non-goals, soak-test risks).

## License

ensō is licensed under the GNU Affero General Public License v3.0 or
later (`AGPL-3.0-or-later`). The full text of v3 is in
[`LICENSE`](LICENSE); "or later" means you may also redistribute under
any later version published by the Free Software Foundation.
