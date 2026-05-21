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
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/TaraTheStar/enso/internal/backend"
	"github.com/TaraTheStar/enso/internal/backend/exestage"
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

	// Template selects the guest IMAGE, not a full Lima template. A
	// bare distro name ("alpine", "debian", "ubuntu") resolves to the
	// image-only sub-template template:_images/<name> — deliberately
	// NOT template:<name>, whose base chain pulls template:_default/
	// mounts and would bind host $HOME into the guest. Empty →
	// "alpine". A path or URL is used verbatim as the YAML `base:`
	// (advanced: the user then owns the mount posture / iptables).
	// Extra guest packages belong in [backend.lima] init, not a custom
	// template.
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

	// Init are shell script lines run once during VM provisioning,
	// rendered into the generated instance YAML's `provision:` block
	// (mode: system, so they run as root before the worker starts).
	// The podman `init` analogue — the place to install toolchains the
	// base template lacks. Empty → no provision block.
	Init []string

	// Sealed makes the guest network egress default-deny: a per-task
	// firewall (Start → sealGuestEgress) drops all new outbound from the
	// guest except loopback, established/related (the host's inbound
	// limactl-shell SSH return path — the Channel), and the host egress
	// proxy when one is wired. Set by host.SelectBackend for the lima
	// backend. With it set, a `bash` `curl https://example.com` fails
	// the same way it does in a default `--network none` podman box;
	// inference is unaffected (host-proxied over the stdio Channel, not
	// the guest network). If the seal cannot be applied, Start REFUSES
	// rather than run a box that claims to be sealed but isn't.
	Sealed bool

	// EgressProxy, when set, is the host loopback proxy URL. It is
	// translated to the Lima host gateway (192.168.5.2) and injected
	// into the worker as HTTPS_PROXY/HTTP_PROXY, and the firewall opens
	// exactly that gateway:port — making the allowlist proxy the box's
	// only route out. Empty keeps the box fully sealed (no egress).
	EgressProxy string
}

