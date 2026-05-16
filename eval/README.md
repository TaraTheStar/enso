# enso eval harness

Phase-1 measurement infrastructure for the prompt-design work. Runs a small,
fixed task suite against any number of providers, scores each run from the
`enso run --format json` event stream, and writes a CSV.

The harness is intentionally minimal. It does not try to be a benchmark.
Its job is to convert "is this prompt change actually helping?" from a vibes
question into a measured one.

## What it measures

For every (task × model) cell, one CSV row with:

| metric | source |
|---|---|
| `pass` | `check.sh` exit code |
| `turns` | count of `assistant_done` events |
| `tool_calls` / `tool_errors` | `tool_call_start` / `tool_call_end` events |
| `hallucinated_tools` | tool names not in the task's `expected_tools` list |
| `assistant_bytes` / `reasoning_bytes` | size of `assistant_delta` vs `reasoning_delta` |
| `think_leak` | true if `<think>` appeared in `assistant_delta` (reasoning escaping the reasoning channel) |
| `hit_max_turns` | true if the agent loop hit the task's cap |
| `wall_s` | wall-clock seconds across all legs |
| `swap_*` | same metrics for the second leg of swap tasks |

Reasoning bytes are tracked separately from assistant bytes — for thinking
models, large reasoning is fine, large *visible* output usually isn't.

## Running it

```sh
make build
go run ./eval/cmd/runner \
  --enso ./bin/enso \
  --config ~/.config/enso/config.toml \
  --models gemma4-31b,qwen3.6-27b,gemma4-26b-a4b,qwen3.6-35b-a3b \
  --output eval/results/run.csv
```

The named providers must exist in the config you pass via `--config`.
The runner is strict about this: an unknown name fails the cell with a
clear error rather than silently falling back.

### Useful flags

- `--filter 01-edit-whitespace,07-execute-not-narrate` — run a subset.
- `--swap-model <name>` — for swap tasks, which provider to switch to
  on the second leg. Defaults to the second entry in `--models`.
- `-v` — tee the JSON event stream to stderr while the agent works.

### Isolation

Each cell gets its own tempdir for `HOME` (so the spawned `enso run` writes
its session DB into `<tempdir>/.local/share/enso/enso.db`, not yours) and its own
working directory seeded from the task's `fixtures/`. On pass, both are
deleted. On fail, both are kept and their paths logged so you can poke at
what the model produced.

The user config is layered in via `enso run -c <path>`, so the runner does
not need to know about your provider definitions, API keys, or sampler
settings.

## Adding a task

Each task is a directory under `eval/tasks/<id>/`:

```
eval/tasks/01-edit-whitespace/
  task.json          # description, prompt, max_turns, expected_tools, optional swap step
  fixtures/          # files copied verbatim into the workdir before the run
  check.sh           # exit 0 iff the task is satisfied
```

`task.json` schema:

```json
{
  "description": "human-readable note",
  "prompt": "what the user types",
  "max_turns": 12,
  "expected_tools": ["read", "edit", "write", "grep", "glob", "bash", "todo"],
  "init_cmds": ["git init -q"],
  "swap": {
    "prompt": "second turn after the model swap",
    "max_turns": 16,
    "new_model": "optional; overrides --swap-model for this task"
  }
}
```

Keep tasks small. The point is signal per dollar of inference, not a
realistic codebase.

## Reading the CSV

Pass/fail is the verdict but rarely the most interesting column. The
patterns to look for:

- **Hallucinated tools** consistently nonzero on a model → that family
  needs an explicit tool-name allowlist in its system prompt.
- **`think_leak` true** → the harness or system prompt isn't separating
  reasoning from visible output for that model.
- **`hit_max_turns`** without `pass` → agent gave up or got stuck;
  inspect the kept workdir.
- **`assistant_bytes` >> tool_calls × small constant** → the model is
  narrating instead of executing. Expected on `05-ambiguous-request` and
  the "ask first" path; suspicious elsewhere.
- **Big swap_tool_errors after a successful first leg** → likely a
  silent-swap regression: the new model is still anchored to the
  previous model's tool-call style or is confused by the existing
  history. This is the failure mode we built the swap task to surface.

## What this does *not* do

- It does not test prompt variants yet. The phase-1 baseline is
  the current `internal/embed/default_enso.md`. Phase 2 adds a
  `--system-prompt-file` override to `enso run` and a `--prompt`
  flag here to compare candidates.
- It does not stress-test long contexts, large repos, or
  rate-limited cloud providers. All tasks fit in well under
  10k tokens of context.
- It does not validate model output quality beyond `check.sh` pass/fail
  and the tracked metrics. There is no LLM-as-judge step.
