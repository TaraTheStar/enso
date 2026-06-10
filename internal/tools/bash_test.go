// SPDX-License-Identifier: AGPL-3.0-or-later

package tools

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/TaraTheStar/enso/internal/bus"
)

// TestBashTool_PublishesProgress runs a tiny shell command and asserts that
// ToolCallProgress events flow through the bus tagged with the right id and
// containing the command's stdout. Smoke-level — depends on `sh` and `echo`
// being available, which is true in any reasonable Go test environment.
func TestBashTool_PublishesProgress(t *testing.T) {
	b := bus.New()
	sub := b.Subscribe(16)

	ac := &AgentContext{
		Cwd:           t.TempDir(),
		Bus:           b,
		CurrentToolID: "tc-1",
	}

	res, err := BashTool{}.Run(
		context.Background(),
		map[string]any{"cmd": "echo hello"},
		ac,
	)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(res.LLMOutput, "hello") {
		t.Errorf("LLMOutput = %q, expected to contain 'hello'", res.LLMOutput)
	}

	// Drain bus until we either find the expected event or time out.
	deadline := time.After(time.Second)
	gotProgress := false
	for !gotProgress {
		select {
		case evt := <-sub:
			if evt.Type != bus.EventToolCallProgress {
				continue
			}
			m, _ := evt.Payload.(map[string]any)
			if m["id"] == "tc-1" && strings.Contains(m["text"].(string), "hello") {
				gotProgress = true
			}
		case <-deadline:
			t.Fatalf("no ToolCallProgress event with id=tc-1 / text~hello within timeout")
		}
	}
}

// TestBashTool_EmptyStdoutIsExplicit covers the case that surfaced via
// the eval harness: a command that succeeds with no output. An empty
// LLMOutput would marshal to a tool message with no `content` field
// (omitempty), and some OpenAI-compatible servers reject that with HTTP
// 400. We substitute an explicit "(exit 0, no output)" marker.
func TestBashTool_EmptyStdoutIsExplicit(t *testing.T) {
	ac := &AgentContext{Cwd: t.TempDir(), Bus: bus.New()}
	res, err := BashTool{}.Run(
		context.Background(),
		map[string]any{"cmd": "true"}, // exits 0 with no stdout
		ac,
	)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.LLMOutput == "" {
		t.Errorf("empty stdout must yield non-empty LLMOutput so tool messages carry content")
	}
	if !strings.Contains(res.LLMOutput, "no output") {
		t.Errorf("expected explicit marker, got %q", res.LLMOutput)
	}
}

// TestBashTool_ScrubsSecretEnv is the S9 regression: the model must not
// be able to read enso's resolved credentials via the bash child env.
func TestBashTool_ScrubsSecretEnv(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-secret-should-not-leak")
	t.Setenv("ENSO_OPENAI_KEY", "enso-indirection-secret")
	t.Setenv("GITHUB_TOKEN", "ghp-secret")
	t.Setenv("MY_HARMLESS_VAR", "keep-me")

	ac := &AgentContext{Cwd: t.TempDir()}
	res, err := BashTool{}.Run(context.Background(),
		map[string]any{"cmd": "echo key=$OPENAI_API_KEY enso=$ENSO_OPENAI_KEY gh=$GITHUB_TOKEN ok=$MY_HARMLESS_VAR"},
		ac)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, leak := range []string{"sk-secret-should-not-leak", "enso-indirection-secret", "ghp-secret"} {
		if strings.Contains(res.LLMOutput, leak) {
			t.Errorf("secret leaked into bash output: %q\n%s", leak, res.LLMOutput)
		}
	}
	// A non-secret var still passes through (we use a denylist, not an
	// allowlist, so builds keep the env they need).
	if !strings.Contains(res.LLMOutput, "keep-me") {
		t.Errorf("harmless var was wrongly scrubbed:\n%s", res.LLMOutput)
	}
}

func TestIsSecretEnvName(t *testing.T) {
	secret := []string{
		"OPENAI_API_KEY", "ENSO_FOO", "AWS_SECRET_ACCESS_KEY", "GITHUB_TOKEN", "DB_PASSWORD", "x_apikey", "MY_PRIVATE_KEY",
		// URL/connection-string style credentials.
		"DATABASE_URL", "REDIS_URL", "AMQP_URL", "MONGO_URI", "SENTRY_DSN", "PG_CONNECTION_STRING", "NPM_CONFIG_AUTH",
	}
	for _, n := range secret {
		if !isSecretEnvName(n) {
			t.Errorf("%q should be treated as secret", n)
		}
	}
	keep := []string{"PATH", "HOME", "LANG", "GOPATH", "TERM", "MY_VAR"}
	for _, n := range keep {
		if isSecretEnvName(n) {
			t.Errorf("%q should NOT be scrubbed", n)
		}
	}
}

// TestBashTool_CompressesFilteredOutput is the R1 end-to-end check: with a
// FilterSet loaded, go-test scaffolding is stripped from the model-visible
// output, the savings counter advances, and the spill footer points at the
// recoverable raw output.
func TestBashTool_CompressesFilteredOutput(t *testing.T) {
	stats := &CompressionStats{}
	ac := &AgentContext{
		Cwd:         t.TempDir(),
		Filters:     LoadFilterSet("", nil),
		Compression: stats,
		Spill:       &FileSpill{Root: t.TempDir(), SessionID: "s1"},
	}
	// Emulate `go test` output via a printf (exit 0 → success path). The
	// trailing comment makes the command string match the go-test filter
	// without actually invoking the toolchain.
	cmd := `printf '=== RUN   TestA\n--- PASS: TestA (0.00s)\n=== RUN   TestB\n    b_test.go:9: boom\n--- FAIL: TestB (0.01s)\nFAIL\n' # go test ./...`
	res, err := BashTool{}.Run(context.Background(),
		map[string]any{"cmd": cmd}, ac)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.Contains(res.LLMOutput, "=== RUN") || strings.Contains(res.LLMOutput, "--- PASS") {
		t.Fatalf("passing scaffolding not stripped from model output:\n%s", res.LLMOutput)
	}
	if !strings.Contains(res.LLMOutput, "b_test.go:9: boom") {
		t.Fatalf("failure detail dropped:\n%s", res.LLMOutput)
	}
	if stats.Saved() <= 0 {
		t.Fatalf("expected compression savings to be recorded, got %d", stats.Saved())
	}
	// Raw output is preserved in FullOutput for the session/recovery path.
	if !strings.Contains(res.FullOutput, "--- PASS: TestA") {
		t.Fatalf("raw output not preserved in FullOutput:\n%s", res.FullOutput)
	}
}

// TestBashTool_NoFilterSetPassesThrough confirms a nil FilterSet disables
// compression wholesale (config compress=false) — output is untouched.
func TestBashTool_NoFilterSetPassesThrough(t *testing.T) {
	ac := &AgentContext{Cwd: t.TempDir()} // Filters nil
	res, err := BashTool{}.Run(context.Background(),
		map[string]any{"cmd": `printf '=== RUN   TestA\n--- PASS: TestA\n'`}, ac)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(res.LLMOutput, "=== RUN") {
		t.Fatalf("nil FilterSet should not compress:\n%s", res.LLMOutput)
	}
}
