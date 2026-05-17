---
title: Sandbox
weight: 4
---

# Sandbox

By default the `bash` tool runs as your user on the host. With
`[bash] sandbox = "auto"` (or `"podman"` / `"docker"`), every `bash`
call instead runs inside a per-project container with the project cwd
bind-mounted at `/work`. The agent's shell can't see `~`, `~/.ssh`,
sibling repos, or anything else outside the project.

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
[bash]
sandbox = "auto"          # "off" | "auto" | "podman" | "docker"

[bash.sandbox_options]
image        = "alpine:latest"
init         = ["apk add --no-cache git curl jq make"]
network      = ""         # "" inherits runtime default; "none" = offline
extra_mounts = ["~/.cache/go-build:/root/.cache/go-build:rw"]
env          = []
# name       = "..."      # override auto-generated name
# uid        = "..."      # override default user (rarely needed)
# workdir_mount = "/work"
```

`sandbox = "auto"` prefers podman (rootless, no daemon) and falls
back to docker. To pin one explicitly use `"podman"` or `"docker"`.

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

## Managing containers

```bash
enso sandbox list      # show every enso-managed container, all projects
enso sandbox stop      # stop the current project's container (keeps state)
enso sandbox rm        # stop and remove the current project's container
enso sandbox prune     # remove every enso-managed container, all projects
```

`list` filters by the `enso.managed=true` label; you'll never see
unrelated containers in the output.

`prune` only touches containers with the `enso.managed=true` label, so
your dev databases, redis, postgres, etc. are safe.

## File ownership

On Linux with **rootless podman**, the host user maps to the
container's root via user namespaces — bind-mount writes from inside
the container land as your user on the host. No special config needed.

On Linux with **docker** (root daemon), bind-mount writes default to
root-owned. Set `uid = "1000:1000"` (or whichever your UID is) in
`[bash.sandbox_options]` to make new files match your host user.

On macOS Docker Desktop: handles UID translation automatically; no
config needed.

## gVisor hardening

By default the container uses the runtime's normal OCI runtime
(`runc`/`crun`), which shares the host kernel. Set:

```toml
[bash.sandbox_options]
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

## Failure modes

- **Runtime not installed**: ensō refuses to start with a clear
  message naming both supported runtimes.
- **Image pull fails**: ensō surfaces the runtime error and exits.
  Common causes: registry rate limits, no internet, invalid image
  name.
- **Init script fails**: container is removed and ensō reports which
  init line failed.
- **Container survives across ensō runs but you want to nuke it**:
  `enso sandbox rm` (current project) or `enso sandbox prune` (all).

## Daemon mode caveat

The daemon path doesn't currently expose `[bash] sandbox`. Each
`enso run --detach` can target a different cwd, but the registry is
shared across sessions and per-session sandboxing isn't in v1 scope.
Use `enso run` or `enso tui` (in-process) if you need the sandbox.
