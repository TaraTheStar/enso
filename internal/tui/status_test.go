// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"strings"
	"testing"
)

func TestCompileDefaultStatusLine(t *testing.T) {
	tpl, err := compileStatusLine("")
	if err != nil {
		t.Fatal(err)
	}
	got := renderStatusLine(tpl, statusContext{
		Provider:  "local",
		Model:     "qwen3.6",
		Session:   "abc",
		TokensFmt: "1k/32k 3%",
	})
	// Default no longer includes provider/model/session — those moved
	// to the right sidebar so they don't render in two places at once.
	want := "1k/32k 3%"
	if got != want {
		t.Errorf("default render = %q, want %q", got, want)
	}
}

func TestCompileCustomStatusLine(t *testing.T) {
	tpl, err := compileStatusLine("{{.Mode}} | {{.Model}} | {{.TokensFmt}}")
	if err != nil {
		t.Fatal(err)
	}
	got := renderStatusLine(tpl, statusContext{
		Provider: "local", Model: "qwen3.6", Mode: "AUTO", TokensFmt: "5k/32k 16%",
	})
	if got != "AUTO | qwen3.6 | 5k/32k 16%" {
		t.Errorf("got %q", got)
	}
}

func TestDefaultStatusLine_AppendsTokensPerSecWhileStreaming(t *testing.T) {
	tpl, err := compileStatusLine("")
	if err != nil {
		t.Fatal(err)
	}
	got := renderStatusLine(tpl, statusContext{
		Provider:     "local",
		Model:        "qwen3.6",
		Session:      "abc",
		TokensFmt:    "1k/32k",
		TokensPerSec: 42,
	})
	want := "1k/32k · 42 t/s"
	if got != want {
		t.Errorf("streaming render = %q, want %q", got, want)
	}
}

func TestDefaultStatusLine_OmitsRateWhenZero(t *testing.T) {
	tpl, err := compileStatusLine("")
	if err != nil {
		t.Fatal(err)
	}
	got := renderStatusLine(tpl, statusContext{
		Provider: "local", Model: "qwen3.6", Session: "abc", TokensFmt: "1k/32k",
	})
	if strings.Contains(got, "t/s") {
		t.Errorf("rate segment leaked when TokensPerSec=0: %q", got)
	}
}

func TestCompileBadTemplateErrors(t *testing.T) {
	_, err := compileStatusLine("{{.Model")
	if err == nil {
		t.Errorf("expected parse error on unclosed action")
	}
}

func TestRenderRecoversFromExecError(t *testing.T) {
	// A field that doesn't exist on statusContext fails at execute-time.
	tpl, err := compileStatusLine("{{.NoSuchField}}")
	if err != nil {
		t.Fatal(err)
	}
	got := renderStatusLine(tpl, statusContext{Provider: "local", Model: "m", Session: "s", TokensFmt: "0/0"})
	if !strings.Contains(got, "template err") {
		t.Errorf("expected fallback with template-err marker, got %q", got)
	}
}
