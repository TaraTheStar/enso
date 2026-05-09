// SPDX-License-Identifier: AGPL-3.0-or-later

package status

import (
	"strings"
	"testing"
)

func TestCompileDefaultStatusLine(t *testing.T) {
	tpl, err := Compile("")
	if err != nil {
		t.Fatal(err)
	}
	got := Render(tpl, Context{
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
	tpl, err := Compile("{{.Mode}} | {{.Model}} | {{.TokensFmt}}")
	if err != nil {
		t.Fatal(err)
	}
	got := Render(tpl, Context{
		Provider: "local", Model: "qwen3.6", Mode: "AUTO", TokensFmt: "5k/32k 16%",
	})
	if got != "AUTO | qwen3.6 | 5k/32k 16%" {
		t.Errorf("got %q", got)
	}
}

func TestDefaultStatusLine_AppendsTokensPerSecWhileStreaming(t *testing.T) {
	tpl, err := Compile("")
	if err != nil {
		t.Fatal(err)
	}
	got := Render(tpl, Context{
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
	tpl, err := Compile("")
	if err != nil {
		t.Fatal(err)
	}
	got := Render(tpl, Context{
		Provider: "local", Model: "qwen3.6", Session: "abc", TokensFmt: "1k/32k",
	})
	if strings.Contains(got, "t/s") {
		t.Errorf("rate segment leaked when TokensPerSec=0: %q", got)
	}
}

func TestCompileBadTemplateErrors(t *testing.T) {
	_, err := Compile("{{.Model")
	if err == nil {
		t.Errorf("expected parse error on unclosed action")
	}
}

func TestRenderRecoversFromExecError(t *testing.T) {
	// A field that doesn't exist on Context fails at execute-time.
	tpl, err := Compile("{{.NoSuchField}}")
	if err != nil {
		t.Fatal(err)
	}
	got := Render(tpl, Context{Provider: "local", Model: "m", Session: "s", TokensFmt: "0/0"})
	if !strings.Contains(got, "template err") {
		t.Errorf("expected fallback with template-err marker, got %q", got)
	}
}

// Templated conn-state segment tests live here because they exercise
// the template engine. The classifier (fmtConnState) lives in
// internal/tui because its output uses tview tag markup; tests for it
// stay there too.

func TestStatusTemplate_HiddenWhenHealthy(t *testing.T) {
	tpl, err := Compile("")
	if err != nil {
		t.Fatal(err)
	}
	got := Render(tpl, Context{
		ConnState:      "",
		SidebarVisible: true,
	})
	if got != "" {
		t.Errorf("healthy state with sidebar open should render empty, got %q", got)
	}
}

func TestStatusTemplate_DegradedWhenSidebarOpen(t *testing.T) {
	// With the sidebar open and no streaming, the template normally
	// renders nothing. The conn segment is the one thing that should
	// still show up — that's the whole reason for the indicator.
	tpl, err := Compile("")
	if err != nil {
		t.Fatal(err)
	}
	got := Render(tpl, Context{
		ConnState:      "[red]✘ disconnected[-]",
		SidebarVisible: true,
	})
	if !strings.Contains(got, "disconnected") {
		t.Errorf("conn segment missing in degraded render: %q", got)
	}
}

func TestStatusTemplate_SeparatorBetweenConnAndOther(t *testing.T) {
	tpl, err := Compile("")
	if err != nil {
		t.Fatal(err)
	}
	got := Render(tpl, Context{
		ConnState:      "[red]✘ disconnected[-]",
		TokensFmt:      "12k/32k",
		SidebarVisible: false,
	})
	if !strings.Contains(got, "disconnected") || !strings.Contains(got, "12k/32k") {
		t.Errorf("missing segments: %q", got)
	}
	if !strings.Contains(got, " · ") {
		t.Errorf("missing separator between conn and tokens segments: %q", got)
	}
}

func TestStatusTemplate_NoTrailingSeparatorWhenOnlyConn(t *testing.T) {
	// Conn-only render must not leave a dangling " · " visual artefact.
	tpl, err := Compile("")
	if err != nil {
		t.Fatal(err)
	}
	got := Render(tpl, Context{
		ConnState:      "[red]✘ disconnected[-]",
		SidebarVisible: true,
	})
	if strings.HasSuffix(got, " · ") {
		t.Errorf("trailing separator present with conn-only render: %q", got)
	}
}
