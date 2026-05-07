// SPDX-License-Identifier: AGPL-3.0-or-later

package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/TaraTheStar/enso/internal/config"
)

// Manager owns a pool of LSP server processes — one per `[lsp.<name>]`
// config block — spawned lazily on first request. Each server runs its
// JSON-RPC Run loop in a goroutine; the manager wraps it with a Client
// that does didOpen tracking and diagnostic caching.
type Manager struct {
	cwd     string
	configs map[string]config.LSPConfig

	mu      sync.Mutex
	clients map[string]*serverInstance // by config-block name
}

type serverInstance struct {
	name    string
	cfg     config.LSPConfig
	cmd     *exec.Cmd
	client  *Client
	rootURI string

	// initErr captures whether Initialize failed; non-nil means the
	// instance is unusable and should be left alone (don't repeatedly
	// attempt to spawn a broken server).
	initErr error
}

// NewManager constructs a Manager around the parsed `[lsp]` config map
// and the project cwd. Servers are NOT spawned eagerly; the first call
// to ClientFor lazily starts the matching one.
func NewManager(cwd string, configs map[string]config.LSPConfig) *Manager {
	return &Manager{
		cwd:     cwd,
		configs: configs,
		clients: map[string]*serverInstance{},
	}
}

// HasServers reports whether any LSP server is configured. Callers use
// this to decide whether to register the lsp_* tools at all.
func (m *Manager) HasServers() bool { return m != nil && len(m.configs) > 0 }

