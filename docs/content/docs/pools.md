---
title: Pools & concurrency
weight: 14
---

# Pools & provider concurrency

A **pool** bounds how many LLM requests run at once across a set of
providers that share a constraint — typically one GPU behind one
llama-swap, but also a cloud endpoint's rate limit. Without pools, two
models served by the same backend would each get their own concurrency
budget and fight over the hardware; pools make co-located models
*serialise* instead of thrash.

## Zero-config behaviour

You usually don't configure anything. Providers are auto-grouped by
`endpoint`: every provider pointing at the same URL shares one pool
named `auto-<host>-<port>`, with concurrency 1 (one request at a time).
That's the right default for a single llama-swap or Ollama serving
several models.

A provider on its own unique endpoint gets a solo pool that inherits its
`concurrency` setting — so the pre-pools `concurrency = N` on a distinct
endpoint keeps working unchanged.

## Explicit pools

Override grouping with a per-provider `pool = "<name>"`, and tune the
pool with a `[pools.<name>]` block:

```toml
[providers.qwen-fast]
endpoint = "http://latchkey:4000/v1"
model    = "qwen3.6-27b"
pool     = "latchkey-3090"

[providers.qwen-deep]
endpoint = "http://latchkey:4000/v1"
model    = "minimax-m2.7"
pool     = "latchkey-3090"   # shares one semaphore with qwen-fast

[pools.latchkey-3090]
concurrency   = 1
queue_timeout = "300s"
swap_cost     = "high"
```

| Key | Default | Meaning |
| --- | --- | --- |
| `concurrency` | 1 | Max in-flight requests across **all** members. |
| `queue_timeout` | `300s` | How long a request waits for a slot before erroring. |
| `swap_cost` | `""` | Hint (`low`/`high`) shown to the model so it avoids needless same-pool swaps. |
| `rpm` / `tpm` / `daily_budget` | unset | **Reserved** for cloud rate-limit scheduling — parsed, warned-once, not yet enforced. |

Full field semantics live in the
[config reference]({{< relref "../config/reference.md" >}}).

## What the model sees

Pool membership and `swap_cost` are rendered into the
[`## Available models`]({{< relref "prompt-layering.md" >}}#available-models-section)
prompt section, and when two listed providers share a pool a one-line
note explains that switching between them is expensive. This nudges the
model to finish work on one model before `/model`-swapping and to
prefer delegating across *different* pools when it fans out.

## Saturation behaviour

When a pool is full, new requests **queue and are granted in FIFO
arrival order** — the model just sees the tool/turn take a while. If a
request waits longer than `queue_timeout`, it returns an error and the
model decides what to do (retry, pick another model, give up). Ctrl-C
cancels a queued wait immediately.

## Coordination scope

Pools are in-memory semaphores. What they cover depends on how you run
ensō:

- **One `enso daemon`** — every session it hosts (`enso run --detach`,
  attached clients) and all their sub-agents share the daemon's pools.
  This is full cross-session coordination: the daemon runs every agent
  loop in-process, so they all contend on the same semaphores.
- **A standalone `enso` process** — the agent and any sub-agents it
  spawns share that process's pools.
- **Gap:** two *separate* standalone processes on the same host do
  **not** see each other's pools and can thrash shared hardware. Run
  `enso daemon` and attach if you need cross-process coordination.

## See also

- [Config reference: `[pools.<name>]`]({{< relref "../config/reference.md" >}})
- [Daemon]({{< relref "daemon.md" >}}) — long-lived server that
  coordinates pools across sessions.
- [Prompt layering]({{< relref "prompt-layering.md" >}}) — where
  pool/`swap_cost` is surfaced to the model.
