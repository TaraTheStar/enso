package llm

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// ChatRequest is not just an internal DTO: it is serialized VERBATIM as
// the OpenAI-compatible POST body (openai.go embeds it directly into
// the request struct) and it crosses the worker seam inside
// backend/wire.InferenceRequest. Any field added to the struct
// therefore lands on both wires immediately — there is no translation
// layer to catch it. These tests freeze the field set and JSON tags so
// adding internal bookkeeping to ChatRequest is a deliberate,
// test-visible protocol change instead of a silent leak.

// chatRequestWireFields is the FROZEN Go-field → JSON-key mapping.
// omitempty records which fields vanish from the wire at zero value
// (servers keep their own defaults for those knobs).
var chatRequestWireFields = []struct {
	goField   string
	jsonKey   string
	omitempty bool
}{
	{"Model", "model", false},
	{"Messages", "messages", false},
	{"Stream", "stream", false},
	{"Tools", "tools", true},
	{"MaxTokens", "max_tokens", true},
	{"Stop", "stop", true},
	{"Temperature", "temperature", true},
	{"TopK", "top_k", true},
	{"TopP", "top_p", true},
	{"MinP", "min_p", true},
	{"PresencePenalty", "presence_penalty", true},
	{"FrequencyPenalty", "frequency_penalty", true},
	{"RepetitionPenalty", "repetition_penalty", true},
}

// TestChatRequestWireShapeFrozen asserts ChatRequest has exactly the
// frozen fields with exactly the frozen tags — a new field, a renamed
// tag, or a dropped omitempty all fail here.
func TestChatRequestWireShapeFrozen(t *testing.T) {
	rt := reflect.TypeOf(ChatRequest{})
	if rt.NumField() != len(chatRequestWireFields) {
		t.Errorf("ChatRequest has %d fields, frozen table has %d — ChatRequest is an "+
			"OpenAI wire body AND the worker-seam inference payload; new fields ship "+
			"on both wires, so freeze them here deliberately", rt.NumField(), len(chatRequestWireFields))
	}
	for i, want := range chatRequestWireFields {
		if i >= rt.NumField() {
			t.Errorf("missing field %q (frozen table row %d)", want.goField, i)
			continue
		}
		f := rt.Field(i)
		if f.Name != want.goField {
			t.Errorf("field %d: name = %q, frozen table says %q (order matters: it is the wire order)", i, f.Name, want.goField)
		}
		tag := f.Tag.Get("json")
		key, opts, _ := strings.Cut(tag, ",")
		if key != want.jsonKey {
			t.Errorf("field %s: json key = %q, frozen wire key is %q", f.Name, key, want.jsonKey)
		}
		if gotOmit := strings.Contains(opts, "omitempty"); gotOmit != want.omitempty {
			t.Errorf("field %s: omitempty = %v, frozen as %v — changes when the key appears on the wire", f.Name, gotOmit, want.omitempty)
		}
	}
}

// TestChatRequestMarshalKeys round-trips a fully-populated and a
// zero-value request through encoding/json and asserts the exact key
// sets that reach the wire.
func TestChatRequestMarshalKeys(t *testing.T) {
	full := ChatRequest{
		Model:             "m",
		Messages:          []Message{{Role: "user", Content: "hi"}},
		Stream:            true,
		Tools:             []ToolDef{{Type: "function"}},
		MaxTokens:         1,
		Stop:              []string{"x"},
		Temperature:       0.1,
		TopK:              2,
		TopP:              0.3,
		MinP:              0.4,
		PresencePenalty:   0.5,
		FrequencyPenalty:  0.6,
		RepetitionPenalty: 0.7,
	}
	assertKeys := func(t *testing.T, req ChatRequest, want []string) {
		t.Helper()
		data, err := json.Marshal(req)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		for _, k := range want {
			if _, ok := m[k]; !ok {
				t.Errorf("wire body missing key %q", k)
			}
			delete(m, k)
		}
		for k := range m {
			t.Errorf("unexpected wire key %q — a ChatRequest field leaked onto the OpenAI/seam wire", k)
		}
	}
	t.Run("full", func(t *testing.T) {
		want := make([]string, len(chatRequestWireFields))
		for i, f := range chatRequestWireFields {
			want[i] = f.jsonKey
		}
		assertKeys(t, full, want)
	})
	t.Run("zero", func(t *testing.T) {
		// Only the non-omitempty trio survives at zero value; every
		// sampler knob must vanish so servers keep their own defaults.
		assertKeys(t, ChatRequest{}, []string{"model", "messages", "stream"})
	})
}