// ConfiguredNames returns the names of configured LSP servers (sorted)
// regardless of whether each one has been spawned yet. Servers are
// spawned lazily on first use, so most are "configured but idle" until
// a matching file is opened.
func (m *Manager) ConfiguredNames() []string {
	if m == nil {
		return nil
	}
	out := make([]string, 0, len(m.configs))
	for name := range m.configs {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// IsRunning reports whether the named LSP server has been spawned and
// completed initialisation successfully. Returns false for unconfigured
// names, lazily-not-yet-started servers, and servers whose Initialize
// returned an error.
func (m *Manager) IsRunning(name string) bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	inst, ok := m.clients[name]
	if !ok {
		return false
	}
	return inst.client != nil && inst.initErr == nil
}

// ClientFor returns the Client that should handle requests about
// `absPath`. The matching server is chosen by file extension; if no
// server matches, returns (nil, nil). If a matching server exists but
// fails to spawn or initialize, returns the error.
func (m *Manager) ClientFor(ctx context.Context, absPath string) (*Client, string, error) {
	if m == nil {
		return nil, "", nil
	}
	name, cfg := m.matchByExtension(absPath)
	if name == "" {
		return nil, "", nil
	}

	m.mu.Lock()
	inst, ok := m.clients[name]
	if !ok {
		inst = &serverInstance{name: name, cfg: cfg}
		m.clients[name] = inst
	}
	m.mu.Unlock()

	if inst.initErr != nil {
		return nil, "", inst.initErr
	}
	if inst.client == nil {
		if err := m.startServer(ctx, inst, absPath); err != nil {
			inst.initErr = err
			return nil, "", err
		}
	}
	return inst.client, inst.rootURI, nil
}

// matchByExtension returns the name + config of the server whose
// extension list contains the file's extension. First match wins.
func (m *Manager) matchByExtension(absPath string) (string, config.LSPConfig) {
	ext := strings.ToLower(filepath.Ext(absPath))
	if ext == "" {
		return "", config.LSPConfig{}
	}
	for name, cfg := range m.configs {
		for _, e := range cfg.Extensions {
			if strings.EqualFold(e, ext) {
				return name, cfg
			}
		}
	}
	return "", config.LSPConfig{}
}

func (m *Manager) startServer(ctx context.Context, inst *serverInstance, absPath string) error {
	cfg := inst.cfg
	if cfg.Command == "" {
		return fmt.Errorf("lsp[%s]: command is empty", inst.name)
	}

	root := findRoot(absPath, cfg.RootMarkers, m.cwd)
	rootURI := pathToURI(root)
	inst.rootURI = rootURI

	cmd := exec.Command(cfg.Command, cfg.Args...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), cfg.Env...)
	cmd.Stderr = &lspStderrTap{name: inst.name}
	setPdeathsig(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("lsp[%s]: stdin: %w", inst.name, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("lsp[%s]: stdout: %w", inst.name, err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("lsp[%s]: start %q: %w", inst.name, cfg.Command, err)
	}
	inst.cmd = cmd

	conn := NewConn(stdin, stdout)
	go func() {
		if err := conn.Run(); err != nil && err != io.EOF {
			slog.Warn("lsp run", "server", inst.name, "err", err)
		}
	}()
	client := NewClient(conn)

	var initOpts json.RawMessage
	if cfg.InitOptions != nil {
		raw, err := json.Marshal(cfg.InitOptions)
		if err != nil {
			return fmt.Errorf("lsp[%s]: marshal init_options: %w", inst.name, err)
		}
		initOpts = raw
	}

	initCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := client.Initialize(initCtx, rootURI, initOpts); err != nil {
		_ = cmd.Process.Kill()
		return fmt.Errorf("lsp[%s]: initialize: %w", inst.name, err)
	}
	inst.client = client
	slog.Info("lsp started", "server", inst.name, "root", root)
	return nil
}

// LanguageID returns the LSP languageId for `absPath`'s server, or "" if
// no server matches. Used by the tools layer when sending didOpen.
func (m *Manager) LanguageID(absPath string) string {
	name, cfg := m.matchByExtension(absPath)
	if name == "" {
		return ""
	}
	if cfg.LanguageID != "" {
		return cfg.LanguageID
	}
	return name
}

// Close shuts every spawned server down: shutdown request, exit
// notification, then kill if it doesn't go quietly. Safe to call
// multiple times.
func (m *Manager) Close() {
	if m == nil {
		return
	}
	m.mu.Lock()
	insts := make([]*serverInstance, 0, len(m.clients))
	for _, inst := range m.clients {
		insts = append(insts, inst)
	}
	m.clients = map[string]*serverInstance{}
	m.mu.Unlock()

	for _, inst := range insts {
		if inst.client == nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = inst.client.Shutdown(ctx)
		cancel()
		if inst.cmd != nil && inst.cmd.Process != nil {
			done := make(chan struct{})
			go func() { _ = inst.cmd.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				_ = inst.cmd.Process.Kill()
			}
		}
	}
}

// findRoot walks up from `path` looking for a directory containing any
// of `markers`. Falls back to `fallback` (typically the project cwd) if
// no marker matches before the filesystem root.
func findRoot(path string, markers []string, fallback string) string {
	if len(markers) == 0 {
		return fallback
	}
	dir := path
	if info, err := os.Stat(path); err == nil && !info.IsDir() {
		dir = filepath.Dir(path)
	}
	for {
		for _, m := range markers {
			if _, err := os.Stat(filepath.Join(dir, m)); err == nil {
				return dir
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return fallback
		}
		dir = parent
	}
}

// pathToURI returns `file://` + an absolute, slash-normalised path.
// Valid only for local files; LSP servers all accept this shape.
func pathToURI(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	abs = filepath.ToSlash(abs)
	if !strings.HasPrefix(abs, "/") {
		// Windows; slap a leading slash so file:///C:/... is well-formed.
		abs = "/" + abs
	}
	u := &url.URL{Scheme: "file", Path: abs}
	return u.String()
}

// URIToPath is the inverse of pathToURI for the local case. Used when
// the model receives a Location and we want to render a relative path
// or feed it back into a Read tool.
func URIToPath(uri string) string {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "file" {
		return uri
	}
	p := u.Path
	if len(p) >= 3 && p[0] == '/' && p[2] == ':' {
		// Windows: strip the leading slash from /C:/...
		p = p[1:]
	}
	return filepath.FromSlash(p)
}

// lspStderrTap forwards LSP stderr lines to slog so they end up in
// ~/.enso/enso.log instead of corrupting the TUI on stderr.
type lspStderrTap struct {
	name string
	buf  []byte
}

func (t *lspStderrTap) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	for {
		idx := -1
		for i, b := range t.buf {
			if b == '\n' {
				idx = i
				break
			}
		}
		if idx < 0 {
			break
		}
		line := strings.TrimRight(string(t.buf[:idx]), "\r")
		t.buf = t.buf[idx+1:]
		if line != "" {
			slog.Debug("lsp stderr", "server", t.name, "line", line)
		}
	}
	return len(p), nil
}
