// SPDX-License-Identifier: AGPL-3.0-or-later

package mcp

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	mcptransport "github.com/mark3labs/mcp-go/client/transport"
	mcpproto "github.com/mark3labs/mcp-go/mcp"

	"github.com/TaraTheStar/enso/internal/config"
	"github.com/TaraTheStar/enso/internal/tools"
)

// ServerState is the per-server health surfaced in the sidebar. There is
// no Reconnecting state today because the manager doesn't reconnect —
// failed servers stay failed until enso restarts. Adding the third
// state would mean adding the retry loop, which we deliberately punted
// for v1.
type ServerState int

const (
	StateHealthy ServerState = iota
	StateFailed
)

func (s ServerState) String() string {
	switch s {
	case StateHealthy:
		return "healthy"
	case StateFailed:
		return "failed"
	default:
		return "unknown"
	}
}

// dialTimeout is the budget for connecting + initialising one server.
const dialTimeout = 10 * time.Second

// Server is one connected MCP server, plus the adapted tools it exposes.
type Server struct {
	Name   string
	Client *mcpclient.Client
	Tools  []tools.Tool
}

// serverStatus carries the sidebar-facing state for one server. Kept
// separate from Server so failed entries (which never had a Client to
// hold onto) can still appear in ConfiguredNames / State.
type serverStatus struct {
	state  ServerState
	lastEr string // human-readable failure reason ("" while healthy)
}

// Manager holds connected MCP servers. The agent treats it as opaque after
// `RegisterAll` has been called.
type Manager struct {
	mu sync.RWMutex
	// servers contains successfully-connected entries; failed servers
	// are absent so RegisterAll skips them naturally. Status carries
	// state for *every* configured server, including ones that never
	// connected — that's what the sidebar iterates.
	servers map[string]*Server
	status  map[string]*serverStatus
	// configured preserves the input set of server names. Lets the
	// sidebar render every name (including failures) regardless of
	// connection success.
	configured []string
}

// NewManager constructs an empty manager.
func NewManager() *Manager {
	return &Manager{
		servers: make(map[string]*Server),
		status:  make(map[string]*serverStatus),
	}
}

// Start connects every server in cfg under a per-server timeout. Failures are
// logged and recorded in the status map (so the sidebar can render them as
// ✘ failed) but do not abort startup. Server names are validated up-front
// against the OpenAI-compatible name charset so a typo'd `[mcp.with spaces]`
// block doesn't surface later as an opaque upstream HTTP 400 on the model's
// next tool-call request.
func (m *Manager) Start(ctx context.Context, cfg map[string]config.MCPConfig) {
	// Snapshot configured names up-front so ConfiguredNames returns
	// everything, even servers we refuse to dial because of bad names.
	names := make([]string, 0, len(cfg))
	for name := range cfg {
		names = append(names, name)
	}
	sort.Strings(names)

	m.mu.Lock()
	m.configured = names
	m.mu.Unlock()

	for _, name := range names {
		c := cfg[name]
		if err := validateName(name, maxServerNameLen); err != nil {
			slog.Error("mcp: refusing server with invalid name", "server", name, "err", err)
			m.recordFailure(name, fmt.Errorf("invalid name: %w", err))
			continue
		}
		dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
		srv, err := dial(dialCtx, name, c)
		cancel()
		if err != nil {
			slog.Warn("mcp: server failed to start", "server", name, "err", err)
			m.recordFailure(name, err)
			continue
		}
		// Inject the failure callback into each adapted tool so a
		// transport-level CallTool error during a tool invocation
		// flips the server's sidebar badge to ✘.
		serverName := name
		for _, t := range srv.Tools {
			if mt, ok := t.(*mcpTool); ok {
				mt.onTransportError = func(err error) { m.MarkFailed(serverName, err) }
			}
		}
		m.mu.Lock()
		m.servers[name] = srv
		m.status[name] = &serverStatus{state: StateHealthy}
		m.mu.Unlock()
		slog.Info("mcp: server connected", "server", name, "tools", len(srv.Tools))
	}
}

func (m *Manager) recordFailure(name string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.status[name] = &serverStatus{state: StateFailed, lastEr: shortErr(err)}
}

// shortErr collapses long error chains into a single human-scannable
// line for the sidebar. Per-server rows live in cramped 28-column
// real estate; "context deadline exceeded" beats a multi-line stack.
func shortErr(err error) string {
	s := err.Error()
	if len(s) > 80 {
		s = s[:77] + "…"
	}
	return s
}

// ConfiguredNames returns every server name from the original config,
// sorted, regardless of whether dial succeeded. The sidebar's MCP
// section iterates this so failed-at-startup entries still appear.
func (m *Manager) ConfiguredNames() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, len(m.configured))
	copy(out, m.configured)
	return out
}

// State returns the current state and last failure reason for a
// configured server. Returns (StateHealthy, "") for a server that has
// never been seen (defensive default — the sidebar should only ask
// about names it got from ConfiguredNames).
func (m *Manager) State(name string) (ServerState, string) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	st, ok := m.status[name]
	if !ok {
		return StateHealthy, ""
	}
	return st.state, st.lastEr
}

