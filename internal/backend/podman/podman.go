// SPDX-License-Identifier: AGPL-3.0-or-later

// Package podman implements the sandboxed Backend: the agent-core
// Worker runs as PID 1 of a `podman run --rm` container. There is no
// per-tool Exec/Mount surface and no /work translation — the project
// directory is bind-mounted at its REAL host path and the worker's cwd
// is that same path, so there is exactly one filesystem namespace by
// construction (the structural fix for the historical split-brain bug).
//
// Channel transport is the container process's stdio, identical to
// LocalBackend: the host writes framed envelopes to the `podman run`
// process's stdin and reads them from its stdout (podman's own logs go
// to stderr, which is inherited). The same `enso __worker` entrypoint
// runs inside; the host enso binary is bind-mounted read-only so no
// image rebuild is needed.
//
// Network is sealed by default (`--network none`): the worker dials no
// model (inference is host-proxied over the Channel) and reaches the
// outside only via the tier-3 capability broker. Per-task naming +
// labels make orphan GC possible.
package podman

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/TaraTheStar/enso/internal/backend"
	"github.com/TaraTheStar/enso/internal/backend/exestage"
	"github.com/TaraTheStar/enso/internal/backend/seal"
	"github.com/TaraTheStar/enso/internal/paths"
)

// Backend launches each task's Worker in a fresh `podman run --rm`
// container. The zero value is not usable; construct via host.SelectBackend
// (which fills it from config) or set fields directly.
type Backend struct {
	// Exe is the host path to a STATIC (CGO_ENABLED=0) enso binary,
	// bind-mounted read-only to /usr/local/bin/enso in the container.
	// Empty → the running executable (correct for release builds, which
	// are static).
	Exe string

	// Runtime is "podman", "docker", or "auto" (resolved via the same
	// resolver `enso prune` uses, so they cannot disagree).
	Runtime string

	Image       string   // container image; required
	Network     string   // --network value; "" → "none" (sealed)
	ExtraMounts []string // additional -v entries (host:container[:opts])
	Env         []string // -e KEY=VALUE: explicit opt-in only (never host env)
	UID         string   // --user value; "" → runtime default

	// Init are shell lines run once in the container BEFORE the worker
	// starts (the container is the worker: there is no persistent box
	// to exec into, so init is wrapped into the entrypoint). Output is
	// redirected to stderr so it can't corrupt the binary Channel on
	// stdout; `set -e` so a failed init fails the task rather than
	// running a half-provisioned box. Because each task is a fresh
	// `podman run --rm`, init re-runs per task — keep it cheap (package
	// installs), or bake heavy tooling into the image.
	Init []string

	// MountSource is the host path bind-mounted at the project's REAL
	// path inside the container. Empty → the project dir itself
	// (in-place). The workspace overlay sets this to a
	// host-controlled throwaway copy so the real project is untouched
	// while the agent runs; the in-container path is unchanged either
	// way (one namespace). Podman does not manage the overlay — the
	// caller owns its lifecycle (commit/discard/keep at task end).
	MountSource string

	// EgressProxy, when set, is the HTTPS_PROXY value injected into the
	// container so the only route out is the host's allowlist proxy
	// (egress control). Empty keeps the box fully sealed.
	EgressProxy string

	// OCIRuntime is an optional `--runtime` value selecting a hardened
	// OCI runtime — "runsc" runs the container under gVisor, which
	// intercepts and filters syscalls in a userspace kernel (a much
	// smaller host kernel attack surface, at a syscall-heavy
	// performance cost; Linux only). Empty = the runtime default
	// (runc/crun). If set but the runtime is not installed, Start
	// refuses to launch rather than silently running unhardened.
	OCIRuntime string
}

func (b *Backend) Name() string { return "podman" }

