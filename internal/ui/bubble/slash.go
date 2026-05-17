// SPDX-License-Identifier: AGPL-3.0-or-later

package bubble

import (
	"context"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/TaraTheStar/enso/internal/backend/host"
	"github.com/TaraTheStar/enso/internal/bus"
	"github.com/TaraTheStar/enso/internal/llm"
	"github.com/TaraTheStar/enso/internal/lsp"
	"github.com/TaraTheStar/enso/internal/mcp"
	"github.com/TaraTheStar/enso/internal/permissions"
	"github.com/TaraTheStar/enso/internal/session"
	"github.com/TaraTheStar/enso/internal/slash"
	"github.com/TaraTheStar/enso/internal/tools"
	"github.com/TaraTheStar/enso/internal/workflow"
)

// slashCtx is the state slash command handlers see. Handlers print
// into the `out` buffer; the dispatcher tea.Println's the result to
// scrollback after Run returns. quit short-circuits the program.
type slashCtx struct {
	agt       agentControl
	checker   *permissions.Checker
	registry  *tools.Registry
	store     *session.Store
	writer    *session.Writer
	providers map[string]*llm.Provider
	cwd       string

	// sess is the worker session when the TUI runs behind the Backend
	// seam (sandbox-off / LocalBackend); nil on the legacy in-process
	// path. Slash commands whose effect must reach the worker's REAL
	// state (e.g. /yolo toggling the enforcing checker) round-trip
	// through it; the host-side checker is then a display mirror.
	sess *host.Session

	// bus is the host event bus (busInst). /compact publishes its
	// background ForceCompact failure here. In-process and worker paths
	// both have a host bus, so the slash layer never reaches through
	// the agent for it (the worker agent's bus isn't host-visible).
	bus *bus.Bus

	// Runtime managers for /lsp and /mcp. Either may be nil (e.g. if
	// cfg has no servers configured) — handlers must guard.
	lspMgr *lsp.Manager
	mcpMgr *mcp.Manager

	// transcripts is the per-agent message log captured for sub-agent
	// runs. /transcript reads it to surface a child agent's history.
	transcripts *tools.Transcripts

	// conv is read by /find for in-session match search. Set by
	// run.go after the conversation state machine is constructed.
	conv *conversation

	// submit pushes a synthetic user message onto the agent's input
	// channel without echoing it through the typed-input path. Used by
	// /init (and /loop's per-tick submission).
	submit func(text string)

	// workflowDeps is the pre-built RunDeps that /workflow uses. Built
	// in run.go after the agent is constructed so all the cross-cutting
	// runtime references (transcripts, agent ctx counters, config
	// flags) are wired in one place.
	workflowDeps workflow.RunDeps

	out  strings.Builder
	quit bool
}

// printf appends formatted output that the dispatcher will surface as a
// single styled scrollback line group after the handler returns. A
// trailing newline is appended automatically when missing.
func (c *slashCtx) printf(format string, args ...any) {
	fmt.Fprintf(&c.out, format, args...)
	if !strings.HasSuffix(format, "\n") {
		c.out.WriteByte('\n')
	}
}

// dispatchSlash runs `line` as a slash command (caller has already
// confirmed it begins with `/`) and returns the Cmd the model should
// emit — typically a tea.Println echoing the command and its output to
// scrollback. Returns nil if the line is empty or unparseable.
func dispatchSlash(reg *slash.Registry, sc *slashCtx, line string) tea.Cmd {
	name, args, ok := slash.Parse(line)
	if !ok {
		return nil
	}
	cmdEcho := "/" + name
	if args != "" {
		cmdEcho += " " + args
	}
	echo := statusStyle.Render(cmdEcho)

	cmd := reg.Get(name)
	if cmd == nil {
		body := errorStyle.Render(fmt.Sprintf("unknown command: /%s (try /help)", name))
		return tea.Println(echo + "\n" + body)
	}

	sc.out.Reset()
	if err := cmd.Run(context.Background(), args); err != nil {
		body := errorStyle.Render(fmt.Sprintf("/%s: %v", name, err))
		return tea.Println(echo + "\n" + body)
	}
	output := strings.TrimRight(sc.out.String(), "\n")
	var print tea.Cmd
	if output == "" {
		print = tea.Println(echo)
	} else {
		// Output is dim by default so it reads as a system response,
		// not regular chat content. Handlers that want emphasis on
		// individual lines can include their own escapes.
		print = tea.Println(echo + "\n" + statusStyle.Render(output))
	}

	if sc.quit {
		sc.quit = false
		return tea.Sequence(print, tea.Quit)
	}
	return print
}
