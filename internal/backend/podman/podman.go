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
	"github.com/TaraTheStar/enso/internal/sandbox"
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
	// resolver `enso sandbox prune` uses, so they cannot disagree).
	Runtime string

	Image       string   // container image; required
	Network     string   // --network value; "" → "none" (sealed)
	ExtraMounts []string // additional -v entries (host:container[:opts])
	Env         []string // -e KEY=VALUE: explicit opt-in only (never host env)
	UID         string   // --user value; "" → runtime default

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
}

func (b *Backend) Name() string { return "podman" }

// Start provisions and launches the container Worker. spec.TaskID names
// the box (enso-<base>-<taskid>); the project dir is the only thing
// mounted in (plus the read-only enso binary) — no host $HOME.
func (b *Backend) Start(ctx context.Context, spec backend.TaskSpec) (backend.Worker, error) {
	runtime, err := sandbox.ResolveRuntimeBinary(sandbox.Runtime(b.Runtime))
	if err != nil {
		return nil, fmt.Errorf("podman: %w", err)
	}
	exe := b.Exe
	if exe == "" {
		if exe, err = os.Executable(); err != nil {
			return nil, fmt.Errorf("podman: resolve executable: %w", err)
		}
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

	name := containerName(spec.Cwd, spec.TaskID)
	args := b.buildRunArgs(name, spec.TaskID, exe, spec.Cwd)

	// Not CommandContext: shutdown is an ordered Teardown (close the
	// Channel → worker winds down → --rm reaps the container), not an
	// abrupt mid-frame kill.
	cmd := exec.Command(runtime, args...)
	cmd.Stderr = os.Stderr // podman progress + worker stderr → user

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

	w := &podmanWorker{cmd: cmd, runtime: runtime, name: name}
	w.ch = backend.NewStreamChannelRW(stdout, stdin, &pipePair{stdin, stdout})
	return w, nil
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
func (b *Backend) buildRunArgs(name, taskID, exe, cwd string) []string {
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

	args := []string{
		"run", "--rm", "-i",
		"--name", name,
		"--label", "enso.managed=true",
		"--label", "enso.task=" + taskID,
		"--label", "enso.created=" + strconv.FormatInt(time.Now().Unix(), 10),
		"--network", network,
		"-v", mount,
		"-w", cwd,
		"-v", exe + ":/usr/local/bin/enso:ro",
	}
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
		)
	}
	for _, e := range b.Env { // explicit opt-in only; never host env
		args = append(args, "-e", e)
	}
	args = append(args, b.Image, "/usr/local/bin/enso", "__worker")
	return args
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
}

func (w *podmanWorker) Channel() backend.Channel { return w.ch }

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