// Start provisions and launches the container Worker. spec.TaskID names
// the box (enso-<base>-<taskid>); the project dir is the only thing
// mounted in (plus the read-only enso binary) — no host $HOME.
func (b *Backend) Start(ctx context.Context, spec backend.TaskSpec) (backend.Worker, error) {
	// Fail safe FIRST (cheapest, most actionable): a requested hardened
	// runtime that is not installed must REFUSE, never silently fall
	// back to unhardened isolation.
	if b.OCIRuntime != "" && !runtimeAvailable(b.OCIRuntime) {
		return nil, fmt.Errorf(
			"podman: OCI runtime %q not found on PATH — gVisor isolation requires it. "+
				"Install gVisor (https://gvisor.dev/docs/user_guide/install/) and configure "+
				"%q as a podman runtime, or unset [backend.podman] hardening. "+
				"Refusing to run unhardened.", b.OCIRuntime, b.OCIRuntime)
	}

	runtime, err := resolveRuntimeBinary(b.Runtime)
	if err != nil {
		return nil, fmt.Errorf("podman: %w", err)
	}
	exe := b.Exe
	if exe == "" {
		if exe, err = os.Executable(); err != nil {
			return nil, fmt.Errorf("podman: resolve executable: %w", err)
		}
	}
	// Bind-mount an IMMUTABLE content-addressed snapshot, not the
	// host's live build output: a rebuild overwriting it in place can
	// corrupt the running container's mmap of the binary (invalid
	// runtime symbol table). Content-addressed → copied at most once.
	if exe, _, err = exestage.Stage(exe); err != nil {
		return nil, fmt.Errorf("podman: stage executable: %w", err)
	}
	if b.Image == "" {
		return nil, fmt.Errorf("podman: no image configured")
	}
	if spec.Cwd == "" {
		return nil, fmt.Errorf("podman: empty cwd")
	}

	// Startup sweep (once per process): reap terminal orphan workers +
	// their volumes left by a prior SIGKILLed run before launching a
	// new one. Best-effort; never blocks the launch.
	startupSweep(runtime)

	// Rootless gVisor auto-adapt (scoped to enso's OWN invocation — we
	// never touch the user's containers.conf): rootless podman can't
	// use the systemd cgroup manager without an interactive polkit
	// session, and rootless runsc can't configure cgroups at all. So
	// when a gVisor runtime is requested rootless, run podman with the
	// cgroupfs manager and point --runtime at a private wrapper that
	// adds `runsc --ignore-cgroups`. Best-effort: if the wrapper can't
	// be written we fall through to plain runsc (podman will error,
	// and StartupDiagnostic explains the fix).
	effRuntime := b.OCIRuntime
	var globalFlags []string
	gvisor := isGvisorRuntime(effRuntime)
	if gvisor && os.Geteuid() != 0 {
		if real, lerr := exec.LookPath(effRuntime); lerr == nil {
			// A sealed box (podman --network none) hands runsc the root
			// netns, which gVisor refuses ("cannot run with network
			// enabled in root network namespace") unless its own
			// netstack is also disabled. In egress mode the container
			// has a real (slirp) netns, so runsc must keep its netstack
			// to reach the proxy — don't disable it there.
			sealed := b.EgressProxy == "" && (b.Network == "" || b.Network == "none")
			if wp, werr := ensureRootlessRunscWrapper(real, sealed); werr == nil {
				effRuntime = wp
				globalFlags = []string{"--cgroup-manager=cgroupfs"}
			}
		}
	}

	name := containerName(spec.Cwd, spec.TaskID)
	argv := append(append([]string{}, globalFlags...),
		b.buildRunArgs(name, spec.TaskID, exe, spec.Cwd, effRuntime)...)

	// Not CommandContext: shutdown is an ordered Teardown (close the
	// Channel → worker winds down → --rm reaps the container), not an
	// abrupt mid-frame kill.
	cmd := exec.Command(runtime, argv...)
	// Tee podman/runtime stderr to the user AND a bounded buffer so a
	// box that never comes up yields an actionable error (not a bare
	// EOF). 8 KiB is plenty for an OCI-runtime failure.
	diag := &ringBuffer{max: 8 << 10}
	cmd.Stderr = io.MultiWriter(os.Stderr, diag)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("podman: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("podman: stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("podman: start %s: %w", runtime, err)
	}

	w := &podmanWorker{cmd: cmd, runtime: runtime, name: name, diag: diag, gvisor: gvisor}
	w.ch = backend.NewStreamChannelRW(stdout, stdin, &pipePair{stdin, stdout})
	return w, nil
}

