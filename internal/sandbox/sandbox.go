// SPDX-License-Identifier: AGPL-3.0-or-later

package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/TaraTheStar/enso/internal/config"
)

// Config is the runtime configuration for a single project's sandbox.
// Fields mirror the `[bash.sandbox]` TOML block.
type Config struct {
	Runtime      Runtime
	Image        string
	Init         []string
	Network      string
	ExtraMounts  []string
	Env          []string
	Name         string // explicit override; defaults to enso-<basename>-<hash>
	UID          string // optional --user value; auto-filled to current user on Linux
	WorkdirMount string // path inside container where cwd is mounted; defaults to /work
}

// Manager owns a single per-project container. `Ensure` is idempotent —
// callers invoke it before the first Exec. The container is left
// running between enso processes by design.
type Manager struct {
	cfg     Config
	cwd     string
	runtime string

	// Lazily resolved after Ensure().
	containerName string
	hash          string
}

// NewManager validates `cfg` and resolves the runtime. Errors here are
// startup errors — bail out and fall back to host bash, or print and
// quit, depending on caller policy.
func NewManager(cwd string, cfg Config) (*Manager, error) {
	if cwd == "" {
		return nil, errors.New("sandbox: cwd is required")
	}
	if cfg.Image == "" {
		cfg.Image = "alpine:latest"
	}
	if cfg.WorkdirMount == "" {
		cfg.WorkdirMount = "/work"
	}
	rt, err := resolveRuntime(cfg.Runtime)
	if err != nil {
		return nil, fmt.Errorf("sandbox: %w", err)
	}
	m := &Manager{cfg: cfg, cwd: cwd, runtime: rt}
	m.containerName = m.resolveName()
	m.hash = m.computeHash()
	return m, nil
}

// Runtime returns the resolved runtime binary name (podman or docker).
func (m *Manager) Runtime() string { return m.runtime }

// ContainerName returns the name the manager uses for this project.
func (m *Manager) ContainerName() string { return m.containerName }

// Ensure creates the container if it doesn't exist, recreates it if its
// init-hash label doesn't match the current config, and starts it if
// stopped. Idempotent — call as often as you like.
func (m *Manager) Ensure(ctx context.Context, log io.Writer) error {
	state, err := m.inspect(ctx)
	if err != nil {
		return fmt.Errorf("sandbox: inspect: %w", err)
	}

	switch {
	case state == nil:
		// Container missing.
		fmt.Fprintf(log, "sandbox: creating %s (image %s, runtime %s)\n", m.containerName, m.cfg.Image, m.runtime)
		return m.create(ctx, log)
	case state.hash != m.hash:
		// Stale config — recreate.
		fmt.Fprintf(log, "sandbox: %s init-hash changed; recreating\n", m.containerName)
		if err := m.remove(ctx); err != nil {
			return fmt.Errorf("sandbox: rm stale: %w", err)
		}
		return m.create(ctx, log)
	case !state.running:
		// Existing, fresh, but stopped — start it.
		if _, err := runCapture(ctx, m.runtime, "start", m.containerName); err != nil {
			return fmt.Errorf("sandbox: start: %w", err)
		}
		return nil
	default:
		return nil // running, fresh — reuse.
	}
}

// Exec runs `cmd` inside the container. Stdout+stderr stream to `w`.
// Cancellation propagates via ctx — `<runtime> exec` is killed which
// in turn signals the in-container process. The container itself is
// not affected.
func (m *Manager) Exec(ctx context.Context, w io.Writer, cmd string) error {
	args := []string{"exec", "-i", m.containerName, "sh", "-c", cmd}
	return runStreaming(ctx, w, m.runtime, args...)
}

// Stop sends a graceful shutdown to the container without removing it.
// Subsequent enso runs will re-`start` it instantly. Errors are
// non-fatal — best-effort cleanup.
func (m *Manager) Stop(ctx context.Context) {
	if _, err := runCapture(ctx, m.runtime, "stop", "-t", "1", m.containerName); err != nil {
		slog.Debug("sandbox: stop failed", "container", m.containerName, "err", err)
	}
}

// Remove deletes the container outright. Used by `enso sandbox rm` and
// internally when an init-hash mismatch is detected.
func (m *Manager) Remove(ctx context.Context) error { return m.remove(ctx) }

func (m *Manager) remove(ctx context.Context) error {
	_, err := runCapture(ctx, m.runtime, "rm", "-f", m.containerName)
	return err
}

// containerState is what `inspect` returns. nil means the container
// doesn't exist.
type containerState struct {
	running bool
	hash    string // value of the enso.init-hash label, "" if missing
}

