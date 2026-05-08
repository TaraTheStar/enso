// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestWriteStatsText_EmptyAllTime(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteStatsText(&buf, Stats{}, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "no sessions yet") {
		t.Errorf("expected 'no sessions yet': %q", buf.String())
	}
}

func TestWriteStatsText_EmptyWithSince(t *testing.T) {
	since := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var buf bytes.Buffer
	if err := WriteStatsText(&buf, Stats{}, since); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	if !strings.Contains(got, "no sessions since") {
		t.Errorf("expected 'no sessions since': %q", got)
	}
	if !strings.Contains(got, "2026-01-01") {
		t.Errorf("expected since date in output: %q", got)
	}
}

func TestWriteStatsText_PopulatedReportsAllSections(t *testing.T) {
	st := Stats{
		SessionCount:      3,
		InterruptedCount:  1,
		ApproxTotalTokens: 12345,
		MessagesByRole:    map[string]int{"user": 4, "assistant": 5, "tool": 2},
		SessionsByModel:   map[string]int{"qwen3.6": 3},
		ToolCallsByName: map[string]ToolCallStats{
			"bash": {Total: 5, OK: 4, Error: 1},
		},
		OldestUpdatedAt: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
		NewestUpdatedAt: time.Date(2026, 5, 8, 0, 0, 0, 0, time.UTC),
	}
	var buf bytes.Buffer
	if err := WriteStatsText(&buf, st, time.Time{}); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	wantSubs := []string{
		"Sessions:    3",
		"2026-05-01",
		"2026-05-08",
		"Interrupted: 1",
		"~12,345",
		"user       4",
		"assistant  5",
		"qwen3.6",
		"bash",
		"ok 4, error 1",
	}
	for _, s := range wantSubs {
		if !strings.Contains(got, s) {
			t.Errorf("output missing %q\nfull:\n%s", s, got)
		}
	}
}

func TestFormatThousands(t *testing.T) {
	cases := map[int]string{
		0:        "0",
		1000:     "1,000",
		12345:    "12,345",
		1234567:  "1,234,567",
		-1234567: "-1,234,567",
	}
	for in, want := range cases {
		if got := formatThousands(in); got != want {
			t.Errorf("formatThousands(%d)=%q, want %q", in, got, want)
		}
	}
}
