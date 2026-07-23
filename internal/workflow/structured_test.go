// SPDX-License-Identifier: AGPL-3.0-or-later

package workflow

import (
	"context"
	"testing"

	"github.com/TaraTheStar/azoth/llm/llmtest"
	"github.com/TaraTheStar/enso/internal/bus"
)

func TestParseStructured_JSONBlock(t *testing.T) {
	text := "Here is my review.\n\n```json\n{\"verdict\": \"LGTM\", \"score\": 9, \"blocking\": false}\n```\n"
	got := parseStructured(text)
	if got["verdict"] != "LGTM" {
		t.Errorf("verdict = %q, want LGTM", got["verdict"])
	}
	if got["score"] != "9" { // integral float renders without ".0"
		t.Errorf("score = %q, want 9", got["score"])
	}
	if got["blocking"] != "false" {
		t.Errorf("blocking = %q, want false", got["blocking"])
	}
}

func TestParseStructured_LastJSONBlockWins(t *testing.T) {
	text := "```json\n{\"verdict\": \"reject\"}\n```\n\nrevised:\n\n```json\n{\"verdict\": \"LGTM\"}\n```"
	got := parseStructured(text)
	if got["verdict"] != "LGTM" {
		t.Errorf("verdict = %q, want LGTM (last block wins)", got["verdict"])
	}
}

func TestParseStructured_KeyValueFallback(t *testing.T) {
	text := "I have finished the analysis.\n\nverdict: LGTM\nreason: all tests pass\n"
	got := parseStructured(text)
	if got["verdict"] != "LGTM" {
		t.Errorf("verdict = %q, want LGTM", got["verdict"])
	}
	if got["reason"] != "all tests pass" {
		t.Errorf("reason = %q, want 'all tests pass'", got["reason"])
	}
}

func TestParseStructured_KeyValueStopsAtProse(t *testing.T) {
	// A trailing prose line that isn't KEY: value breaks the contiguous run;
	// only the lines below it are captured.
	text := "verdict: ignored because prose follows\nThis is a sentence, not a field.\nstatus: done\n"
	got := parseStructured(text)
	if got["status"] != "done" {
		t.Errorf("status = %q, want done", got["status"])
	}
	if _, ok := got["verdict"]; ok {
		t.Errorf("verdict should not be captured above the prose break: %v", got)
	}
}

func TestParseStructured_None(t *testing.T) {
	got := parseStructured("just some prose with no fields and no json")
	if len(got) != 0 {
		t.Errorf("expected empty fields, got %v", got)
	}
}

func TestParseStructured_MalformedJSON(t *testing.T) {
	// A malformed last JSON block yields empty fields (no KV fallback).
	text := "key: notused\n\n```json\n{not valid json,,,}\n```"
	got := parseStructured(text)
	if len(got) != 0 {
		t.Errorf("malformed json should yield empty fields, got %v", got)
	}
}

func TestParseStructured_NestedValuesStringified(t *testing.T) {
	text := "```json\n{\"tags\": [\"a\", \"b\"], \"meta\": {\"k\": 1}}\n```"
	got := parseStructured(text)
	if got["tags"] != `["a","b"]` {
		t.Errorf("tags = %q, want compact JSON array", got["tags"])
	}
	if got["meta"] != `{"k":1}` {
		t.Errorf("meta = %q, want compact JSON object", got["meta"])
	}
}

// TestRun_StructuredFieldReadableDownstream verifies a role emitting a JSON
// block exposes its fields as .<role>.<field> in a downstream template, while
// .<role>.output still carries the raw text.
func TestRun_StructuredFieldReadableDownstream(t *testing.T) {
	src := []byte("---\n" +
		"roles:\n" +
		"  review: {}\n" +
		"  report: {}\n" +
		"edges:\n" +
		"  - review -> report\n" +
		"---\n\n" +
		"## review\n\n" +
		"Review: {{ .Args }}\n\n" +
		"## report\n\n" +
		"verdict={{ .review.verdict }} raw={{ .review.output }}\n")
	wf, err := Parse("structured.md", src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	mock := llmtest.NewT(t)
	mock.Push(llmtest.Script{Text: "ok\n\n```json\n{\"verdict\": \"LGTM\"}\n```"})
	mock.Push(llmtest.Script{Text: "REPORT-DONE"})

	res, err := Run(context.Background(), wf, "x", runDeps(t, fakeProvider(mock, 1), bus.New()))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Fields["review"]["verdict"] != "LGTM" {
		t.Errorf("review fields = %v, want verdict=LGTM", res.Fields["review"])
	}

	// The report's rendered prompt must have resolved both the field and the
	// raw output.
	calls := mock.Calls()
	if len(calls) != 2 {
		t.Fatalf("want 2 turns, got %d", len(calls))
	}
	var prompt string
	for _, m := range calls[1].Messages {
		if m.Role == "user" {
			prompt = m.Content
		}
	}
	if !containsStr(prompt, "verdict=LGTM") {
		t.Errorf("report prompt missing resolved field: %q", prompt)
	}
	if !containsStr(prompt, "raw=ok") {
		t.Errorf("report prompt missing raw output: %q", prompt)
	}
}
