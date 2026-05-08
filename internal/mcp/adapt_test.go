// SPDX-License-Identifier: AGPL-3.0-or-later

package mcp

import (
	"strings"
	"testing"

	mcpproto "github.com/mark3labs/mcp-go/mcp"
)

func TestSchemaToMap_FullSchema(t *testing.T) {
	in := mcpproto.ToolInputSchema{
		Type: "object",
		Properties: map[string]any{
			"path": map[string]any{"type": "string"},
		},
		Required:             []string{"path"},
		AdditionalProperties: false,
	}
	got := schemaToMap(in)
	if got["type"] != "object" {
		t.Errorf("type = %v, want object", got["type"])
	}
	props, ok := got["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties = %T, want map", got["properties"])
	}
	if _, ok := props["path"]; !ok {
		t.Errorf("properties missing 'path'")
	}
	req, ok := got["required"].([]string)
	if !ok || len(req) != 1 || req[0] != "path" {
		t.Errorf("required = %v", got["required"])
	}
	if got["additionalProperties"] != false {
		t.Errorf("additionalProperties = %v, want false", got["additionalProperties"])
	}
}

func TestSchemaToMap_EmptyProperties(t *testing.T) {
	// An MCP schema with no properties should still produce a properties map
	// (empty, but present) since OpenAI-compatible servers expect the key.
	got := schemaToMap(mcpproto.ToolInputSchema{Type: "object"})
	props, ok := got["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties type = %T, want map", got["properties"])
	}
	if len(props) != 0 {
		t.Errorf("properties = %v, want empty map", props)
	}
	if _, has := got["required"]; has {
		t.Errorf("empty schema should not include `required` key")
	}
	if _, has := got["additionalProperties"]; has {
		t.Errorf("empty schema should not include `additionalProperties` key")
	}
}

func TestSchemaToMap_MissingTypeDefaultsToObject(t *testing.T) {
	got := schemaToMap(mcpproto.ToolInputSchema{})
	if got["type"] != "object" {
		t.Errorf("type = %v, want default 'object'", got["type"])
	}
}

func TestJoinTextContent_PicksTextDropsOthers(t *testing.T) {
	items := []mcpproto.Content{
		mcpproto.NewTextContent("first"),
		mcpproto.NewTextContent("second"),
	}
	got := joinTextContent(items)
	if !strings.Contains(got, "first") || !strings.Contains(got, "second") {
		t.Errorf("got %q, missing 'first'/'second'", got)
	}
	// joins with newlines between fragments
	if got != "first\nsecond" {
		t.Errorf("got %q, want exactly 'first\\nsecond'", got)
	}
}

func TestJoinTextContent_EmptyList(t *testing.T) {
	if got := joinTextContent(nil); got != "" {
		t.Errorf("empty list = %q, want empty string", got)
	}
}

func TestValidateName(t *testing.T) {
	cases := []struct {
		name    string
		s       string
		max     int
		wantErr bool
	}{
		{"plain", "search", 32, false},
		{"with-dash", "list-items", 32, false},
		{"with-underscore", "do_thing", 32, false},
		{"alphanumeric", "tool42", 32, false},
		{"empty", "", 32, true},
		{"space", "do thing", 32, true},
		{"slash", "list/items", 32, true},
		{"dot", "db.query", 32, true},
		{"newline", "do\nthing", 32, true},
		{"unicode", "café", 32, true},
		{"too-long", strings.Repeat("a", 33), 32, true},
		{"max-len", strings.Repeat("a", 32), 32, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateName(tc.s, tc.max)
			if (err != nil) != tc.wantErr {
				t.Errorf("validateName(%q) err=%v, wantErr=%v", tc.s, err, tc.wantErr)
			}
		})
	}
}

func TestAdaptTool_RejectsBadToolName(t *testing.T) {
	cases := []struct {
		toolName string
		wantNil  bool
	}{
		{"search", false},
		{"do thing", true},
		{"list/items", true},
		{"", true},
		{strings.Repeat("a", 33), true},
		{"db.query", true}, // dotted names rejected (out of OpenAI charset)
		{"\nevil", true},
		{"normal_name", false},
	}
	for _, tc := range cases {
		t.Run(tc.toolName, func(t *testing.T) {
			got := adaptTool("server", nil, mcpproto.Tool{Name: tc.toolName}, nil)
			if (got == nil) != tc.wantNil {
				t.Errorf("adaptTool(%q) nil=%v, wantNil=%v", tc.toolName, got == nil, tc.wantNil)
			}
		})
	}
}

func TestAdaptTool_ValidNamePrefixed(t *testing.T) {
	got := adaptTool("myserver", nil, mcpproto.Tool{Name: "search"}, nil)
	if got == nil {
		t.Fatal("expected tool, got nil")
	}
	if got.Name() != "mcp__myserver__search" {
		t.Errorf("name = %q, want mcp__myserver__search", got.Name())
	}
}