func (m *Manager) inspect(ctx context.Context) (*containerState, error) {
	// First check existence with `ps -a --filter`. Returns the ID if
	// present, empty string if not — unambiguous, no error-string
	// matching. Avoids the previous "is this 'no such container' or a
	// real error?" guess that broke under permission errors and varied
	// across runtime locales/versions.
	idOut, err := runCapture(ctx, m.runtime, "ps", "-a",
		"--filter", "name=^"+m.containerName+"$",
		"--format", "{{.ID}}",
	)
	if err != nil {
		return nil, fmt.Errorf("ps: %w", err)
	}
	if strings.TrimSpace(idOut) == "" {
		return nil, nil
	}

	// Container exists — pull full JSON inspect and decode just the
	// fields we need. Replaces the previous custom `;`-separated mini-
	// format that would silently mis-parse if any future label value
	// contained a semicolon.
	raw, err := runCapture(ctx, m.runtime, "inspect", m.containerName)
	if err != nil {
		return nil, fmt.Errorf("inspect: %w", err)
	}
	var arr []struct {
		State struct {
			Running bool `json:"Running"`
		} `json:"State"`
		Config struct {
			Labels map[string]string `json:"Labels"`
		} `json:"Config"`
	}
	if err := json.Unmarshal([]byte(raw), &arr); err != nil {
		return nil, fmt.Errorf("parse inspect json: %w", err)
	}
	if len(arr) == 0 {
		// Race: container disappeared between ps and inspect. Treat as
		// absent so the caller recreates rather than erroring.
		return nil, nil
	}
	return &containerState{
		running: arr[0].State.Running,
		hash:    arr[0].Config.Labels["enso.init-hash"],
	}, nil
}

// create runs the long-lived `sleep infinity` container with all the
// necessary labels + mounts, then runs the init steps inside. On any
// init failure the container is removed so a half-initialised
// container doesn't get reused on the next run.
func (m *Manager) create(ctx context.Context, log io.Writer) error {
	args := []string{
		"run", "-d",
		"--name", m.containerName,
		"--label", "enso.managed=true",
		"--label", "enso.init-hash=" + m.hash,
		"--label", "enso.cwd=" + sanitizeLabelValue(m.cwd),
		"-w", m.cfg.WorkdirMount,
		"-v", fmt.Sprintf("%s:%s", m.cwd, m.cfg.WorkdirMount),
	}
	if u := m.cfg.UID; u != "" {
		args = append(args, "--user", u)
	}
	if n := m.cfg.Network; n != "" {
		args = append(args, "--network", n)
	}
	for _, mnt := range m.cfg.ExtraMounts {
		args = append(args, "-v", mnt)
	}
	for _, env := range m.cfg.Env {
		args = append(args, "-e", env)
	}
	args = append(args, m.cfg.Image, "sleep", "infinity")

	if _, err := runCapture(ctx, m.runtime, args...); err != nil {
		return fmt.Errorf("create: %w", err)
	}

	// Run init lines. Each line is sent as a separate exec invocation
	// so output associates cleanly with its line on failure.
	for _, line := range m.cfg.Init {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fmt.Fprintf(log, "sandbox: init: %s\n", line)
		if err := m.Exec(ctx, log, line); err != nil {
			fmt.Fprintf(log, "sandbox: init failed; removing %s\n", m.containerName)
			_ = m.remove(context.Background())
			return fmt.Errorf("init %q: %w", line, err)
		}
	}
	return nil
}

// resolveName returns the configured Name, or
// `enso-<sanitized basename>-<6-hex of cwd>` for collision-safe
// per-project naming.
func (m *Manager) resolveName() string {
	if m.cfg.Name != "" {
		return m.cfg.Name
	}
	base := sanitizeName(filepath.Base(m.cwd))
	if base == "" {
		base = "project"
	}
	sum := sha256.Sum256([]byte(m.cwd))
	return "enso-" + base + "-" + hex.EncodeToString(sum[:3])
}

// sanitizeName lowercases and reduces a path basename to characters
// container runtimes accept in names: `[a-z0-9_.-]`. Runs of
// non-alnum chars collapse to a single dash; leading/trailing dashes
// are trimmed. Long basenames are truncated for readability.
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

// sanitizeLabelValue makes s safe to embed in a Docker/Podman --label
// value. The runtime accepts arbitrary bytes here, but a control char
// in the cwd (newline, tab, NUL, DEL) would render as gibberish in
// `docker inspect` output and confuse downstream tooling that expects
// labels to be one-line scalars. Replace control bytes with `?` and
// cap at 256 chars — the label is operator-facing, so longer is just
// noise. Defense-in-depth; we never read this label back ourselves.
func sanitizeLabelValue(s string) string {
	const maxLen = 256
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c == 0x7f {
			b.WriteByte('?')
		} else {
			b.WriteByte(c)
		}
	}
	out := b.String()
	if len(out) > maxLen {
		out = out[:maxLen]
	}
	return out
}