// limaHostGateway is the address the guest reaches the HOST on in
// Lima's default user-mode network (QEMU slirp / VZ). It is hardcoded
// in Lima itself (pkg/networks/const.go: SlirpGateway, subnet
// 192.168.5.0/24, intentionally non-configurable per instance). Host
// loopback services bound on 127.0.0.1:PORT are reachable from the
// guest at this address:PORT — the Lima analogue of podman's slirp
// 10.0.2.2. Using the literal IP (not host.lima.internal) is
// deliberate: a sealed guest cannot reach Lima's DNS to resolve the
// name.
const limaHostGateway = "192.168.5.2"

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
	// Exec an IMMUTABLE content-addressed snapshot, never the host's
	// live build output: a host rebuild overwriting the original in
	// place would corrupt a running guest's mmap (invalid runtime
	// symbol table). We 9p-mount the STABLE snapshot ROOT (exeMount,
	// constant across rebuilds) read-only and exec the content-addressed
	// path WITHIN it. The mount is in the drift-keyed YAML; the exec
	// path is only the shell argv — so a rebuild does NOT drift-recreate
	// the persistent VM (a full ~10s cold boot per `make build`). The
	// new binary still reaches the guest: it appears as a fresh <hash>
	// subdir under the already-mounted root and the next shell execs it.
	var exeMount string
	if exe, exeMount, err = exestage.Stage(exe); err != nil {
		return nil, fmt.Errorf("lima: stage executable: %w", err)
	}
	if spec.Cwd == "" {
		return nil, fmt.Errorf("lima: empty cwd")
	}

	// Reap orphan enso VMs (host reboot / SIGKILL) once per process
	// before bringing one up.
	startupSweep(limactl)

	name := vmName(spec.Cwd)

	if err := b.ensureRunning(ctx, limactl, name, spec.Cwd, exeMount); err != nil {
		return nil, err
	}

	// Seal the guest egress BEFORE launching the worker. Re-applied
	// every task: the persistent per-project VM outlives any single
	// host proxy instance (a fresh loopback port each process), so the
	// firewall's allowed proxy port must be refreshed to match. A guest
	// without iptables, or any other failure, REFUSES the launch — a
	// box that cannot be sealed must never run while the prompt claims
	// it is (the Phase-6 honesty invariant).
	proxyURL := ""
	if b.Sealed {
		proxyURL = guestProxyURL(b.EgressProxy)
		fmt.Fprintln(os.Stderr, "enso: sealing the guest network…")
		if err := sealGuestEgress(ctx, limactl, name, proxyURL); err != nil {
			return nil, fmt.Errorf(
				"lima: could not seal guest egress on %q: %w — refusing to "+
					"run a VM that is not sealed while isolation claims it is. "+
					"The guest needs iptables: enso bootstraps it on the default "+
					"Alpine image, but a custom path/URL [backend.lima] template "+
					"must provide it (or set [backend] type to \"podman\"/\"local\").",
				name, err)
		}
	}

	fmt.Fprintln(os.Stderr, "enso: Lima VM ready — starting the agent…")

	// Not CommandContext: shutdown is an ordered Teardown (close the
	// Channel → worker winds down), not an abrupt mid-frame kill. The
	// persistent VM is NOT stopped per task (only GC stops/deletes it).
	cmd := exec.Command(limactl, buildShellArgs(name, spec.Cwd, exe, proxyURL)...)
	// Own process group: `limactl shell` forks an ssh child, and a bare
	// Kill() of the limactl leader (the old Teardown) left that ssh
	// orphaned — it kept the SSH session into the VM open, so the
	// in-guest worker never EOFed, the VM stayed busy, and the user's
	// terminal saw a lingering process until the VM was manually
	// stopped. A dedicated group lets Teardown signal limactl+ssh
	// together and decouples them from a terminal-close SIGHUP to enso.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	diag := &ringBuffer{max: 8 << 10}
	// Own the stderr pipe; do NOT hand os/exec an io.Writer for it.
	// With a non-*os.File Stderr, os/exec spawns a copy goroutine and
	// cmd.Wait() blocks until that goroutine returns. Lima's ssh
	// ControlMaster persists past `limactl shell` (ssh.config sets
	// ControlPersist) as a backgrounded `ssh.sock [mux]` in its own
	// session: Teardown's process-group kill cannot reap it (it's
	// Lima's — it dies on `enso prune`/VM stop, by design). That
	// surviving mux inherits and holds the stderr-pipe write end open
	// forever, so the copy goroutine never EOFs, cmd.Wait() never
	// returns, Teardown's <-done blocks, and enso (hence the user's
	// terminal) can't exit. Passing an *os.File write end hands the fd
	// to the child directly with NO join goroutine, so Wait returns
	// when limactl exits regardless of the lingering mux. We copy the
	// read end ourselves and close it in Teardown to end that goroutine
	// (it won't EOF on its own — the mux still holds the write end).
	errR, errW, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("lima: stderr pipe: %w", err)
	}
	cmd.Stderr = errW

	stdin, err := cmd.StdinPipe()
	if err != nil {
		errR.Close()
		errW.Close()
		return nil, fmt.Errorf("lima: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		errR.Close()
		errW.Close()
		return nil, fmt.Errorf("lima: stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		errR.Close()
		errW.Close()
		return nil, fmt.Errorf("lima: start shell: %w", err)
	}
	// The child (and any ssh master it forks) now holds the write end;
	// drop ours so a stuck reader is never us keeping the pipe alive.
	_ = errW.Close()
	go func() { _, _ = io.Copy(io.MultiWriter(os.Stderr, diag), errR) }()

	w := &limaWorker{cmd: cmd, diag: diag, errR: errR}
	w.ch = backend.NewStreamChannelRW(stdout, stdin, &pipePair{stdin, stdout})
	return w, nil
}

// ensureRunning brings the per-project VM to a running state with the
// correct mounts: created (from generated YAML) if absent, resumed if
// stopped, left alone if already running.
func (b *Backend) ensureRunning(ctx context.Context, limactl, name, cwd, exeMount string) error {
	dir, err := paths.RuntimeDir()
	if err != nil {
		return fmt.Errorf("lima: runtime dir: %w", err)
	}
	ldir := filepath.Join(dir, "lima")
	yp := filepath.Join(ldir, name+".yaml")
	desired := b.buildVMConfig(cwd, exeMount)

	status := vmStatus(ctx, limactl, name)
	if status != "" && configDrifted(yp, desired) {
		// The per-project VM's mounts/image/provisioning no longer
		// match the effective config (workspace toggled, lima settings
		// changed, or the enso mount layout itself changed across an
		// upgrade). The generated config is authoritative ("regenerated
		// per project"), so REBUILD rather than silently run a stale VM
		// whose project mount may be wrong or empty. Safe to destroy:
		// the workspace overlay keeps project state on the host stage
		// dir, and the VM is reproducible from init/provisioning.
		fmt.Fprintf(os.Stderr,
			"enso: lima VM %q config changed — recreating it to apply the new settings\n", name)
		_ = exec.CommandContext(ctx, limactl, "stop", "--force", name).Run()
		_ = exec.CommandContext(ctx, limactl, "delete", "--force", name).Run()
		status = ""
	}

	switch status {
	case "Running":
		return nil
	case "": // absent (or just deleted for drift) → create from generated YAML
		if err := os.MkdirAll(ldir, 0o755); err != nil {
			return fmt.Errorf("lima: mkdir %s: %w", ldir, err)
		}
		if err := os.WriteFile(yp, []byte(desired), 0o644); err != nil {
			return fmt.Errorf("lima: write VM config: %w", err)
		}
		fmt.Fprintf(os.Stderr,
			"enso: creating Lima VM %q — first run downloads the Alpine image and "+
				"provisions the guest; this can take a few minutes…\n", name)
		return runLimactl(ctx, limactl, buildStartArgs(name, yp))
	default: // Stopped / recoverable, config matches → resume in place
		fmt.Fprintf(os.Stderr, "enso: resuming Lima VM %q…\n", name)
		return runLimactl(ctx, limactl, []string{"start", "--tty=false", name})
	}
}

// configDrifted reports whether the VM's last-written generated config
// differs from what enso would generate now. A missing/unreadable
// stored YAML counts as drift: we cannot prove a match, and rebuilding
// from the current config is the safe, correct action for a VM that is
// "regenerated per project".
func configDrifted(yp, desired string) bool {
	data, err := os.ReadFile(yp)
	if err != nil {
		return true
	}
	return string(data) != desired
}

// capWriter keeps only the last max bytes written — a bounded tail for
// an error message, without unbounded buffering of (potentially MBs of)
// image-download progress.
type capWriter struct {
	max int
	buf []byte
}

func (w *capWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	if len(w.buf) > w.max {
		w.buf = w.buf[len(w.buf)-w.max:]
	}
	return len(p), nil
}

// runLimactl runs a limactl lifecycle command. Its output is STREAMED
// live to stderr (Lima's own image-download / boot / provisioning
// progress — otherwise a cold first run looks hung for minutes), and a
// bounded tail is kept so a failure still yields an actionable error.
// The TUI has not started yet at bring-up time, so stderr is the user's
// terminal; json stdout stays clean.
func runLimactl(ctx context.Context, limactl string, args []string) error {
	c := exec.CommandContext(ctx, limactl, args...)
	tail := &capWriter{max: 8 << 10}
	c.Stdout = io.MultiWriter(os.Stderr, tail)
	c.Stderr = c.Stdout
	// Setsid: run limactl (and the long-lived hostagent it daemonizes)
	// in its OWN session with NO controlling terminal. `limactl start`
	// forks the persistent-VM `limactl hostagent`, which — by design
	// for a persistent VM — outlives this call AND enso. Lima does not
	// fully detach it: the hostagent keeps `/dev/tty` (fd 4) of
	// whatever session launched it. Inherited from a plain child of
	// enso, that is the user's shell pts, so the hostagent pins the
	// terminal open and `exit`/terminal-close hangs until the VM is
	// stopped — even though enso itself exited cleanly. A new session
	// means there is no controlling tty to inherit, so the hostagent's
	// open("/dev/tty") finds nothing of ours. Stdout/Stderr are
	// explicitly our own pipes, so Setsid does not affect the live
	// progress stream; stdin is /dev/null so nothing reads the tty.
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if devnull, err := os.Open(os.DevNull); err == nil {
		c.Stdin = devnull
		defer devnull.Close()
	}
	if err := c.Run(); err != nil {
		return fmt.Errorf("lima: %s: %v\n%s", strings.Join(args, " "), err, strings.TrimSpace(string(tail.buf)))
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

// resolveBase maps the Template knob to a Lima `base:` value, and
// reports whether the resolved guest is Alpine. A bare distro name
// selects the IMAGE-ONLY internal sub-template
// (template:_images/<name>) — deliberately NOT template:<name>, whose
// `base:` chain pulls template:_default/mounts and would bind host
// $HOME into the guest. Empty → "alpine". A path/URL is used verbatim
// (advanced; the user then owns the mount posture and must provide
// iptables for the seal).
func resolveBase(tmpl string) (base string, alpine bool) {
	tmpl = strings.TrimSpace(tmpl)
	if tmpl == "" {
		tmpl = "alpine"
	}
	if strings.Contains(tmpl, "://") || strings.HasPrefix(tmpl, "/") {
		return tmpl, false // custom: distro unknown, posture is the user's
	}
	return "template:_images/" + tmpl, strings.EqualFold(tmpl, "alpine")
}

// bootSpeedupScript zeroes the guest bootloader's interactive menu
// wait. Lima's Alpine image ships a ~10s GRUB serial-console countdown;
// patching it (effective NEXT boot) means a persistent VM eats that
// only on its very first cold boot. Purely cosmetic, so every step is
// guarded and the script always exits 0 — it must never fail an
// otherwise-good boot. Idempotent (re-applied each boot, harmless).
// Handles GRUB (grub.cfg + /etc/default/grub) and the extlinux variant
// some Alpine builds use.
const bootSpeedupScript = `if [ -f /boot/grub/grub.cfg ]; then
  sed -i -e 's/^[[:space:]]*set timeout=.*/set timeout=0/' -e 's/^[[:space:]]*set timeout_style=.*/set timeout_style=hidden/' /boot/grub/grub.cfg || true
fi
if [ -f /etc/default/grub ]; then
  sed -i -e 's/^GRUB_TIMEOUT=.*/GRUB_TIMEOUT=0/' -e 's/^GRUB_TIMEOUT_STYLE=.*/GRUB_TIMEOUT_STYLE=hidden/' /etc/default/grub || true
  grep -q '^GRUB_TIMEOUT=' /etc/default/grub || echo 'GRUB_TIMEOUT=0' >> /etc/default/grub
fi
if [ -f /boot/extlinux.conf ]; then
  sed -i -e 's/^TIMEOUT .*/TIMEOUT 0/' -e 's/^PROMPT .*/PROMPT 0/' /boot/extlinux.conf || true
fi
true`

// buildVMConfig is the pure Lima instance-YAML builder (unit-tested
// without limactl). It inherits ONLY an image (template:_images/<distro>
// — no mounts) and pins exactly what isolation/identity requires:
//
//   - one filesystem namespace: MountSource (the per-task overlay copy)
//     mounted WRITABLE at the project's REAL path; the worker cwd is
//     that path — never /work;
//   - the static enso binary's dir mounted READ-ONLY at its real path
//     so the guest execs it 1:1 (no image rebuild);
//   - host $HOME is NOT mounted. The image-only base inherits no `~`
//     mount, so the agent cannot read ~/.ssh, ~/.aws, ~/.config/enso
//     (provider API keys) or sibling repos. With no `~` parent there
//     is also no mount to shadow the writable project, so the old
//     read-only-workspace bug cannot occur — its CAUSE is removed, not
//     worked around;
//   - no containerd — a plain, minimal VM;
//   - sealed: the worker dials no model (inference is host-proxied over
//     the Channel) and the guest egress is firewalled default-deny at
//     launch (Start → sealGuestEgress).
func (b *Backend) buildVMConfig(cwd, exeMount string) string {
	base, alpine := resolveBase(b.Template)

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
	// Mounts: project WRITABLE at its real path + the STABLE enso
	// snapshot ROOT (exestage's exe/ parent) READ-ONLY. The root — not
	// the per-build <hash> dir — is mounted so this YAML is invariant
	// across host rebuilds (no drift-recreate; the guest still execs
	// the fresh content-addressed path within it, via buildShellArgs).
	// No `~`: host $HOME is never exposed (see the doc comment).
	// ExtraMounts are project-declared, read-only.
	sb.WriteString("mounts:\n")
	fmt.Fprintf(&sb, "  - location: %q\n    mountPoint: %q\n    writable: true\n", src, cwd)
	fmt.Fprintf(&sb, "  - location: %q\n    writable: false\n", exeMount)
	for _, m := range b.ExtraMounts {
		fmt.Fprintf(&sb, "  - location: %q\n    writable: false\n", m)
	}
	// Provisioning. Each entry is a `mode: system` script (root, before
	// the worker starts) with `set -e` so a failure aborts loudly:
	//   1. bootloader timeout zero-out — Alpine only. Lima's Alpine
	//      image defaults to a ~10s GRUB serial-console countdown; this
	//      takes effect on the NEXT boot, so combined with the no-drift
	//      mount above the persistent VM boots slow at most ONCE. Purely
	//      cosmetic, so it is best-effort and never fails the boot (its
	//      own entry; cannot strand the seal even if it somehow errors).
	//   2. iptables bootstrap — sealed + Alpine only. The Alpine cloud
	//      image ships no iptables, but Start→sealGuestEgress needs it
	//      or it REFUSES to launch. Ubuntu/Debian images already carry
	//      it, so this is skipped there. It is a separate, earlier step
	//      than user init so a broken user init can't strand the seal.
	//   3. user [backend.lima] init — all lines in one script so a
	//      `cd`/env set carries across them.
	var scripts []string
	if alpine {
		scripts = append(scripts, bootSpeedupScript)
	}
	if b.Sealed && alpine {
		scripts = append(scripts, "apk add --no-cache iptables")
	}
	if len(b.Init) > 0 {
		scripts = append(scripts, strings.Join(b.Init, "\n"))
	}
	if len(scripts) > 0 {
		sb.WriteString("provision:\n")
		for _, s := range scripts {
			sb.WriteString("  - mode: system\n")
			sb.WriteString("    script: |\n")
			sb.WriteString("      #!/bin/sh\n")
			sb.WriteString("      set -e\n")
			for _, phys := range strings.Split(s, "\n") {
				fmt.Fprintf(&sb, "      %s\n", phys)
			}
		}
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
// enso binary is execed at its real (read-only mounted) path. When a
// guest-reachable proxy URL is given the worker is wrapped in `env` so
// HTTPS_PROXY/HTTP_PROXY point at the host egress proxy (its only route
// out) — the Lima analogue of podman's -e proxy injection. Loopback is
// never proxied (the worker dials no model; inference is host-proxied
// over stdio, not the guest network).
func buildShellArgs(name, cwd, exe, proxyURL string) []string {
	args := []string{"shell", "--workdir", cwd, name}
	if proxyURL != "" {
		args = append(args, "env",
			"HTTPS_PROXY="+proxyURL, "HTTP_PROXY="+proxyURL,
			"https_proxy="+proxyURL, "http_proxy="+proxyURL,
			"NO_PROXY=127.0.0.1,localhost", "no_proxy=127.0.0.1,localhost")
	}
	return append(args, exe, "__worker")
}

// guestProxyURL rewrites a host-loopback proxy URL to the address the
// sealed guest actually reaches it on: Lima exposes host 127.0.0.1
// services at the slirp gateway (192.168.5.2), the same way podman's
// containerProxyURL maps to 10.0.2.2. The host bind stays loopback (the
// proxy is host-private); only the in-guest env is translated. A
// non-loopback host (operator pointed EgressProxy at a real address) is
// passed through unchanged. Empty in → empty out (no proxy → no env).
func guestProxyURL(hostURL string) string {
	if hostURL == "" {
		return ""
	}
	u, err := url.Parse(hostURL)
	if err != nil {
		return hostURL
	}
	if h := u.Hostname(); h != "127.0.0.1" && h != "localhost" && h != "::1" {
		return hostURL
	}
	host := limaHostGateway
	if p := u.Port(); p != "" {
		host += ":" + p
	}
	u.Host = host
	return u.String()
}

// sealScript is the pure (unit-tested) guest firewall program: a
// default-deny OUTPUT chain that drops every NEW outbound connection
// the guest originates EXCEPT loopback, established/related (so the
// host's inbound limactl-shell SSH return traffic — the Channel —
// keeps working), and, when a proxy is wired, exactly the host egress
// proxy at the Lima gateway:port. INPUT is untouched (the host must
// still reach guest sshd). Idempotent: it rebuilds its own ENSO_EGRESS
// chain and installs the OUTPUT jump at most once, so re-running it
// every task with a fresh proxy port is safe. `-w` waits for the xtables
// lock; conntrack matches the established return path.
func sealScript(proxyHostport string) string {
	allowProxy := ""
	if proxyHostport != "" {
		host, port := proxyHostport, ""
		if i := strings.LastIndex(proxyHostport, ":"); i >= 0 {
			host, port = proxyHostport[:i], proxyHostport[i+1:]
		}
		if port != "" {
			allowProxy = "iptables -w -A ENSO_EGRESS -p tcp -d " + host +
				" --dport " + port + " -j ACCEPT\n"
		}
	}
	return "set -e\n" +
		"iptables -w -F ENSO_EGRESS 2>/dev/null || iptables -w -N ENSO_EGRESS\n" +
		"iptables -w -A ENSO_EGRESS -o lo -j ACCEPT\n" +
		"iptables -w -A ENSO_EGRESS -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT\n" +
		allowProxy +
		"iptables -w -A ENSO_EGRESS -j REJECT --reject-with icmp-port-unreachable\n" +
		"iptables -w -C OUTPUT -j ENSO_EGRESS 2>/dev/null || iptables -w -I OUTPUT 1 -j ENSO_EGRESS\n"
}

// sealGuestEgress applies sealScript inside the guest as root (Lima's
// default user has passwordless sudo). proxyURL is the already
// gateway-translated value handed to the worker; the firewall opens the
// same host:port. A non-zero exit (no iptables, sudo refused, …)
// propagates so Start can REFUSE — never run unsealed while claiming to
// be sealed.
func sealGuestEgress(ctx context.Context, limactl, name, proxyURL string) error {
	hostport := ""
	if proxyURL != "" {
		if u, err := url.Parse(proxyURL); err == nil {
			hostport = u.Host
		}
	}
	script := sealScript(hostport)
	c := exec.CommandContext(ctx, limactl, "shell", name,
		"sudo", "sh", "-c", script)
	if out, err := c.CombinedOutput(); err != nil {
		return fmt.Errorf("%v\n%s", err, strings.TrimSpace(string(out)))
	}
	return nil
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
	// errR is the read end of the stderr pipe we own (see Start). Closed
	// in Teardown to end the copy goroutine: a persisted ssh mux keeps
	// the write end, so it never EOFs by itself.
	errR *os.File
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
// shell. It signals the worker's whole process GROUP (limactl + its ssh
// child), not just the limactl leader: a bare leader kill orphaned the
// ssh, which held the VM's SSH session open so the in-guest worker
// never EOFed and the terminal saw a lingering process. SIGTERM first
// so limactl/ssh close the session cleanly (in-guest worker EOFs, VM
// goes idle), SIGKILL the group as a backstop. The persistent
// per-project VM is deliberately NOT stopped or deleted here — that is
// its whole point (substrate reuse); VM reclamation is GC's job
// (Sweep / `enso prune`). Idempotent; safe after Wait.
func (w *limaWorker) Teardown(ctx context.Context) error {
	w.once.Do(func() {
		_ = w.ch.Close()
		// End the stderr copy goroutine. The persisted ssh mux still
		// holds the write end, so the read end never EOFs on its own;
		// closing it here unblocks io.Copy. cmd.Stderr is an *os.File,
		// so cmd.Wait() does not itself join on this — it returns once
		// limactl exits, even with the mux still alive.
		if w.errR != nil {
			_ = w.errR.Close()
		}
		p := w.cmd.Process
		if p == nil {
			return
		}
		// Setpgid at Start ⇒ pgid == leader pid; negative pid signals
		// the whole group. ESRCH (already reaped) is benign.
		pgid := p.Pid
		_ = syscall.Kill(-pgid, syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _ = w.cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
			<-done
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
// fresh task would, so a `enso prune` reclaims old superseded
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
