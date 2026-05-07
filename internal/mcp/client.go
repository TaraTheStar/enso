// SPDX-License-Identifier: AGPL-3.0-or-later

package mcp

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	mcptransport "github.com/mark3labs/mcp-go/client/transport"
	mcpproto "github.com/mark3labs/mcp-go/mcp"

	"github.com/TaraTheStar/enso/internal/config"
	"github.com/TaraTheStar/enso/internal/tools"
)

// dialTimeout is the budget for connecting + initialising one server.
const dialTimeout = 10 * time.Second

// Server is one connected MCP server, plus the adapted tools it exposes.
type Server struct {
	Name   string
	Client *mcpclient.Client
	Tools  []tools.Tool
}

// Manager holds connected MCP servers. The agent treats it as opaque after
// `RegisterAll` has been called.
type Manager struct {
	servers map[string]*Server
}

// NewManager constructs an empty manager.
func NewManager() *Manager { return &Manager{servers: make(map[string]*Server)} }

// Start connects every server in cfg under a per-server timeout. Failures are
// logged and skipped; one bad server does not abort startup. Server names
// are validated up-front against the OpenAI-compatible name charset so a
// typo'd `[mcp.with spaces]` block doesn't surface later as an opaque
// upstream HTTP 400 on the model's next tool-call request.
func (m *Manager) Start(ctx context.Context, cfg map[string]config.MCPConfig) {
	for name, c := range cfg {
		if err := validateName(name, maxServerNameLen); err != nil {
			slog.Error("mcp: refusing server with invalid name", "server", name, "err", err)
			continue
		}
		dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
		srv, err := dial(dialCtx, name, c)
		cancel()
		if err != nil {
			slog.Warn("mcp: server failed to start", "server", name, "err", err)
			continue
		}
		m.servers[name] = srv
		slog.Info("mcp: server connected", "server", name, "tools", len(srv.Tools))
	}
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
		t := adaptTool(name, cli, mt)
		if t == nil {
			slog.Warn("mcp: skipping tool with invalid name", "server", name, "tool", mt.Name)
			continue
		}
		srv.Tools = append(srv.Tools, t)
	}
	return srv, nil
}