// MarkFailed records a runtime failure for `name`. Called from the
// adapter's tool-call path when a CallTool error suggests the
// transport is dead (broken pipe, EOF on stdio, connection closed).
// Idempotent — repeated calls keep the most recent reason.
func (m *Manager) MarkFailed(name string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.status[name]; !ok && !contains(m.configured, name) {
		// Don't record state for an unknown name — that would make
		// ConfiguredNames misleading. The mark must come from a tool
		// call routed through a configured server.
		return
	}
	m.status[name] = &serverStatus{state: StateFailed, lastEr: shortErr(err)}
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

// RegisterAll adds every connected server's adapted tools to the registry.
func (m *Manager) RegisterAll(r *tools.Registry) {
	for _, srv := range m.servers {
		for _, t := range srv.Tools {
			r.Register(t)
		}
	}
}

// Servers returns a snapshot of connected servers (for diagnostics / /tools).
func (m *Manager) Servers() map[string]*Server { return m.servers }

// Close shuts every client down. Errors are logged.
func (m *Manager) Close() {
	for name, srv := range m.servers {
		if err := srv.Client.Close(); err != nil {
			slog.Warn("mcp: close", "server", name, "err", err)
		}
	}
}

// dial picks the transport by config shape, opens it, runs the MCP handshake,
// lists the server's tools, and returns a ready-to-use Server. For URL
// transports we try Streamable-HTTP first and fall back to SSE — older
// servers (and a handful of niche ones) only speak the SSE path.
func dial(ctx context.Context, name string, cfg config.MCPConfig) (*Server, error) {
	switch {
	case cfg.Command != "":
		cli, err := openStdio(cfg)
		if err != nil {
			return nil, err
		}
		return finishDial(ctx, name, cli)

	case cfg.URL != "":
		headers := expandHeaders(cfg.Headers)
		// Try Streamable-HTTP first.
		var streamOpts []mcptransport.StreamableHTTPCOption
		if len(headers) > 0 {
			streamOpts = append(streamOpts, mcptransport.WithHTTPHeaders(headers))
		}
		if cli, err := mcpclient.NewStreamableHttpClient(cfg.URL, streamOpts...); err == nil {
			if srv, ferr := finishDial(ctx, name, cli); ferr == nil {
				return srv, nil
			} else {
				slog.Debug("mcp: streamable-http failed, trying SSE",
					"server", name, "url", cfg.URL, "err", ferr)
			}
		}
		var sseOpts []mcptransport.ClientOption
		if len(headers) > 0 {
			sseOpts = append(sseOpts, mcptransport.WithHeaders(headers))
		}
		cli, err := mcpclient.NewSSEMCPClient(cfg.URL, sseOpts...)
		if err != nil {
			return nil, fmt.Errorf("sse %s: %w", cfg.URL, err)
		}
		return finishDial(ctx, name, cli)

	default:
		return nil, fmt.Errorf("must set either `command` (stdio) or `url` (http)")
	}
}

// expandHeaders resolves $VAR / ${VAR} references in each header value
// against ENSO_-prefixed environment variables only (see
// config.ExpandEnsoEnv). Unset and non-prefixed references collapse to
// empty — the blank header makes misconfigured tokens fail visibly at
// the server rather than silently authenticating as anonymous (or,
// worse, with an unrelated host secret).
func expandHeaders(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = config.ExpandEnsoEnv(v)
	}
	return out
}

// finishDial runs the MCP handshake against an already-constructed client
// and wraps the result as a Server. On any failure the client is closed
// before returning the error so callers don't leak the underlying
// transport connection.
func finishDial(ctx context.Context, name string, cli *mcpclient.Client) (*Server, error) {
	if err := cli.Start(ctx); err != nil {
		_ = cli.Close()
		return nil, fmt.Errorf("start: %w", err)
	}

	var initReq mcpproto.InitializeRequest
	initReq.Params.ProtocolVersion = mcpproto.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcpproto.Implementation{Name: "enso", Version: "0.1"}
	if _, err := cli.Initialize(ctx, initReq); err != nil {
		_ = cli.Close()
		return nil, fmt.Errorf("initialize: %w", err)
	}

	listed, err := cli.ListTools(ctx, mcpproto.ListToolsRequest{})
	if err != nil {
		_ = cli.Close()
		return nil, fmt.Errorf("list tools: %w", err)
	}

	srv := &Server{Name: name, Client: cli}
	for _, mt := range listed.Tools {
		// onTransportError is wired by the manager after dial returns
		// (Start does that injection); finishDial doesn't see the
		// manager itself, which keeps this function unit-testable
		// without a Manager.
		t := adaptTool(name, cli, mt, nil)
		if t == nil {
			slog.Warn("mcp: skipping tool with invalid name", "server", name, "tool", mt.Name)
			continue
		}
		srv.Tools = append(srv.Tools, t)
	}
	return srv, nil
}