// computeHash collapses image + init + mounts + env + network into a
// single hex digest stored on the container as `enso.init-hash`. Any
// change to those fields recreates the container on the next start.
func (m *Manager) computeHash() string {
	h := sha256.New()
	for _, s := range []string{
		m.cfg.Image, m.cfg.Network, m.cfg.WorkdirMount, m.cfg.UID,
	} {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	for _, list := range [][]string{m.cfg.Init, m.cfg.ExtraMounts, m.cfg.Env} {
		for _, s := range list {
			h.Write([]byte(s))
			h.Write([]byte{0})
		}
		h.Write([]byte{1})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// CurrentUserSpec returns "$(id -u):$(id -g)" for the host process,
// suitable for podman/docker `--user`. On platforms where this doesn't
// matter (macOS Docker Desktop) it's still safe to set. Returns "" if
// for any reason we can't read it (Windows, unusual environments).
func CurrentUserSpec() string {
	u, err := exec.Command("id", "-u").Output()
	if err != nil {
		return ""
	}
	g, err := exec.Command("id", "-g").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(u)) + ":" + strings.TrimSpace(string(g))
}

// EnsoManagedContainers lists every container whose `enso.managed=true`
// label is set, regardless of project. Used by `enso sandbox list /
// prune`. Returns names paired with their `enso.cwd` label so the user
// can identify which project each container belongs to.
type Managed struct {
	Name    string
	Cwd     string
	Running bool
	Image   string
}

// FromConfig translates the user-facing TOML shape into the sandbox
// package's Config and reports whether sandboxing is enabled. Returns
// `enabled=false` when `[bash] sandbox` is empty or "off".
func FromConfig(cfg *config.Config) (Config, bool) {
	mode := cfg.Bash.Sandbox
	if mode == "" || mode == "off" {
		return Config{}, false
	}
	opts := cfg.Bash.Sb
	return Config{
		Runtime:      Runtime(mode),
		Image:        opts.Image,
		Init:         opts.Init,
		Network:      opts.Network,
		ExtraMounts:  opts.ExtraMounts,
		Env:          opts.Env,
		Name:         opts.Name,
		WorkdirMount: opts.WorkdirMount,
		UID:          opts.UID,
	}, true
}

// ResolveRuntimeBinary exposes the runtime resolver to other packages
// (e.g. the `enso sandbox prune` subcommand) without forcing them to
// build a full Manager.
func ResolveRuntimeBinary(r Runtime) (string, error) { return resolveRuntime(r) }

// RemoveByName forces removal of a container by name regardless of
// state. Used by `enso sandbox prune` to iterate ListManaged results.
func RemoveByName(ctx context.Context, runtime, name string) error {
	_, err := runCapture(ctx, runtime, "rm", "-f", name)
	return err
}

// ListManaged shells out to the runtime to enumerate enso-managed
// containers. The runtime is auto-resolved (or pass an explicit Runtime
// via the second arg). Returns an empty slice on a missing runtime.
func ListManaged(ctx context.Context, r Runtime) ([]Managed, error) {
	rt, err := resolveRuntime(r)
	if err != nil {
		return nil, err
	}
	out, err := runCapture(ctx, rt,
		"ps", "-a",
		"--filter", "label=enso.managed=true",
		"--format", `{{.Names}}|{{.State}}|{{.Image}}|{{ index .Labels "enso.cwd" }}`,
	)
	if err != nil {
		return nil, err
	}
	var ms []Managed
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "|", 4)
		for len(parts) < 4 {
			parts = append(parts, "")
		}
		ms = append(ms, Managed{
			Name:    parts[0],
			Running: strings.EqualFold(parts[1], "running"),
			Image:   parts[2],
			Cwd:     parts[3],
		})
	}
	return ms, nil
}

// PathInsideContainer maps a host path to its in-container equivalent
// when the host path is under the manager's cwd. Returns "" otherwise.
// Used by tools that want to emit container-relative paths in error
// messages or display.
func (m *Manager) PathInsideContainer(hostPath string) string {
	abs, err := filepath.Abs(hostPath)
	if err != nil {
		return ""
	}
	rel, err := filepath.Rel(m.cwd, abs)
	if err != nil || strings.HasPrefix(rel, "..") {
		return ""
	}
	return filepath.ToSlash(filepath.Join(m.cfg.WorkdirMount, rel))
}