// ringBuffer keeps the last max bytes written to it (concurrent-safe);
// used to retain the tail of podman/runtime stderr for diagnostics.
type ringBuffer struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (r *ringBuffer) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, p...)
	if len(r.buf) > r.max {
		r.buf = r.buf[len(r.buf)-r.max:]
	}
	return len(p), nil
}

func (r *ringBuffer) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.TrimSpace(string(r.buf))
}

// isGvisorRuntime reports whether an OCI-runtime selector names gVisor
// (the bare "runsc" the config maps to, or a path to it).
func isGvisorRuntime(rt string) bool {
	return rt != "" && filepath.Base(rt) == "runsc"
}

// ensureRootlessRunscWrapper writes (idempotently) a tiny wrapper under
// enso's own runtime dir that execs the real runsc with
// --ignore-cgroups, and returns its path. This is an enso-owned
// artifact — it does not modify the user's podman configuration.
func ensureRootlessRunscWrapper(realRunsc string, sealed bool) (string, error) {
	dir, err := paths.RuntimeDir()
	if err != nil {
		return "", err
	}
	wdir := filepath.Join(dir, "podman")
	if err := os.MkdirAll(wdir, 0o755); err != nil {
		return "", err
	}
	flags := "--ignore-cgroups"
	name := "runsc-rootless-net"
	if sealed {
		// No container netns → gVisor must also disable its netstack.
		flags += " --network=none"
		name = "runsc-rootless-sealed"
	}
	wp := filepath.Join(wdir, name)
	script := "#!/bin/sh\n" +
		"# Generated by enso. Rootless runsc cannot configure cgroups\n" +
		"# (--ignore-cgroups); a sealed box has no netns (--network=none).\n" +
		"exec " + realRunsc + " " + flags + " \"$@\"\n"
	if cur, _ := os.ReadFile(wp); string(cur) != script {
		if err := os.WriteFile(wp, []byte(script), 0o755); err != nil {
			return "", err
		}
	}
	return wp, nil
}

