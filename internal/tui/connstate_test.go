// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"context"
	"strings"
	"testing"

	"github.com/TaraTheStar/enso/internal/llm"
)

// stubReporter is a ChatClient that also implements ConnStateReporter.
// We don't drive Chat() in these tests — only the type assertion path.
type stubReporter struct{ state llm.ConnState }

func (stubReporter) Chat(context.Context, llm.ChatRequest) (<-chan llm.Event, error) {
	return nil, nil
}
func (s stubReporter) LLMConnState() llm.ConnState { return s.state }

// stubNoReporter satisfies ChatClient but not ConnStateReporter — the
// path tests fall through silently for fakes that don't track state.
type stubNoReporter struct{}

func (stubNoReporter) Chat(context.Context, llm.ChatRequest) (<-chan llm.Event, error) {
	return nil, nil
}

func TestFmtConnState_HealthyEmpty(t *testing.T) {
	if got := fmtConnState(stubReporter{state: llm.StateConnected}); got != "" {
		t.Errorf("Connected should produce empty segment, got %q", got)
	}
}

func TestFmtConnState_Reconnecting(t *testing.T) {
	got := fmtConnState(stubReporter{state: llm.StateReconnecting})
	if !strings.Contains(got, "reconnecting") {
		t.Errorf("missing 'reconnecting' in %q", got)
	}
	if !strings.Contains(got, "[yellow]") {
		t.Errorf("expected yellow tcell tag, got %q", got)
	}
}

func TestFmtConnState_Disconnected(t *testing.T) {
	got := fmtConnState(stubReporter{state: llm.StateDisconnected})
	if !strings.Contains(got, "disconnected") {
		t.Errorf("missing 'disconnected' in %q", got)
	}
	if !strings.Contains(got, "[red]") {
		t.Errorf("expected red tcell tag, got %q", got)
	}
}

func TestFmtConnState_NonReporterIsHealthy(t *testing.T) {
	// Test fakes (the llmtest.Mock chat client) don't implement
	// ConnStateReporter; the segment must stay empty so test runs
	// don't surface a misleading "disconnected" indicator.
	if got := fmtConnState(stubNoReporter{}); got != "" {
		t.Errorf("non-reporter client should produce empty segment, got %q", got)
	}
}

func TestStatusTemplate_HiddenWhenHealthy(t *testing.T) {
	tpl, err := compileStatusLine("")
	if err != nil {
		t.Fatal(err)
	}
	got := renderStatusLine(tpl, statusContext{
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
	tpl, err := compileStatusLine("")
	if err != nil {
		t.Fatal(err)
	}
	got := renderStatusLine(tpl, statusContext{
		ConnState:      "[red]✘ disconnected[-]",
		SidebarVisible: true,
	})
	if !strings.Contains(got, "disconnected") {
		t.Errorf("conn segment missing in degraded render: %q", got)
	}
}

func TestStatusTemplate_SeparatorBetweenConnAndOther(t *testing.T) {
	tpl, err := compileStatusLine("")
	if err != nil {
		t.Fatal(err)
	}
	got := renderStatusLine(tpl, statusContext{
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
	tpl, err := compileStatusLine("")
	if err != nil {
		t.Fatal(err)
	}
	got := renderStatusLine(tpl, statusContext{
		ConnState:      "[red]✘ disconnected[-]",
		SidebarVisible: true,
	})
	if strings.HasSuffix(got, " · ") {
		t.Errorf("trailing separator present with conn-only render: %q", got)
	}
}
