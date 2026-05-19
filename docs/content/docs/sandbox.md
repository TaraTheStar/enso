---
title: Sandbox
weight: 4
---

# Sandbox

By default (`[backend] type = "local"`) the agent runs as your user
on the host. With `[backend] type = "podman"` (or `"lima"`) the
**whole agent** — model loop and every tool — runs inside a
per-project container or VM, with the project mounted at its **real
path** (not `/work`; there is one filesystem namespace by
construction). It can't see `~`, `~/.ssh`, sibling repos, or anything
else outside the project.

The sandbox is **off by default**. Turn it on per project (or
globally in your user config) when you want the safety.

## Threat model

The thing the sandbox addresses: **bash escaping the project
directory**. `rm -rf ~`, `cat ~/.ssh/id_rsa`, `find / -name "*.env"`
— all blocked because `~` and `/` are the *container's* views, not
the host's.

The sandbox does **not** address:

- A malicious sophisticated attacker who finds a container-runtime
  bug and breaks out. Docker/podman are not security boundaries
  against root-in-container exploits.
- The agent reading sensitive files via the `read`/`grep` tools.
  Those run in the ensō process on the host — but when the sandbox is
  on, they're confined to cwd + `additional_directories` by a parallel
  guard. See [path confinement](#path-confinement-of-file-tools)
  below.
- Network exfiltration. By default the container inherits the host
  network; set `network = "none"` if you want it offline.

If your threat model is "the model writes confidently-wrong destructive
commands," the sandbox solves that. If it's "an adversary has
compromised the model and is trying to exfiltrate," you want a
multi-layered story — sandbox + denied web_fetch domains + sensitive
files in `.ensoignore` + network = "none".

## Configuration

```toml
[backend]
type      = "podman"      # "local" (default) | "podman" | "lima"
# workspace = "overlay"   # throwaway copy + resolve (any backend)

[backend.podman]           # used when type = "podman"
image        = "alpine:latest"
init         = ["apk add --no-cache git curl jq make"]
network      = ""         # "" inherits runtime default; "none" = offline
# runtime    = "auto"     # "auto" | "podman" | "docker"
extra_mounts = ["~/.cache/go-build:/root/.cache/go-build:rw"]
env          = []
# name       = "..."      # override auto-generated name
# uid        = "..."      # override default user (rarely needed)
# workdir_mount = "/work"
```

`[backend] type` is the **single** backend selector — flip it to
switch. `"local"` (no isolation, the default), `"podman"` (container),
or `"lima"` (full-VM). An empty or unrecognized value falls safe to
`"local"`. Each backend's environment lives under its own
`[backend.<name>]` sub-table.

**Scoping (enforced).** `type` and `workspace` are a personal/machine
preference — set them wherever you like (user config is typical). The
per-backend *environment* (`[backend.podman]`, `[backend.lima]`,
`[backend.egress]` — image, packages/`init`, template, mounts, egress,
hardening) describes what the **project** needs and must be
reproducible for teammates and CI, so it is read **only** from the
repo's `.enso/config.toml` (or `.enso/config.local.toml`, or an
explicit `-c` file). Put it in the user or system config and it is
**stripped with a warning** — there is deliberately no user-global
backend environment, so a user's `init` can never silently collide with
a repo's.

With `type = "podman"`, `[backend.podman] runtime` chooses the
container CLI: `"auto"` (default) prefers podman (rootless, no daemon)
and falls back to docker; pin one with `"podman"` or `"docker"`.

> **Migration (breaking):** the per-backend config was unified. The old
> `[bash.sandbox_options]` / `[lima]` / `[backend] runtime` keys are
> **removed** and silently ignored, so a stale config runs with
> *default* settings (e.g. `alpine:latest`, no `init`) until migrated.
> Move container settings to `[backend.podman]`, VM settings to
> `[backend.lima]`, the egress allowlist/credentials to
> `[backend.egress]` (`egress` → `allow`), and the overlay toggle to
> `[backend] workspace`. See the CHANGELOG for the full mapping.

## Per-project containers

Each project gets its own container, named
`enso-<basename>-<6-hex-of-cwd>`:

```
~/dev/enso       → enso-enso-d3a2c1
~/dev/api        → enso-api-7f8b09
```

The 6-hex suffix is computed from the absolute path so two projects
named `frontend` at different paths don't collide.

Containers are **persistent across ensō runs**. First start pays the
image-pull and init cost; subsequent runs `podman start` instantly.
State (installed packages, modified rootfs) carries over until the
config changes.

