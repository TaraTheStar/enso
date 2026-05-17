// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"reflect"
	"testing"
)

func TestScrubbedForWorker(t *testing.T) {
	c := &Config{
		Providers: map[string]ProviderConfig{
			"a": {Endpoint: "https://api.openai.com/v1", APIKey: "sk-secret-a", Model: "gpt"},
			"b": {Endpoint: "http://localhost:8080", APIKey: "sk-secret-b", Model: "qwen"},
		},
	}
	c.Search.SearXNG.Endpoint = "http://localhost:8888"
	c.Search.SearXNG.APIKey = "searxng-key"
	c.MCP = map[string]MCPConfig{
		"gh": {Command: "gh-mcp", Headers: map[string]string{"Authorization": "Bearer ghp_x"}},
	}

	got := c.ScrubbedForWorker()

	// Provider creds (worker:"deny") gone from every map entry.
	for name, p := range got.Providers {
		if p.Endpoint != "" || p.APIKey != "" {
			t.Errorf("provider %q not scrubbed: %+v", name, p)
		}
		if p.Model == "" {
			t.Errorf("provider %q lost non-secret field Model", name)
		}
	}

	// The caller's live config must be untouched (deep copy).
	if c.Providers["a"].APIKey != "sk-secret-a" || c.Providers["a"].Endpoint == "" {
		t.Errorf("ScrubbedForWorker mutated the caller's config: %+v", c.Providers["a"])
	}

	// Non-provider secrets the worker legitimately uses are deliberately
	// preserved (LocalBackend has no boundary; Podman withholds them via
	// empty-env + the broker, not by mangling the config).
	if got.Search.SearXNG.APIKey != "searxng-key" {
		t.Errorf("SearXNG api_key should NOT be scrubbed (worker uses it); got %q", got.Search.SearXNG.APIKey)
	}
	if got.MCP["gh"].Headers["Authorization"] != "Bearer ghp_x" {
		t.Errorf("MCP auth header should NOT be scrubbed; got %q", got.MCP["gh"].Headers["Authorization"])
	}

	// Invariant: no field tagged worker:"deny" survives anywhere.
	assertNoDenyTagSurvives(t, reflect.ValueOf(got), "Config")
}

// assertNoDenyTagSurvives walks the scrubbed config and fails if any
// worker:"deny" field is non-zero — this is the regression guard the
// reviewer asked for: tag a new host-proxied-only secret and it is
// covered automatically; forget to and this test catches a non-zero
// one immediately.
func assertNoDenyTagSurvives(t *testing.T, v reflect.Value, path string) {
	t.Helper()
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			assertNoDenyTagSurvives(t, v.Elem(), path)
		}
	case reflect.Struct:
		tp := v.Type()
		for i := 0; i < v.NumField(); i++ {
			f := tp.Field(i)
			if !f.IsExported() {
				continue
			}
			fp := path + "." + f.Name
			if tag, ok := f.Tag.Lookup("worker"); ok && tag == "deny" {
				if !v.Field(i).IsZero() {
					t.Errorf("%s is tagged worker:\"deny\" but survived scrubbing: %v", fp, v.Field(i).Interface())
				}
				continue
			}
			assertNoDenyTagSurvives(t, v.Field(i), fp)
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			assertNoDenyTagSurvives(t, v.Index(i), path)
		}
	case reflect.Map:
		for _, k := range v.MapKeys() {
			assertNoDenyTagSurvives(t, v.MapIndex(k), path)
		}
	}
}