// buildRunArgs is the pure `podman run …` argv builder (unit-tested
// without a runtime). It encodes the isolation posture:
//
//   - one filesystem namespace: the project is mounted at its REAL
//     path and is the cwd — never /work;
//   - workspace rollback: with Overlay the project mount gets podman's
//     `:O` ephemeral overlay, so every write is discarded by `--rm`;
//   - credential scrub: NO host environment is forwarded (podman does
//     not inherit it; we add only explicit, opt-in -e pairs). The
//     model endpoint/key are not here at all — inference is
//     host-proxied and the config was scrubbed before it crossed;
//   - egress: sealed (`--network none`) unless an allowlist proxy is
//     wired, in which case the box gets exactly HTTPS_PROXY/HTTP_PROXY
//     and nothing else.
func (b *Backend) buildRunArgs(name, taskID, exe, cwd, ociRuntime string) []string {
	src := cwd
	if b.MountSource != "" {
		src = b.MountSource // host-side throwaway copy (workspace overlay)
	}
	mount := src + ":" + cwd

	network := b.Network
	if network == "" {
		network = "none" // sealed by default
	}
	if b.EgressProxy != "" && (network == "" || network == "none") {
		// A proxy on host loopback is unreachable from a sealed netns.
		// slirp4netns with allow_host_loopback exposes the host's
		// 127.0.0.1 to the container at the slirp gateway (10.0.2.2),
		// so the allowlist proxy becomes the box's ONLY route out.
		network = "slirp4netns:allow_host_loopback=true"
	}

	args := []string{"run", "--rm", "-i"}
	if ociRuntime != "" {
		args = append(args, "--runtime", ociRuntime) // e.g. gVisor's runsc (or its rootless wrapper)
	}
	args = append(args,
		"--name", name,
		"--label", "enso.managed=true",
		"--label", "enso.task="+taskID,
		"--label", "enso.created="+strconv.FormatInt(time.Now().Unix(), 10),
		"--network", network,
		"-v", mount,
		"-w", cwd,
		"-v", exe+":/usr/local/bin/enso:ro",
	)
	if b.UID != "" {
		args = append(args, "--user", b.UID)
	}
	for _, m := range b.ExtraMounts {
		args = append(args, "-v", m)
	}
	if b.EgressProxy != "" {
		// The proxy binds host loopback; inside the container that is
		// reachable at the slirp gateway, so the env must point there,
		// not at 127.0.0.1 (which is the container's own loopback).
		pu := containerProxyURL(b.EgressProxy)
		args = append(args,
			"-e", "HTTPS_PROXY="+pu,
			"-e", "HTTP_PROXY="+pu,
			"-e", "https_proxy="+pu,
			"-e", "http_proxy="+pu,
			// Never proxy the in-container loopback / the gateway itself.
			"-e", "NO_PROXY=127.0.0.1,localhost,"+slirpGatewayIP,
			"-e", "no_proxy=127.0.0.1,localhost,"+slirpGatewayIP,
			// The HTTPS_PROXY env is only advisory — a model can unset it
			// and dial slirp's NAT (full internet) or the host loopback at
			// the gateway directly. NET_ADMIN lets the entrypoint install a
			// packet-layer default-deny in the container netns so the proxy
			// is the ONLY route out at L3, not by convention. See the seal
			// in the entrypoint below.
			"--cap-add", "NET_ADMIN",
			// The privileged entrypoint applies the seal and then drops
			// NET_ADMIN (setpriv, see entrypointScript) before exec'ing the
			// untrusted worker, so the worker cannot flush the ENSO_EGRESS
			// chain and dial around the proxy. no-new-privileges makes that
			// one-way: a setuid binary in the image can never re-acquire the
			// dropped capability.
			"--security-opt", "no-new-privileges",
		)
	}
	for _, e := range b.Env { // explicit opt-in only; never host env
		args = append(args, "-e", e)
	}
	if entry := b.entrypointScript(); entry != "" {
		// No persistent container to exec into — the container IS the
		// worker. Wrap the prep (stderr-redirected so it can't corrupt the
		// framed Channel on stdout; `set -e` so a failure aborts before the
		// worker), then exec the worker as the container's process so
		// signals/teardown still target it directly.
		args = append(args, b.Image, "sh", "-c", entry)
	} else {
		args = append(args, b.Image, "/usr/local/bin/enso", "__worker")
	}
	return args
}