## Init commands

`init` runs once after container creation. Re-runs **only when**
image, init list, mounts, env, network, or workdir change — tracked
via an `enso.init-hash` label on the container. Edit the init list,
restart ensō, and the container is rebuilt.

If init fails, the half-baked container is removed immediately so it
won't be reused.

Examples:

```toml
# Go project
init = [
  "apk add --no-cache git make gcc musl-dev",
  "wget -qO- https://go.dev/dl/go1.22.linux-amd64.tar.gz | tar -C /usr/local -xz",
  "ln -sf /usr/local/go/bin/go /usr/local/bin/go",
]
```

```toml
# Node project — use a richer base image instead of installing on top of alpine
image = "node:20-alpine"
init  = ["apk add --no-cache git make"]
```

```toml
# Python project
image = "python:3.12-slim"
init  = ["apt-get update && apt-get install -y --no-install-recommends git make"]
```

## Path confinement of file tools

When the sandbox is enabled, `read`, `write`, `edit`, `grep`, and
`glob` are also restricted: they refuse paths that don't resolve
under cwd or one of `[permissions] additional_directories`. This
mirrors the bash sandbox at the host-tool level so the model can't
bypass it via path arguments.

Symbolic links are *not* followed for the confinement check — a
symlink at `./.env` pointing at `/etc/passwd` is rejected based on
the lexical path, not the resolved one.

## Managing sandboxes

Podman containers are per-task and `--rm`: they clean themselves up
when the task ends, so there is nothing to start/stop/remove by hand.
Lima VMs are persistent per project and are recreated automatically
when their config changes. The one maintenance command reclaims
stragglers — terminal podman workers (and their anonymous volumes)
plus enso-managed lima VMs and accumulated workspace review copies:

```bash
enso prune                  # reclaim everything enso-managed, all projects
enso prune --older-than 168h   # only instances idle ≥ 7 days
```

(The legacy `enso sandbox list|stop|rm` commands and the persistent
per-project podman container they managed were removed — podman is now
strictly per-task.)

`prune` only touches containers with the `enso.managed=true` label, so
your dev databases, redis, postgres, etc. are safe.

## File ownership

On Linux with **rootless podman**, the host user maps to the
container's root via user namespaces — bind-mount writes from inside
the container land as your user on the host. No special config needed.

On Linux with **docker** (root daemon), bind-mount writes default to
root-owned. Set `uid = "1000:1000"` (or whichever your UID is) in
`[backend.podman]` to make new files match your host user.

On macOS Docker Desktop: handles UID translation automatically; no
config needed.

## Workspace overlay

```toml
[backend]
workspace = "overlay"
```

With `workspace = "overlay"` the project is cloned (`cp
--reflink=auto`, near-free on CoW filesystems) **before** the agent
runs into two trees: a pristine `base/` baseline and a `merged/` the
agent works in (bind-mounted at the project's real path inside the
box, so the real tree is untouched while it runs).

At task end ensō does a **three-way compare**: `base→merged` is
exactly what the agent did; `base→project` is anything that changed
on the host meanwhile. Files only the agent touched apply cleanly;
files **both** sides changed are *conflicts*.

- **Interactive** (TUI / `enso` at a terminal): the agent's unified
  diff is shown (conflicts listed), then a prompt — `[c]ommit`
  (apply the non-conflicting changes per file; conflicts are left for
  you to merge and the copy is kept), `[d]iscard`, `[k]eep`, `[v]iew`
  the full diff, or — when there are conflicts — `[f]orce-all` (apply
  the agent's version even over host edits, requires typing
  `overwrite`). Bare Enter is **keep** — never a silent commit or
  destroy. Commit is per-file (create/modify/delete); there is no
  blanket `rsync --delete`, so files neither side changed (and any
  unrelated host work) are never touched.
- **Non-interactive** (`enso run`, daemon): nothing is applied — the
  copy and baseline are **kept** with a summary of how many changes
  apply cleanly vs. need a manual merge. Never auto-committed, never
  destroyed.

Because of the baseline, **concurrent host edits are no longer a
footgun**: editing the project (or running `git`) while the agent
runs only creates a conflict on the *specific files both sides
touched* — your other host work is preserved automatically, and
conflicts are never clobbered without an explicit `force` +
`overwrite`. `.git` is excluded from the compare (the overlay syncs
working-tree files, not git internals, and git churn would otherwise
dominate). Co-editing the same tree (e.g. hacking on ensō itself) is
safe, though for a clean diff it's still tidier to commit/stash
in-progress work first.

