// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/TaraTheStar/enso/internal/llm"
	"github.com/TaraTheStar/enso/internal/session"
)

func TestWriteMarkdownExport(t *testing.T) {
	asst := llm.Message{Role: "assistant", Content: "let me check"}
	asst.ToolCalls = []llm.ToolCall{{ID: "c1"}}
	asst.ToolCalls[0].Function.Name = "bash"
	asst.ToolCalls[0].Function.Arguments = `{"cmd":"ls"}`

	state := &session.State{
		Info: session.SessionInfo{
			ID:        "abc-123",
			Model:     "qwen3.6",
			Provider:  "local",
			Cwd:       "/tmp/proj",
			CreatedAt: time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC),
			UpdatedAt: time.Date(2026, 5, 3, 12, 5, 0, 0, time.UTC),
		},
		History: []llm.Message{
			{Role: "user", Content: "list files"},
			asst,
			{Role: "tool", ToolCallID: "c1", Name: "bash", Content: "main.go\nrun.go"},
			{Role: "assistant", Content: "two files."},
		},
	}

	var buf bytes.Buffer
	if err := writeMarkdownExport(&buf, state); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	wantSubs := []string{
		"# Session abc-123",
		"**Model**: qwen3.6",
		"**Cwd**: /tmp/proj",
		"## User\n\nlist files",
		"## Assistant\n\nlet me check",
		"### Tool call: `bash`",
		`"cmd": "ls"`, // pretty-printed
		"**Result:**",
		"main.go\nrun.go",
		"## Assistant\n\ntwo files.",
	}
	for _, s := range wantSubs {
		if !strings.Contains(out, s) {
			t.Errorf("output missing %q\nfull:\n%s", s, out)
		}
	}

	// The matched tool message should not also render as an "unmatched" block.
	if strings.Contains(out, "unmatched") {
		t.Errorf("matched tool result should not render as unmatched:\n%s", out)
	}
}
