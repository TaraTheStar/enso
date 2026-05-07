// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/TaraTheStar/enso/internal/llm"
	"github.com/TaraTheStar/enso/internal/session"
)

// runExport reads a session by id from ~/.enso/enso.db and writes a markdown
// transcript to stdout (or --out path).
func runExport(id, outPath string) error {
	store, err := session.Open()
	if err != nil {
		return fmt.Errorf("open session store: %w", err)
	}
	defer store.Close()

	state, err := session.Load(store, id)
	if err != nil {
		return err
	}

	var w io.Writer = os.Stdout
	if outPath != "" {
		f, err := os.Create(outPath)
		if err != nil {
			return fmt.Errorf("create %s: %w", outPath, err)
		}
		defer f.Close()
		w = f
	}

	return writeMarkdownExport(w, state)
}

// writeMarkdownExport emits a session transcript as Markdown. Tool results
// (role=tool messages) are inlined under their matching assistant tool_call.
// Free-floating tool messages — if any — are emitted under a "Tool result"
// heading so nothing is silently dropped.
func writeMarkdownExport(w io.Writer, state *session.State) error {
	resultByID := map[string]string{}
	for _, m := range state.History {
		if m.Role == "tool" && m.ToolCallID != "" {
			resultByID[m.ToolCallID] = m.Content
		}
	}

	bw := newWriter(w)
	bw.printf("# Session %s\n\n", state.Info.ID)
	bw.printf("- **Created**: %s\n", state.Info.CreatedAt.Format(time.RFC3339))
	bw.printf("- **Updated**: %s\n", state.Info.UpdatedAt.Format(time.RFC3339))
	bw.printf("- **Model**: %s\n", state.Info.Model)
	bw.printf("- **Provider**: %s\n", state.Info.Provider)
	bw.printf("- **Cwd**: %s\n", state.Info.Cwd)
	if state.Interrupted {
		bw.printf("- **Interrupted**: yes (synthetic tool replies were inserted on resume)\n")
	}
	bw.printf("\n---\n\n")

	for _, m := range state.History {
		switch m.Role {
		case "system":
			bw.printf("## System\n\n%s\n\n", strings.TrimSpace(m.Content))
		case "user":
			bw.printf("## User\n\n%s\n\n", strings.TrimSpace(m.Content))
		case "assistant":
			bw.printf("## Assistant\n\n")
			if c := strings.TrimSpace(m.Content); c != "" {
				bw.printf("%s\n\n", c)
			}
			for _, tc := range m.ToolCalls {
				writeToolCall(bw, tc, resultByID[tc.ID])
			}
		case "tool":
			// Inlined under the assistant message above when matched.
			// A non-matching tool message is rare (tool_call_id is always set
			// on writes from the agent loop) but render anything left so the
			// export is lossless.
			if _, matched := resultByID[m.ToolCallID]; matched && m.ToolCallID != "" {
				continue
			}
			bw.printf("### Tool result (unmatched: %s)\n\n```\n%s\n```\n\n", m.Name, m.Content)
		}
	}
	return bw.err
}

func writeToolCall(bw *writerW, tc llm.ToolCall, result string) {
	bw.printf("### Tool call: `%s`\n\n", tc.Function.Name)
	if args := strings.TrimSpace(tc.Function.Arguments); args != "" {
		bw.printf("```json\n%s\n```\n\n", prettyJSON(args))
	}
	if result != "" {
		bw.printf("**Result:**\n\n```\n%s\n```\n\n", result)
	}
}

// prettyJSON re-indents a JSON string for readability. If the input isn't
// valid JSON we return it as-is — the export shouldn't fail on malformed
// args.
func prettyJSON(s string) string {
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return s
	}
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return s
	}
	return string(out)
}

// writerW is a tiny "io.Writer + sticky-error + Printf" helper so the
// renderer body stays linear.
type writerW struct {
	w   io.Writer
	err error
}

func newWriter(w io.Writer) *writerW { return &writerW{w: w} }

func (b *writerW) printf(format string, args ...any) {
	if b.err != nil {
		return
	}
	_, b.err = fmt.Fprintf(b.w, format, args...)
}