// entrypointScript builds the `sh -c` program run before the worker, or
// "" when neither egress sealing nor user init is needed (the worker is
// then exec'd directly). Ordering is deliberate and security-critical:
//
//		set -e
//		{ <ensure ip[6]tables+setpriv> ; <user init> ; <apply seal> } 1>&2
//		exec setpriv --bounding-set -net_admin --no-new-privs enso __worker
//
//	  - tooling-install and user init run FIRST, while slirp still has full
//	    egress, so package fetches work; both are operator-controlled (image
//	    + config), the same trust level as the allowlist itself;
//	  - the seal is applied LAST, immediately before exec — so the ONLY
//	    thing that ever runs with the box sealed is the untrusted worker;
//	  - `set -e` makes the seal fail-closed: if NET_ADMIN is missing, the
//	    image has no iptables/setpriv, or a rule fails, the script aborts
//	    and the worker never starts (a visible failure, never a silent open
//	    box).
//
// Privilege drop (security-critical): the seal needs CAP_NET_ADMIN to
// program netfilter, but the worker does NOT. Were the worker to inherit
// NET_ADMIN it could `iptables -F ENSO_EGRESS` and dial straight out
// through slirp's NAT, bypassing the allowlist proxy. So the entrypoint
// applies the seal with full privilege and then execs the worker through
// `setpriv --bounding-set -net_admin --no-new-privs`, which removes
// NET_ADMIN from the bounding set (a uid-0 process's permitted set is
// re-derived from the bounding set on execve, so this actually strips the
// capability) without changing uid — the bind-mounted workdir, owned by
// the host user and mapped to container root under the rootless userns,
// stays writable, and the read-only exestage binary stays executable.
// Paired with `--security-opt no-new-privileges` on the run command the
// drop is irreversible.
//
// The seal needs root (uid 0) in the container to program netfilter;
// rootless podman's default container user IS root, so the common path
// works. A non-root [backend.podman] uid in egress mode trips the
// fail-closed abort rather than running unsealed.
func (b *Backend) entrypointScript() string {
	sealHostport := ""
	if b.EgressProxy != "" {
		if u, err := url.Parse(containerProxyURL(b.EgressProxy)); err == nil {
			sealHostport = u.Host
		}
	}
	if sealHostport == "" && len(b.Init) == 0 {
		return "" // nothing to do — direct exec
	}

	var s strings.Builder
	s.WriteString("set -e\n{\n")
	if sealHostport != "" {
		// Ensure the firewall tooling AND setpriv exist before sealing.
		// Best-effort apk (Alpine base) only when absent, so an image that
		// bakes them in skips the per-task fetch; if anything is genuinely
		// missing the seal/drop below fail under `set -e` → fail-closed.
		// setpriv (util-linux) performs the post-seal NET_ADMIN drop.
		s.WriteString("command -v iptables >/dev/null 2>&1 && command -v ip6tables >/dev/null 2>&1 || apk add --no-cache iptables ip6tables\n")
		// The cap drop needs util-linux setpriv (--bounding-set). Alpine's
		// BusyBox ships a setpriv APPLET that satisfies `command -v` but
		// lacks --bounding-set, so probe for the flag itself and apk-install
		// the real binary (which shadows the applet at /bin/setpriv) when
		// absent. Fail-closed under `set -e` if it cannot be provided.
		s.WriteString("setpriv --help 2>&1 | grep -q -- --bounding-set || apk add --no-cache setpriv\n")
	}
	for _, ln := range b.Init {
		s.WriteString(ln)
		s.WriteString("\n")
	}
	if sealHostport != "" {
		// Applied AFTER init so init keeps its egress; the worker (next
		// line) is the only thing that runs sealed.
		s.WriteString(seal.Rules(sealHostport))
	}
	worker := "/usr/local/bin/enso __worker"
	if sealHostport != "" {
		// Drop NET_ADMIN before handing control to the untrusted worker so
		// it cannot unseal the netns (see the doc comment). Only in egress
		// mode, where the run command granted NET_ADMIN; the fully-sealed
		// (--network none) path never gets the capability, so there is
		// nothing to drop.
		worker = "setpriv --bounding-set -net_admin --no-new-privs " + worker
	}
	s.WriteString("} 1>&2\nexec " + worker + "\n")
	return s.String()
}

type pipePair struct{ stdin, stdout io.Closer }

func (p *pipePair) Close() error {
	_ = p.stdout.Close()
	return p.stdin.Close()
}

type podmanWorker struct {
	cmd     *exec.Cmd
	runtime string
	name    string
	ch      backend.Channel
	once    sync.Once
	diag    *ringBuffer
	gvisor  bool
}

func (w *podmanWorker) Channel() backend.Channel { return w.ch }