## gVisor hardening

By default the container uses the runtime's normal OCI runtime
(`runc`/`crun`), which shares the host kernel. Set:

```toml
[backend.podman]
hardening = "gvisor"   # alias: "runsc"
```

to run the box under [gVisor](https://gvisor.dev) instead. gVisor
intercepts the container's syscalls in a userspace kernel, so a
kernel-level container escape has a much smaller surface. The cost is
performance: syscall-heavy workloads (big builds, lots of small file
I/O) run noticeably slower. Linux only.

**Fail-safe.** If `runsc` isn't installed, ensō *refuses to start*
with an actionable hint rather than silently running unhardened — an
explicit hardening request is never quietly downgraded. An unknown
`hardening` value is passed through and fails the same availability
check loudly.

**Rootless: handled automatically.** Rootless podman can't use the
systemd cgroup manager without an interactive polkit session, and
rootless `runsc` can't configure cgroups or run with the root network
namespace. ensō adapts its *own* `podman` invocation for this — it
uses the `cgroupfs` cgroup manager and points `--runtime` at a private
`runsc --ignore-cgroups [--network=none]` wrapper under its runtime
dir. **It never edits your `containers.conf`** or global podman
config; the adaptation is scoped to ensō's containers.

**Requirements / known limits.** You need `runsc` on `PATH` for your
user, cgroup v2, and a kernel that permits unprivileged user
namespaces. gVisor is **very sensitive to your kernel version**, and a
distro-packaged `runsc` can lag a bleeding-edge kernel by enough that
it runs shell images fine but exits Go binaries on an unimplemented
syscall — symptom: the box starts but the agent never comes up. That
is a runsc/host-kernel mismatch, not ensō wiring (ensō surfaces the
captured runtime error plus this guidance). Fix: install a current
upstream `runsc` (gVisor nightly/release) matching your kernel — e.g.
on Debian sid the repo `runsc` may be too old; the upstream build
works. Or unset `hardening` to run without gVisor.

## Lima (VM isolation)

Podman shares the host kernel; gVisor narrows that but still is not a
separate kernel. For the strongest isolation tier ensō can run the
whole agent inside a real VM via [Lima](https://lima-vm.io). Select
it explicitly:

```toml
[backend]
type = "lima"             # "local" | "podman" | "lima"

[backend.lima]
# template     = "alpine"       # guest IMAGE (alpine default | debian | ubuntu …)
# init         = ["apk add --no-cache git"]   # extra guest packages
# cpus         = 4
# memory       = "4GiB"
# disk         = "20GiB"
# extra_mounts = ["~/.cache/go-build"]   # mounted read-only
```

**Host `$HOME` is never mounted into the lima guest.** The VM inherits
an *image-only* base (`template:_images/<distro>`, default Alpine) — not
a full Lima template, whose base chain would bind your home directory
read-only into the guest. So the agent cannot read `~/.ssh`,
`~/.config/enso` (your provider API keys), or sibling repos; it sees
only the project copy (writable at its real path) and the read-only
enso binary. `template` selects the guest **image distro**, not an
arbitrary Lima template; a path/URL is still accepted verbatim but then
*you* own the mount posture.

> **Recreate existing VMs.** A persistent per-project VM created before
> this fix still mounts `$HOME`. enso reuses a running VM as-is, so it
> prints a one-time notice; apply the fix with
> `limactl stop <vm> && limactl delete <vm>` (the VM is rebuilt on the
> next run).

`[backend.lima] init` runs once during VM provisioning (rendered into
the generated instance YAML's `provision:` block as a `mode: system`
script with `set -e`) — the podman `init` analogue for installing
toolchains the image lacks. On the default Alpine image enso also
auto-installs `iptables` (the cloud image omits it; the egress seal
requires it) as a separate provisioning step ahead of your `init`.

Lima is **not** macOS-only (Colima is the separate macOS container
wrapper, not Lima). It runs on macOS (vz/qemu), Linux (qemu+KVM —
needs `/dev/kvm`), and Windows (wsl2). Install: macOS
`brew install lima`; Linux see the
[Lima install docs](https://lima-vm.io/docs/installation/).

Same seam as the other backends: the worker runs `enso __worker`
inside the guest, dials no model (inference is host-proxied over the
`limactl shell` channel), and the project is mounted at its **real
path** so there is one filesystem namespace. Genuinely network-sealed
by default — the guest's outbound traffic is firewalled to the host
egress proxy only (see [Egress](#egress) below), not merely sealed by
inference being host-proxied.

**Substrate model — persistent per-project VM.** A cold per-task VM
boot (image download + tens of seconds) is impractical, so the VM is
**persistent and keyed per project** (`enso-<base>-<projecthash>`):
created once, then resumed (`limactl start`) for later tasks. Per-task
*workspace* isolation is still total — the host-side workspace overlay
copy is what gets mounted in, at a stable per-project staging path so
the VM's mount config never changes. The deliberate tradeoff: a
project's **own sequential tasks share the VM userland** (packages a
task installed, mutated system state) — bounded to that one project
(it can never reach another project's VM) and matching the threat
model (the agent's own mistakes, already project-scoped). The
safety-max follow-up is a fresh per-task qcow2 snapshot clone.

**Reclaiming VMs.** Because the VM is meant to persist, it is **not**
garbage-collected automatically (no startup sweep, no teardown
delete). Remove enso VMs explicitly:

```sh
enso prune                 # delete all enso lima VMs
enso prune --older-than 168h   # only VMs idle ≥ 7 days
```

**Fail-safe.** If `limactl` isn't on `PATH`, ensō *refuses to start*
with an actionable install hint rather than silently downgrading
isolation — an explicit `type = "lima"` is never quietly weakened.

**Perf note.** First use of a project downloads a cloud image and
boots a VM (tens of seconds to minutes). Subsequent tasks reuse the
running/stopped VM and start quickly — that reuse is the whole reason
for the persistent-substrate design.

## Egress

Every sealed backend (podman, Lima) is **default-deny outbound**: the
box has no route to the internet except a host-side allowlist proxy,
and inference never needs it (it is host-proxied over the control
channel). There are three ways a task reaches the network, in order of
precedence:

1. **Static allowlist** — pre-authorize exact targets in config. The
   proxy starts with these open; nothing else is.

   ```toml
   [backend.egress]   # shared by podman + lima
   allow       = ["api.github.com:443", "pypi.org:443", "files.pythonhosted.org:443"]
   credentials = { GH_TOKEN = "ghp_…" }   # brokered to the box, never in its env
   ```

2. **Interactive prompt** — in the attended TUI, a connection to a
   target that is *not* on the static allowlist pauses and asks:

   ```
   ? Allow network egress to github.com:443?
     a sandboxed command tried to reach the network
     [y]es once  [t] this task  [n]o
   ```

   `y` allows that one connection; `t` allows that target for the rest
   of the task (memoised — no re-prompt); `n` refuses it (the command
   sees a `403`). This covers `bash` (`curl`, `git`, `pip`/`npm`/`go`)
   and the `web_fetch` / `web_search` tools uniformly, because they all
   egress through the same injected proxy. The box stays structurally
   sealed throughout — a grant just opens that one target on the host
   proxy; all traffic remains observable there.

3. **`--yolo`** — lifts the gate entirely: the proxy runs allow-all, so
   every destination is permitted with no prompts. The box is still
   structurally sealed and all traffic still flows through (and is
   observable at) the host proxy; only the default-deny decision is
   removed. Configured `credentials` stay explicit even under `--yolo`
   ("all network" never means "all secrets").

**Headless runs fail closed.** `enso run` and any non-interactive path
have no TTY to answer a prompt, so an off-allowlist egress is **denied
with a reason** (never hangs). Use the static allowlist or `--yolo`
for unattended work. Raw non-HTTP TCP (e.g. SSH `git@…`) is not
proxyable — use HTTPS remotes inside a sealed box.

## Failure modes

- **Runtime not installed**: ensō refuses to start with a clear
  message naming both supported runtimes.
- **Image pull fails**: ensō surfaces the runtime error and exits.
  Common causes: registry rate limits, no internet, invalid image
  name.
- **Init fails**: `set -e` aborts before the worker starts, the task
  fails, and the init stderr is surfaced in the startup diagnostic.
- **A lima VM or stale podman worker is lingering**: `enso prune`
  (add `--older-than` to keep recent ones).

## Daemon mode caveat

The daemon path doesn't currently expose `[backend] type`. Each
`enso run --detach` can target a different cwd, but the registry is
shared across sessions and per-session sandboxing isn't in v1 scope.
Use `enso run` or `enso tui` (in-process) if you need the sandbox.
