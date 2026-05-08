// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/TaraTheStar/enso/internal/llm"
)

// WriteMarkdown renders a loaded session State as a Markdown
// transcript. Tool results (role=tool messages) are inlined under
// their matching assistant tool_call. Free-floating tool messages —
// rare but possible from interrupted resumes — are emitted under a
// "Tool result (unmatched)" heading so nothing is silently dropped.
//
// Lifted from cmd/enso/export.go so the same renderer drives both
// the `enso export` CLI and the `/export` slash command.
func WriteMarkdown(w io.Writer, state *State) error {
	resultByID := map[string]string{}
	for _, m := range state.History {
		if m.Role == "tool" && m.ToolCallID != "" {
			resultByID[m.ToolCallID] = m.Content
		}
	}

	bw := &mdWriter{w: w}
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
				writeToolCallMD(bw, tc, resultByID[tc.ID])
			}
		case "tool":
			if _, matched := resultByID[m.ToolCallID]; matched && m.ToolCallID != "" {
				continue
			}
			bw.printf("### Tool result (unmatched: %s)\n\n```\n%s\n```\n\n", m.Name, m.Content)
		}
	}
	return bw.err
}

// WriteMarkdownByID is a convenience for callers that have a Store +
// session id but haven't loaded yet — the most common shape on the
// CLI side.
func WriteMarkdownByID(w io.Writer, store *Store, id string) error {
	state, err := Load(store, id)
	if err != nil {
		return err
	}
	return WriteMarkdown(w, state)
}

func writeToolCallMD(bw *mdWriter, tc llm.ToolCall, result string) {
	bw.printf("### Tool call: `%s`\n\n", tc.Function.Name)
	if args := strings.TrimSpace(tc.Function.Arguments); args != "" {
		bw.printf("```json\n%s\n```\n\n", prettyJSONExport(args))
	}
	if result != "" {
		bw.printf("**Result:**\n\n```\n%s\n```\n\n", result)
	}
}

// prettyJSONExport re-indents a JSON string for readability. Falls
// back to the original string on parse failure — the export must not
// fail on malformed args (the model occasionally emits them).
func prettyJSONExport(s string) string {
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

// mdWriter is a tiny io.Writer + sticky-error + Printf helper so the
// renderer body stays linear.
type mdWriter struct {
	w   io.Writer
	err error
}

func (b *mdWriter) printf(format string, args ...any) {
	if b.err != nil {
		return
	}
	_, b.err = fmt.Fprintf(b.w, format, args...)
}
