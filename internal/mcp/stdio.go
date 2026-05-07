// SPDX-License-Identifier: AGPL-3.0-or-later

package mcp

import (
	"fmt"

	mcpclient "github.com/mark3labs/mcp-go/client"

	"github.com/TaraTheStar/enso/internal/config"
)

// openStdio spawns a subprocess and speaks MCP over its stdio pipes.
// `$VAR` / `${VAR}` in args resolve against ENSO_-prefixed env vars
// only (see config.ExpandEnsoEnv); references to other names collapse
// to empty so a hostile config can't pull AWS_*, GITHUB_TOKEN, etc.
// into argv.
func openStdio(cfg config.MCPConfig) (*mcpclient.Client, error) {
	args := make([]string, len(cfg.Args))
	for i, a := range cfg.Args {
		args[i] = config.ExpandEnsoEnv(a)
	}
	c, err := mcpclient.NewStdioMCPClient(cfg.Command, nil, args...)
	if err != nil {
		return nil, fmt.Errorf("stdio %s: %w", cfg.Command, err)
	}
	return c, nil
}
