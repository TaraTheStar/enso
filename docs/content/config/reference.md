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
| `type`           | `"openai"`                  | Vendor adapter. `"openai"` (default) covers any OpenAI-compatible endpoint — llama.cpp, vLLM, Ollama, Groq, OpenAI proper, OpenRouter, Together, Fireworks. `"bedrock"` routes through AWS Bedrock's Converse API (multi-vendor: Claude / Nova / Llama / Mistral / Cohere / AI21). `"vertex"` routes through GCP Vertex AI's generateContent API (Gemini family). `"anthropic"`, `"anthropic-bedrock"`, `"anthropic-vertex"` are **opt-in** paths through the Anthropic Messages API directly — pick these when you need features Converse/generateContent don't model (prompt-caching control, computer-use, server tools). See [Anthropic-native paths](#anthropic-native-paths-opt-in). |
| `endpoint`       | required (openai)           | OpenAI-compatible base URL (e.g. `http://localhost:8080/v1`). Not used by `type = "bedrock"` or `type = "vertex"` — the AWS/GCP SDKs pick the regional URL. |
| `model`          | required                    | Model id sent to the endpoint. For Bedrock, this is the Bedrock model id (`anthropic.claude-3-5-sonnet-20241022-v2:0`, `amazon.nova-pro-v1:0`) or an inference-profile ARN — distinct from `api.anthropic.com` names. For Vertex, this is the Gemini model id (`gemini-2.5-pro`, `gemini-2.5-flash`). |
| `description`    | `""`                        | Short capability hint. When ≥2 providers are configured it's rendered into the auto "## Available models" prompt section so the model can route across endpoints (see `[instructions]`). |
| `context_window` | 32768                       | Used for compaction triggers and the status-bar tokens display. Set it to the model's real limit so auto-compaction engages in time (when unset, compaction can't size itself). If a request still overflows — a wrong value, or a proxy like litellm that hides the real limit — enso parses the server's "exceeds the available context size (N tokens)" rejection, adopts N as the effective window, compacts, and retries, so it self-corrects after the first overflow. |
| `concurrency`    | 1                           | Max in-flight chat completions when this provider is alone in its pool. Ignored once it shares a pool — set `[pools.<name>].concurrency` instead. |
| `pool`           | auto (by endpoint)          | Pool this provider belongs to. Unset = auto-grouped with every provider sharing its `endpoint`. See `[pools.<name>]`. |
| `api_key`        | `""`                        | Sent as `Authorization: Bearer <key>` if non-empty. Supports `$ENSO_FOO` / `${ENSO_FOO}` env-var indirection — see [Secrets]({{< relref "../docs/secrets.md" >}}). Not used by `type = "bedrock"` or `type = "vertex"`. |
| `max_tokens`     | `0`                         | Caps response length. On the OpenAI/llama.cpp path a `0` (unset) value derives a runaway backstop of `min(16384, context_window/2)` so a model that stops emitting EOS can't run to the context ceiling; set a positive value to cap explicitly. Bedrock applies a default of 4096 when zero; Vertex applies 8192. |
| `prompt_caching` | `false`                     | Opts into vendor-side prompt caching. On Anthropic + `anthropic-bedrock` + `anthropic-vertex`, inserts `cache_control:ephemeral` markers on the last system block and the last tool — system + tool definitions become a single cacheable prefix; subsequent turns that reuse the prefix hit the cache. On Bedrock Converse, equivalent via `CachePoint` blocks. OpenAI and Vertex Gemini cache implicitly — flag is a no-op for them but accepted so configs stay symmetric. Cache writes are billed 1.25× input on Anthropic; cache reads at 0.1×. Break-even after roughly two reuses of the same system/tool prefix. No-op on local providers. |
| `sampler.*`      | various                     | Sampler knobs. Sent in every completion request.                           |
| `generation.*`   | (see below)                 | Generation guards + auto-recovery for the OpenAI/llama.cpp path. See [Generation guards](#generation-guards). |

#### Generation guards

Three hardware-independent guards keep a local model from wedging a turn,
plus turn-level auto-recovery. They apply only to the OpenAI-compatible
path; the hosted adapters (`bedrock`/`vertex`/`anthropic*`) ignore the
block. Pair them with `max_tokens` (above), which is the hard backstop
against a model that never emits EOS.

```toml
[providers.local.generation]
stall_timeout        = "60s"   # abort a stream that emits no token for this long; "0s" disables
loop_guard           = true    # detect mid-stream degeneration loops and abort early
auto_recover         = true    # on length-truncation / tripped loop guard / stall, retry the turn with a nudge
max_recover_attempts = 2       # cap auto-recovery retries per turn
```

| Field                  | Default | Description |
| ---------------------- | ------- | ----------- |
| `stall_timeout`        | `"60s"` | Aborts a stream that produces **no token** for the window — it fires on silence, not slowness, so prompt-processing pauses and speculative/MTP bursts are tolerated. `"0s"` disables the watchdog. |
| `loop_guard`           | `true`  | Detects mid-stream degeneration loops — a short unit repeated back to back ("the the the", a duplicated line, a JSON fragment) — and aborts before the stream reaches the `max_tokens` cap. Cheap, rate-independent, tuned to ignore legitimately repetitive code. |
| `auto_recover`         | `true`  | On a length-truncation, a tripped loop guard, or a stall, retries the turn with a nudge instead of dropping it. |
| `max_recover_attempts` | `2`     | Upper bound on auto-recovery retries within a single turn. |

#### Bedrock-only fields (`type = "bedrock"`)

| Field                       | Default       | Description                                                  |
| --------------------------- | ------------- | ------------------------------------------------------------ |
| `aws_region`                | SDK default   | Bedrock region (`us-east-1`, `eu-west-1`, …). Empty falls back to the AWS chain (`AWS_REGION` env / shared config / instance metadata). |
| `aws_profile`               | `""`          | Named profile from `~/.aws/credentials`. Empty uses the default chain. |
| `extended_thinking`         | `false`       | Claude-only. Routes the model's reasoning through the same channel the TUI already renders for OpenAI reasoning models (ephemeral — not persisted in assistant message history). Pairing with non-Claude Bedrock models gets rejected by the API. |
| `extended_thinking_budget`  | `4096`        | Thinking-token budget. Silently clamped to `[1024, max_tokens)`. Below 1024 → 1024; at-or-above `max_tokens` → `max_tokens - 1`. |
| `bedrock_guardrail_id`      | `""`          | ID of an Amazon Bedrock Guardrail to evaluate every request and response against. Empty disables guardrails. The same key also works on `type = "anthropic-bedrock"`. |
| `bedrock_guardrail_version` | `""`          | Required when `bedrock_guardrail_id` is set. `"DRAFT"` or the numeric version (e.g. `"1"`). |
| `bedrock_guardrail_trace`   | `"enabled"`   | Trace level: `"enabled"`, `"disabled"`, or `"enabled_full"` (Converse only — on `anthropic-bedrock` it collapses to `"enabled"` since the InvokeModel `X-Amzn-Bedrock-Trace` header is binary). Validation fails loud at translate-time on a typo. |

Authentication follows the standard AWS credential chain: environment
variables, shared config (`~/.aws/credentials`), EC2/ECS/EKS instance
role. No keys land in ensō's config file.

Worked example:

```toml
[providers.bedrock-claude]
type       = "bedrock"
model      = "anthropic.claude-3-5-sonnet-20241022-v2:0"
aws_region = "us-east-1"
extended_thinking        = true
extended_thinking_budget = 8000

[providers.bedrock-nova]
type       = "bedrock"
model      = "amazon.nova-pro-v1:0"
aws_region = "us-east-1"
```

Both blocks share the one adapter; `model` picks the vendor.

#### Vertex-only fields (`type = "vertex"`)

| Field                       | Default       | Description                                                  |
| --------------------------- | ------------- | ------------------------------------------------------------ |
| `gcp_project`               | `$GOOGLE_CLOUD_PROJECT` | GCP project ID Vertex routes through. Empty falls back to the `GOOGLE_CLOUD_PROJECT` env var; if both are empty the SDK errors on first use. |
| `gcp_location`              | `us-central1` | Vertex region (`us-central1`, `europe-west4`, …). `us-central1` hosts every Gemini variant. |
| `extended_thinking`         | `false`       | Gemini 2.5+. Enables `IncludeThoughts` so the model returns Thought parts; ensō routes them to the same channel the TUI already renders for OpenAI reasoning models. Ephemeral — not persisted in assistant message history. Older Gemini variants silently ignore this. |
| `extended_thinking_budget`  | `0` (dynamic) | Thinking-token cap. `0` leaves Gemini's dynamic-thinking mode in effect; positive values pin a budget. Unlike Anthropic on Bedrock, ensō does NOT clamp temperature or top_p — Gemini has no such constraints. |
| `vertex_safety.*`           | Gemini default | Sub-table mapping per-category `HarmBlockThreshold` values. Categories: `hate_speech`, `harassment`, `dangerous_content`, `sexually_explicit`, `civic_integrity`. Values: `BLOCK_NONE` / `BLOCK_LOW_AND_ABOVE` / `BLOCK_MEDIUM_AND_ABOVE` / `BLOCK_ONLY_HIGH` / `OFF`. Both case-insensitive. Unknown category or threshold fails loud at translate-time. |

Authentication follows Google Application Default Credentials:
`GOOGLE_APPLICATION_CREDENTIALS` env var pointing at a service-account
JSON, `gcloud auth application-default login` on a workstation, or the
GCE / GKE / Cloud Run metadata server in deployed environments. No
keys land in ensō's config file.

Anthropic Claude on Vertex AI uses a different `:rawPredict` shape and
is **not** covered by this adapter — track its arrival under the parked
anthropic adapters.

Worked example:

```toml
[providers.vertex-gemini]
type         = "vertex"
model        = "gemini-2.5-pro"
gcp_project  = "my-gcp-project"
gcp_location = "us-central1"
extended_thinking        = true
extended_thinking_budget = 0           # leaves Gemini's dynamic mode in effect

[providers.vertex-flash]
type         = "vertex"
model        = "gemini-2.5-flash"
gcp_project  = "my-gcp-project"
gcp_location = "us-central1"

# Per-category safety overrides on the Pro provider above. Empty
# leaves Gemini's defaults in effect — set this sub-table only when
# you need to relax or tighten specific categories.
[providers.vertex-gemini.vertex_safety]
hate_speech       = "BLOCK_NONE"
harassment        = "BLOCK_MEDIUM_AND_ABOVE"
dangerous_content = "BLOCK_ONLY_HIGH"
```

Per-request safety settings only apply to `type = "vertex"` (Gemini).
`type = "anthropic-vertex"` reaches Claude through `:rawPredict`,
which doesn't model a per-request safety knob — Vertex applies its
platform-level Model Armor / safety policy at the infrastructure
layer instead.

#### Anthropic-native paths (opt-in)

Three opt-in types route through the Anthropic Messages API rather than
the lowest-common-denominator Converse / generateContent shapes. They
all share one translator (`buildAnthropicParams`), one streaming loop
(`streamAnthropic`), and one `extended_thinking` semantics — only the
transport and auth differ. Pick these only when you need a Claude
feature that doesn't model into Converse or generateContent.

| `type`                 | Talks to                          | Auth                                     | Model id shape                                  |
| ---------------------- | --------------------------------- | ---------------------------------------- | ----------------------------------------------- |
| `"anthropic"`          | `api.anthropic.com` direct        | `api_key` (literal or `$ENSO_*` env ref) | `claude-sonnet-4-5`, `claude-haiku-4-5`         |
| `"anthropic-bedrock"`  | AWS Bedrock `:invoke-model`       | AWS credential chain (same as `bedrock`) | `anthropic.claude-3-5-sonnet-20241022-v2:0`     |
| `"anthropic-vertex"`   | GCP Vertex AI `:rawPredict`       | Google ADC (same as `vertex`)            | `claude-3-5-sonnet-v2@20241022` (note the `@`)  |

`type = "anthropic-bedrock"` is distinct from `type = "bedrock"` — both
can coexist in one config. The Converse path is the better default for
Claude (multi-vendor symmetry, simpler tool schema); the Anthropic-
native path is for features Converse omits (prompt caching, computer-
use, server tools).

`type = "anthropic-vertex"` is distinct from `type = "vertex"` (which is
Gemini-only generateContent) for the same reason.

`extended_thinking` works identically across all three: budget gets
silently clamped to `[1024, max_tokens)`, `temperature` is forced to 1
and `top_p` / `top_k` are cleared (all Anthropic-side hard requirements
when thinking is on).

Worked example mixing native + opt-in:

```toml
# Default Claude path: through Converse, multi-vendor adapter.
[providers.bedrock-claude]
type       = "bedrock"
model      = "anthropic.claude-3-5-sonnet-20241022-v2:0"
aws_region = "us-east-1"

# Same model, same region — but through the Anthropic-native path
# so you can use prompt caching / computer-use. Separate provider
# because the two adapters are not interchangeable.
[providers.claude-native]
type       = "anthropic-bedrock"
model      = "anthropic.claude-3-5-sonnet-20241022-v2:0"
aws_region = "us-east-1"
extended_thinking        = true
extended_thinking_budget = 8000
```

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
parallel guard that makes the backend isolation meaningful — without it the
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

## `[bash]`

```toml
[bash]
command_timeout     = "120s"   # budget for a foreground command that didn't set its own `timeout`
command_timeout_max = "1h"     # ceiling on an explicit `timeout` (runaway backstop)
```

A foreground `bash` command that runs longer than `command_timeout` is
killed (its whole process group, so pipeline children don't orphan) and
the tool returns the partial output plus a hint — the turn continues
instead of hanging until an operator steps in.

There are two ways to run something long. A command that **finishes on
its own but is slow** (a big test suite, a long build) runs in the
foreground with the tool's `timeout` arg raised; the value the model
passes is **honoured as given, up to `command_timeout_max`** (default
`1h`). The ceiling is a runaway backstop set generously enough to never
bite a real job — a 25-minute test can ask for `timeout: 1500` — while
still bounding a hallucinated absurd value; raise it for a repo with a
longer suite. A command that **never returns on its own** (a dev server,
a watcher) belongs in `run_in_background` instead — see below.

Set `command_timeout = "0s"` to disable the default timeout entirely
(commands then run until they exit or the turn is cancelled — the legacy
behaviour). For a command that legitimately needs to keep running (a dev
server, a file watcher, a long build), the model should instead pass
`run_in_background: true`: the command starts detached and the call
returns immediately with a job id. Use the `bash_output` tool to read its
output incrementally and `bash_kill` to stop it; any still-running
background jobs are killed when the session (or sub-agent) ends.

As a faster guard than the timeout, `bash` also recognises a handful of
commands that can't return on their own — `tail -f`, `watch`,
`journalctl -f`, `logs --follow`, common dev servers — and, when one is
issued in the foreground, returns immediately with a nudge to use
`run_in_background` rather than running it and waiting out the timeout.
Commands that already bound or detach themselves (a `timeout` wrapper,
`&`, `nohup`, a pipe into `head`) are left alone, and passing an explicit
`timeout` runs the command time-bounded without the nudge.

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

## `[backend]`

`[backend] type` is the sole backend selector — flip it to switch.

**Config scoping is enforced.** `type` and `workspace` are
selection/safety knobs — a personal or machine-admin preference — and
are read from any layer (system, user, or project). The per-backend
**environment** sub-tables (`[backend.podman]`, `[backend.lima]`,
`[backend.egress]`) describe what *the project* needs and must be
reproducible from the repo, so they are honored **only** from
project-scoped config: the repo's `.enso/config.toml` /
`.enso/config.local.toml`, or an explicit `-c` file. Set them in the
user or system config and they are **stripped with a warning** — never
composed across scopes.

User (or system) config — selection only:

```toml
[backend]
type      = "local"        # "local" (default) | "podman" | "lima"
workspace = ""             # "overlay" = throwaway copy + resolve (any backend)
```

The repo's `.enso/config.toml` — the environment:

```toml
[backend.egress]            # shared by podman + lima
allow       = []            # ["host[:port]", ...] outbound allowlist
credentials = {}            # { NAME = "$ENSO_SECRET" } brokered secrets

[backend.podman]            # type = "podman" only
image         = "alpine:latest"
init          = []                          # commands run once after creation
network       = ""                          # "" inherits; "none" / "host" / named
runtime       = "auto"                      # "auto" | "podman" | "docker"
extra_mounts  = []                          # ["src:dst[:opts]", ...]
env           = []                          # ["KEY=value", ...]
name          = ""                          # override auto-generated name
uid           = ""                          # --user value (rarely needed)
hardening     = ""                          # "gvisor" / "runsc"

[backend.lima]              # type = "lima" only; all optional
template     = "alpine"    # guest IMAGE distro (default alpine) or a path/URL
init         = []           # shell lines run once at VM provisioning
cpus         = 4
memory       = "4GiB"
disk         = "20GiB"
extra_mounts = []           # extra host paths, mounted read-only
```

Host `$HOME` is **not** mounted into the lima guest: the VM inherits an
image-only base (`template:_images/<distro>`), so the agent can't read
`~/.ssh`, `~/.config/enso`, or sibling repos. `template` picks the
image distro (`alpine`/`debian`/`ubuntu`/…), not a full Lima template;
a path/URL is used verbatim (you then own the mount posture). Extra
guest packages go in `init`, not a custom template. VMs created before
this change must be recreated (`limactl delete <vm>`) — enso prints a
one-time notice.

The pre-unification keys (`[bash.sandbox_options]`, `[lima]`,
`[backend] runtime`) are **removed** (breaking) — they are now silently
ignored by the TOML decoder, so a stale config runs with *default*
settings until migrated. See the CHANGELOG for the exact migration
mapping and [Sandbox]({{< relref "../docs/sandbox.md" >}}).

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
disable the corresponding event. `sh -c` invocation; 10s timeout.
Timeouts and template errors surface as a yellow chat line and a
slog warning; non-zero exit codes from your command are silent.

Two flavours: **template hooks** (`on_file_edit`, `on_session_end`)
use Go `text/template` syntax against per-event variables;
**event hooks** (`on_event`) receive the event as JSON on stdin
instead.

```toml
[hooks]
on_file_edit   = "gofmt -w {{.Path}}"
on_session_end = "notify-send 'enso done'"
on_event       = "node /path/to/dispatch.js"   # JSON on stdin
# on_events    = ["UserMessage", "ToolCallStart", "ToolCallEnd"]
#                # explicit allowlist; omit to use DefaultEventFilter
#                # (curated set that excludes per-token deltas).

[hooks.env]
# Merged onto every hook subprocess's environment, overriding
# matching keys from os.Environ(). Lets observers keep secrets out
# of shell rc files.
WATCHOURAI_TOKEN = "..."
```

| Event            | Fires when                                                          | Payload                    |
| ---------------- | ------------------------------------------------------------------- | -------------------------- |
| `on_file_edit`   | After the `edit` or `write` tool succeeds; before the result returns to the agent. Synchronous, so subsequent reads in the same turn see the post-hook file. | Template vars: `.Path`, `.Tool` (`"edit"` / `"write"`) |
| `on_session_end` | When the top-level `agent.Run` loop returns (any cause). Subagents and workflow roles do not fire this.                                                       | Template vars: `.SessionID`, `.Cwd` |
| `on_event`       | Per bus event after publication, filtered by `on_events` (default: `DefaultEventFilter`, excludes per-token deltas). Fires off the agent's hot path on a fanout goroutine, bounded by the 10s timeout. | JSON on stdin: `{session_id, cwd, type, payload}` |

`on_event` is the supported low-friction integration point for
external observers (status boards, audit pipelines, watchourai-style
visualisers). See the **External observers** section of the README
for the end-to-end shape, including the daemon-socket subscription
alternative for high-volume / stateful consumers.

## `[mcp.<name>]`

```toml
[mcp.gitea]
command = "gitea-mcp-server"
args    = ["--token", "$TOKEN"]   # $VAR expansion at startup

[mcp.notion]
url          = "https://mcp.notion.com/v1"
headers      = { Authorization = "Bearer $NOTION_TOKEN" }   # HTTP-only; $VAR expanded
call_timeout = "120s"   # max time for a single tool invocation; "0s" disables
```

See [MCP servers]({{< relref "../docs/mcp.md" >}}). `command` and
`url` are mutually exclusive; `headers` is HTTP-only. `call_timeout`
bounds a single tool invocation against the server (default `120s`); on
expiry the call is abandoned and the model gets a timeout notice so the
turn keeps moving rather than hanging on an unresponsive server. Set
`"0s"` to disable. The connection/handshake budget is separate and fixed.

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