// StartupDiagnostic explains why the box never came up: the tail of
// podman/runtime stderr, plus — when a gVisor runtime is in play —
// the rootless remediation, since that is by far the most common
// gVisor failure. Empty when there's nothing captured.
func (w *podmanWorker) StartupDiagnostic() string {
	if w.diag == nil {
		return ""
	}
	// The channel EOFs the instant the box dies, often a beat before
	// podman/runtime stderr is flushed into the buffer. A brief settle
	// (error path only) makes the captured tail the actual error
	// instead of empty.
	time.Sleep(750 * time.Millisecond)
	tail := w.diag.String()
	if tail == "" && !w.gvisor {
		return ""
	}
	var b strings.Builder
	if tail != "" {
		b.WriteString("podman/runtime said:\n  ")
		b.WriteString(strings.ReplaceAll(tail, "\n", "\n  "))
		b.WriteString("\n")
	}
	if w.gvisor {
		b.WriteString(
			"\ngVisor (runsc) under rootless podman is finicky. enso already\n" +
				"runs it with --cgroup-manager=cgroupfs and a private\n" +
				"runsc --ignore-cgroups wrapper. If it still fails, the host\n" +
				"usually needs: cgroup v2, runsc installed for this user, and\n" +
				"the kernel allowing unprivileged userns (sysctl\n" +
				"kernel.unprivileged_userns_clone=1). To run without gVisor,\n" +
				"unset [backend.podman] hardening. See docs: Sandbox →\n" +
				"gVisor hardening.")
	}
	return strings.TrimSpace(b.String())
}

func (w *podmanWorker) Wait(ctx context.Context) error {
	done := make(chan error, 1)
	go func() { done <- w.cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Teardown closes the Channel (EOF → worker winds down → `--rm` reaps
// the container and its writable layer), then force-removes the
// container by name as a backstop for crash paths where `--rm` didn't
// fire. Idempotent; safe after Wait. (Overlay/volume reclamation and
// the orphan sweep are handled by Sweep.)
func (w *podmanWorker) Teardown(ctx context.Context) error {
	w.once.Do(func() {
		_ = w.ch.Close()
		if w.cmd.Process != nil {
			_ = w.cmd.Process.Kill()
			_ = w.cmd.Wait()
		}
		// Best-effort: --rm usually already removed it. -v also drops
		// the worker's anonymous volumes (the workspace
		// volumes) so Teardown owns the full reclamation.
		rmCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = exec.CommandContext(rmCtx, w.runtime, "rm", "-f", "-v", w.name).Run()
	})
	return nil
}

// containerName is enso-<sanitized-base>-<taskid>: per-task (concurrent
// tasks on one project cannot collide) and label-safe.
func containerName(cwd, taskID string) string {
	base := sanitizeName(filepath.Base(cwd))
	if base == "" {
		base = "project"
	}
	id := sanitizeName(taskID)
	if len(id) > 16 {
		id = id[:16]
	}
	return "enso-" + base + "-" + id
}

// runtimeAvailable reports whether a hardened OCI runtime binary
// (e.g. "runsc") is on PATH. podman would also error if the runtime is
// declared but missing/misconfigured; checking up front lets Start
// fail with an actionable install hint instead of a raw podman error.
func runtimeAvailable(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// slirpGatewayIP is the well-known slirp4netns gateway. With
// allow_host_loopback=true the container reaches the host's 127.0.0.1
// at this address (same convention as QEMU user-mode networking).
const slirpGatewayIP = "10.0.2.2"

// containerProxyURL rewrites a host-loopback proxy URL to the address
// the sealed container actually reaches it on. Host bind stays
// loopback (the proxy is host-private); only the in-container env is
// translated. A non-loopback host (operator pointed EgressProxy at a
// real address) is passed through unchanged.
func containerProxyURL(hostURL string) string {
	u, err := url.Parse(hostURL)
	if err != nil {
		return hostURL
	}
	h := u.Hostname()
	if h != "127.0.0.1" && h != "localhost" && h != "::1" {
		return hostURL
	}
	host := slirpGatewayIP
	if p := u.Port(); p != "" {
		host += ":" + p
	}
	u.Host = host
	return u.String()
}

func sanitizeName(s string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '.':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 32 {
		out = out[:32]
	}
	return out
}
