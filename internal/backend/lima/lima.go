// SPDX-License-Identifier: AGPL-3.0-or-later

// Package lima implements the real-VM isolation Backend: the agent-core
// Worker runs inside a Lima VM. It is a straight port of
// internal/backend/podman onto the same Backend/Worker/Channel seam —
// the seam already does the hard parts (host-proxied inference, HITL,
// control/capability/telemetry, ScrubbedForWorker).
//
// Channel transport is `limactl shell <vm> <enso> __worker`'s stdio,
// identical framing to LocalBackend and PodmanBackend: the host writes
// length-prefixed envelopes to that process's stdin and reads them from
// its stdout (limactl/ssh diagnostics go to stderr, ring-buffered for
// StartupDiagnostic). It is deliberately PTY-free (pipes, not a tty) so
// the binary frame stays clean. No port-forward is needed.
//
// One filesystem namespace by construction: the project (or its
// per-task workspace overlay copy) is mounted into the VM at its REAL
// host path and the worker's cwd is that same path — never /work.
//
// Substrate model (locked, see repo TODO.md §8): a PERSISTENT
// PER-PROJECT VM (enso-<base>-<projecthash>), not a fresh VM per task —
// a cold per-task VM boot is impractical. Per-task *workspace*
// isolation is still total: the host-side workspace overlay copy is
// what gets mounted in. To keep the persistent VM completely static
// (created once, only `limactl start` to resume — the actual perf win,
// zero per-task VM restarts) the overlay lives at a STABLE per-project
// host staging dir (workspace.NewAt) whose contents are refreshed per
// task; the VM's mount config never changes. The carry-forward
// tradeoff (a project's own sequential tasks share the VM userland) is
// documented in docs/content/docs/sandbox.md; the safety-max follow-up
// is a per-task qcow2 snapshot clone.
package lima

import (
	"context"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/TaraTheStar/enso/internal/backend"
	"github.com/TaraTheStar/enso/internal/backend/workspace"
	"github.com/TaraTheStar/enso/internal/paths"
)

// Backend launches each task's Worker inside a persistent per-project
// Lima VM. The zero value is not usable; construct via
// host.SelectBackend (which fills it from config) or set fields
// directly.
type Backend struct {
	// Exe is the host path to a STATIC (CGO_ENABLED=0) enso binary.
	// Its directory is mounted read-only into the VM at the same path,
	// so the guest execs it at exactly this path (one namespace).
	// Empty → the running executable (correct for release builds,
	// which are static).
	Exe string

	// Template is a Lima template: a bare name ("default", "debian")
	// → template://<name>, or a path/URL used verbatim as the YAML
	// `base:`. Empty → "default".
	Template string

	CPUs   int    // VM vCPUs; 0 → Lima default
	Memory string // e.g. "4GiB"; "" → Lima default
	Disk   string // e.g. "20GiB"; "" → Lima default

	// MountSource is the host path mounted at the project's REAL path
	// inside the VM. Empty → the project dir itself (in-place). The
	// workspace overlay sets this to a STABLE per-project throwaway
	// copy (workspace.NewAt) so the real project is untouched while
	// the agent runs and the persistent VM's mount config stays fixed.
	MountSource string

	// ExtraMounts are additional host paths mounted read-only into the
	// VM (Lima `mounts:` entries).
	ExtraMounts []string
}

func (b *Backend) Name() string { return "lima" }

// limactlBin is the Lima CLI; overridable in tests.
var limactlBin = "limactl"

// Start ensures the persistent per-project VM is running, then launches
// the Worker inside it via `limactl shell`.
func (b *Backend) Start(ctx context.Context, spec backend.TaskSpec) (backend.Worker, error) {
	// Fail safe FIRST: limactl absent must REFUSE with an actionable
	// install hint, never silently downgrade isolation.
	limactl, err := exec.LookPath(limactlBin)
	if err != nil {
		return nil, fmt.Errorf(
			"lima: %q not found on PATH — real-VM isolation requires Lima. "+
				"Install it (macOS: `brew install lima`; Linux: see "+
				"https://lima-vm.io/docs/installation/), or set "+
				"[backend] type to \"podman\" or \"local\". "+
				"Refusing to run without the requested isolation.", limactlBin)
	}

	exe := b.Exe
	if exe == "" {
		if exe, err = os.Executable(); err != nil {
			return nil, fmt.Errorf("lima: resolve executable: %w", err)
		}
	}
	exe, err = filepath.Abs(exe)
	if err != nil {
		return nil, fmt.Errorf("lima: abs executable: %w", err)
	}
	if spec.Cwd == "" {
		return nil, fmt.Errorf("lima: empty cwd")
	}

	// Reap orphan enso VMs (host reboot / SIGKILL) once per process
	// before bringing one up.
	startupSweep(limactl)

	name := vmName(spec.Cwd)

	if err := b.ensureRunning(ctx, limactl, name, spec.Cwd, exe); err != nil {
		return nil, err
	}

	// Not CommandContext: shutdown is an ordered Teardown (close the
	// Channel → worker winds down), not an abrupt mid-frame kill. The
	// persistent VM is NOT stopped per task (only GC stops/deletes it).
	cmd := exec.Command(limactl, buildShellArgs(name, spec.Cwd, exe)...)
	diag := &ringBuffer{max: 8 << 10}
	cmd.Stderr = io.MultiWriter(os.Stderr, diag)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("lima: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("lima: stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("lima: start shell: %w", err)
	}

	w := &limaWorker{cmd: cmd, diag: diag}
	w.ch = backend.NewStreamChannelRW(stdout, stdin, &pipePair{stdin, stdout})
	return w, nil
}

