---
title: Install
weight: 1
---

# Install

ensō is a single static Go binary (named `enso`). No CGO, no runtime
dependencies on the host beyond a POSIX shell (and optionally `podman`
or `docker` if you turn the bash sandbox on).

## Prerequisites

- **Go 1.23+** (only if building from source).
- **An OpenAI-compatible chat endpoint** — `llama.cpp`'s `llama-server`
  is the reference target; vLLM, Ollama, OpenAI itself, anything that
  speaks Chat Completions with SSE streaming will work.
- **POSIX OS** (Linux/macOS/BSD). Windows builds compile but the
  daemon path is unsupported there; run via WSL.

## Build from source

```bash
git clone https://github.com/TaraTheStar/enso.git
cd enso
make build              # produces ./bin/enso
```

Common Make targets:

| target            | what it does                                            |
| ----------------- | ------------------------------------------------------- |
| `make build`      | Compile to `./bin/enso`.                                |
| `make run`        | Build + launch the TUI.                                 |
| `make daemon`     | Build + run the daemon in the foreground.               |
| `make test`       | Run the unit test suite.                                |
| `make check`      | gofmt + vet + test + build (full pre-commit gate).      |
| `make tidy`       | Refresh `go.mod` / `go.sum`.                            |
| `make help`       | Print this list.                                        |

The Makefile sets `CGO_ENABLED=0`, `-trimpath`, and `-ldflags '-s -w'`
by default — the resulting binary is reproducible and stripped.

## Setting up a local model

For the default config, point ensō at `llama.cpp`'s `llama-server`.
On a single RTX 3090:

```bash
llama-server \
  -m unsloth/Qwen3.6-35B-A3B-GGUF/UD-Q4_K_XL.gguf \
  -ngl 999 -c 32768 -fa on --no-mmap \
  -ctk q8_0 -ctv q8_0 \
  --jinja --reasoning-budget 4096 --reasoning-budget-message \
  --temp 0.6 --top-k 20 --top-p 0.95 --min-p 0.0 --presence-penalty 1.5 \
  --port 8080
```

Any other backend works — set `endpoint` and `model` in
`~/.config/enso/config.toml` (or any other layered config path) to
point elsewhere. See the
[config reference]({{< relref "config/reference.md" >}}) for the full list.

## Optional: bash sandbox

If you want the agent's shell to run inside a per-project container,
install one of:

- **[podman](https://podman.io/)** (rootless, no daemon) — recommended.
- **[docker](https://docs.docker.com/engine/install/)** — works equally
  well; runs as the docker daemon's user.

Then set `[bash] sandbox = "auto"` in your config. ensō auto-detects
which is available; if you want to pin one, use `"podman"` or
`"docker"`. Details in the [sandbox page]({{< relref "docs/sandbox.md" >}}).

## Optional: language servers

Configure `[lsp.<name>]` blocks for each language you work with. Common
servers:

- `gopls` — Go.
- `rust-analyzer` — Rust.
- `typescript-language-server --stdio` — TypeScript / JavaScript.
- `pyright-langserver --stdio` — Python.
- `clangd` — C / C++.

See [LSP integration]({{< relref "docs/lsp.md" >}}) for example
configs. Servers are spawned lazily on first use and reused for the
session.

## Next: [Quickstart]({{< relref "quickstart.md" >}})
