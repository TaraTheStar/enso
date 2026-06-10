// SPDX-License-Identifier: AGPL-3.0-or-later

package mcp

import (
	"context"
	"strings"
	"testing"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	mcpproto "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/TaraTheStar/enso/internal/tools"
)

// newSlowServer returns an in-process MCP client wired to a server whose
// single "slow" tool blocks until its context is cancelled. finishDial is
// reused so the tool is wrapped exactly as in production.
func newSlowServer(t *testing.T, callTimeout time.Duration) tools.Tool {
	t.Helper()
	srv := mcpserver.NewMCPServer("slow-test", "0.1")
	srv.AddTool(
		mcpproto.NewTool("slow"),
		func(ctx context.Context, _ mcpproto.CallToolRequest) (*mcpproto.CallToolResult, error) {
			<-ctx.Done() // never returns on its own; honours cancellation
			return nil, ctx.Err()
		},
	)

	cli, err := mcpclient.NewInProcessClient(srv)
	if err != nil {
		t.Fatalf("in-process client: %v", err)
	}
	server, err := finishDial(context.Background(), "slow", cli, callTimeout)
	if err != nil {
		t.Fatalf("finishDial: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	if len(server.Tools) != 1 {
		t.Fatalf("want 1 adapted tool, got %d", len(server.Tools))
	}
	return server.Tools[0]
}

// TestMCPTool_CallTimeout asserts a slow MCP tool is abandoned at the
// per-server call_timeout and reported as a normal (non-error) result so
// the turn continues.
func TestMCPTool_CallTimeout(t *testing.T) {
	tool := newSlowServer(t, 200*time.Millisecond)

	start := time.Now()
	res, err := tool.Run(context.Background(), map[string]any{}, nil)
	if err != nil {
		t.Fatalf("timeout should surface as a normal result, got error: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("call was not abandoned promptly: %s", elapsed)
	}
	if !strings.Contains(res.LLMOutput, "timed out") {
		t.Errorf("result should mention the timeout, got %q", res.LLMOutput)
	}
}

// TestMCPTool_UserCancelNotTimeout verifies a cancelled parent context is
// returned as an error (so the agent loop's cancel handling matches),
// never mislabelled as our timeout.
func TestMCPTool_UserCancelNotTimeout(t *testing.T) {
	tool := newSlowServer(t, 30*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(200 * time.Millisecond); cancel() }()

	res, err := tool.Run(ctx, map[string]any{}, nil)
	if err == nil {
		t.Fatalf("user cancel should return an error, got result %q", res.LLMOutput)
	}
	if strings.Contains(res.LLMOutput, "timed out") {
		t.Errorf("user cancel must not be reported as a timeout, got %q", res.LLMOutput)
	}
}