// ensureRunning brings the per-project VM to a running state with the
// correct mounts: created (from generated YAML) if absent, resumed if
// stopped, left alone if already running.
func (b *Backend) ensureRunning(ctx context.Context, limactl, name, cwd, exe string) error {
	status := vmStatus(ctx, limactl, name)
	switch status {
	case "Running":
		return nil
	case "": // does not exist → create from generated YAML
		yaml := b.buildVMConfig(cwd, exe)
		dir, err := paths.RuntimeDir()
		if err != nil {
			return fmt.Errorf("lima: runtime dir: %w", err)
		}
		ldir := filepath.Join(dir, "lima")
		if err := os.MkdirAll(ldir, 0o755); err != nil {
			return fmt.Errorf("lima: mkdir %s: %w", ldir, err)
		}
		yp := filepath.Join(ldir, name+".yaml")
		if err := os.WriteFile(yp, []byte(yaml), 0o644); err != nil {
			return fmt.Errorf("lima: write VM config: %w", err)
		}
		return runLimactl(ctx, limactl, buildStartArgs(name, yp))
	default: // Stopped / Broken-but-recoverable → resume in place
		return runLimactl(ctx, limactl, []string{"start", "--tty=false", name})
	}
}

// runLimactl runs a limactl lifecycle command, surfacing its combined
// output on failure (VM bring-up errors are otherwise opaque).
func runLimactl(ctx context.Context, limactl string, args []string) error {
	c := exec.CommandContext(ctx, limactl, args...)
	if out, err := c.CombinedOutput(); err != nil {
		return fmt.Errorf("lima: %s: %v\n%s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// vmStatus returns the Lima status string ("Running"/"Stopped"/…) or ""
// if the instance does not exist.
func vmStatus(ctx context.Context, limactl, name string) string {
	out, err := exec.CommandContext(ctx, limactl,
		"list", "--format", "{{.Status}}", name).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// buildVMConfig is the pure Lima instance-YAML builder (unit-tested
// without limactl). It inherits a base template for the image/defaults
// and pins only what isolation/identity requires:
//
//   - one filesystem namespace: MountSource (the per-task overlay copy)
//     mounted WRITABLE at the project's REAL path; the worker cwd is
//     that path — never /work;
//   - the static enso binary's dir mounted READ-ONLY at its real path
//     so the guest execs it 1:1 (no image rebuild);
//   - no containerd — a plain, minimal VM;
//   - sealed by default: the worker dials no model (inference is
//     host-proxied over the Channel); egress is a documented
//     follow-up, mirroring how podman started.
func (b *Backend) buildVMConfig(cwd, exe string) string {
	tmpl := strings.TrimSpace(b.Template)
	if tmpl == "" {
		tmpl = "default"
	}
	base := tmpl
	if !strings.Contains(tmpl, "://") && !strings.HasPrefix(tmpl, "/") {
		base = "template://" + tmpl
	}

	src := cwd
	if b.MountSource != "" {
		src = b.MountSource
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "# Generated by enso (lima backend). Do not edit; regenerated per project.\n")
	fmt.Fprintf(&sb, "base: %q\n", base)
	if b.CPUs > 0 {
		fmt.Fprintf(&sb, "cpus: %d\n", b.CPUs)
	}
	if b.Memory != "" {
		fmt.Fprintf(&sb, "memory: %q\n", b.Memory)
	}
	if b.Disk != "" {
		fmt.Fprintf(&sb, "disk: %q\n", b.Disk)
	}
	sb.WriteString("containerd:\n  system: false\n  user: false\n")
	sb.WriteString("mounts:\n")
	fmt.Fprintf(&sb, "  - location: %q\n    mountPoint: %q\n    writable: true\n", src, cwd)
	fmt.Fprintf(&sb, "  - location: %q\n    writable: false\n", filepath.Dir(exe))
	for _, m := range b.ExtraMounts {
		fmt.Fprintf(&sb, "  - location: %q\n    writable: false\n", m)
	}
	return sb.String()
}

// buildStartArgs is the pure `limactl start` argv for a NEW instance
// from a generated YAML. --tty=false makes it non-interactive (no
// editor / confirmation prompt) — required for unattended runs.
func buildStartArgs(name, yamlPath string) []string {
	return []string{"start", "--name", name, "--tty=false", yamlPath}
}

// buildShellArgs is the pure `limactl shell` argv that launches the
// worker. --workdir pins the guest cwd to the project's REAL path; the
// enso binary is execed at its real (read-only mounted) path.
func buildShellArgs(name, cwd, exe string) []string {
	return []string{"shell", "--workdir", cwd, name, exe, "__worker"}
}

type pipePair struct{ stdin, stdout io.Closer }

func (p *pipePair) Close() error {
	_ = p.stdout.Close()
	return p.stdin.Close()
}

type limaWorker struct {
	cmd  *exec.Cmd
	ch   backend.Channel
	once sync.Once
	diag *ringBuffer
}

func (w *limaWorker) Channel() backend.Channel { return w.ch }

// StartupDiagnostic explains why the worker never came up: the tail of
// limactl/ssh stderr. Empty when nothing was captured.
func (w *limaWorker) StartupDiagnostic() string {
	if w.diag == nil {
		return ""
	}
	// The channel EOFs the instant the shell dies, often a beat before
	// limactl/ssh stderr is flushed. A brief settle (error path only)
	// makes the captured tail the actual error instead of empty.
	time.Sleep(750 * time.Millisecond)
	tail := w.diag.String()
	if tail == "" {
		return ""
	}
	return "limactl said:\n  " + strings.ReplaceAll(tail, "\n", "\n  ")
}

func (w *limaWorker) Wait(ctx context.Context) error {
	done := make(chan error, 1)
	go func() { done <- w.cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Teardown closes the Channel (EOF → worker winds down) and ends the
// shell. The persistent per-project VM is deliberately NOT stopped or
// deleted here — that is its whole point (substrate reuse). VM
// reclamation is GC's job (Sweep / `enso sandbox prune`). Idempotent;
// safe after Wait.
func (w *limaWorker) Teardown(ctx context.Context) error {
	w.once.Do(func() {
		_ = w.ch.Close()
		if w.cmd.Process != nil {
			_ = w.cmd.Process.Kill()
			_ = w.cmd.Wait()
		}
	})
	return nil
}

// VMName is the exported view of the per-project Lima instance name
// for a given project dir — for tooling/inspection and precise
// cleanup (never a broad Sweep, which would hit a user's other enso
// project VMs).
func VMName(cwd string) string { return vmName(cwd) }

// vmName is enso-<sanitized-base>-<8hex-of-abs-cwd>: persistent and
// PER-PROJECT (not per-task) — the locked substrate decision. The hash
// disambiguates same-basename projects in different paths.
func vmName(cwd string) string {
	abs, err := filepath.Abs(cwd)
	if err != nil {
		abs = cwd
	}
	base := sanitizeName(filepath.Base(abs))
	if base == "" {
		base = "project"
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(abs))
	return fmt.Sprintf("enso-%s-%08x", base, h.Sum32())
}

// StageDir is the STABLE per-project host directory the workspace
// overlay copy lives under for the lima backend (workspace.NewAt
// target). Stable so the persistent VM's mount config never changes;
// keyed by the same project hash as vmName so it tracks the project,
// not the task. Under StateDir (XDG; never ~/.enso).
func StageDir(cwd string) (string, error) {
	sd, err := paths.StateDir()
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		abs = cwd
	}
	return filepath.Join(sd, "lima-ws", strings.TrimPrefix(vmName(abs), "enso-")), nil
}

// SweepStageKept bounds accumulated workspace review copies across
// every project's lima stage dir: it enforces the same keptCap as a
// fresh task would, so a `enso sandbox prune` reclaims old superseded
// merged.kept-* without ever destroying the most recent (possibly
// still-unreviewed) ones. Best-effort.
func SweepStageKept(out io.Writer) {
	sd, err := paths.StateDir()
	if err != nil {
		return
	}
	stages, _ := filepath.Glob(filepath.Join(sd, "lima-ws", "*"))
	for _, s := range stages {
		if fi, e := os.Stat(s); e == nil && fi.IsDir() {
			// Enforce the same cap a fresh task would — reclaim old
			// superseded copies, never the most recent (maybe
			// still-unreviewed) ones.
			workspace.PruneKept(s, workspace.KeptCap, out)
		}
	}
}

// sanitizeName lowercases and reduces to a Lima-safe instance segment
// (RFC1123-ish: [a-z0-9-], no leading/trailing dash), capped.
func sanitizeName(s string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
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
	if len(out) > 24 {
		out = strings.Trim(out[:24], "-")
	}
	return out
}

// ringBuffer keeps the last max bytes written to it (concurrent-safe);
// retains the tail of limactl/ssh stderr for diagnostics.
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
