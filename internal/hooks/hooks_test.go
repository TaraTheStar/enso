// SPDX-License-Identifier: AGPL-3.0-or-later

package hooks

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordingHooks builds a Hooks with a synchronous Warn that
// captures messages for assertion.
func recordingHooks(onFileEdit, onSessionEnd string) (*Hooks, *[]string, *sync.Mutex) {
	var mu sync.Mutex
	var msgs []string
	h := New(onFileEdit, onSessionEnd)
	h.Warn = func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		msgs = append(msgs, sprintf(format, args...))
	}
	return h, &msgs, &mu
}

func sprintf(format string, args ...any) string {
	var sb strings.Builder
	for i := 0; ; i++ {
		idx := strings.Index(format, "%")
		if idx < 0 || i >= len(args) {
			sb.WriteString(format)
			break
		}
		sb.WriteString(format[:idx])
		// Skip the verb; %v / %s / %d all fine for tests.
		end := idx + 2
		if end > len(format) {
			end = len(format)
		}
		switch v := args[i].(type) {
		case string:
			sb.WriteString(v)
		case error:
			sb.WriteString(v.Error())
		default:
			sb.WriteString(asString(v))
		}
		format = format[end:]
	}
	return sb.String()
}

func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if s, ok := v.(interface{ String() string }); ok {
		return s.String()
	}
	return ""
}

func TestOnFileEdit_RunsCommandAndExpandsTemplate(t *testing.T) {
	tmp := t.TempDir()
	out := filepath.Join(tmp, "out.txt")

	// No template-side quoting around {{.Tool}}{{.Path}} — auto-quoting
	// in prepareVars produces 'edit':'foo.go', which the shell
	// concatenates into a single edit:foo.go arg. The output redirect
	// uses {{.Raw.Cwd}} explicitly so the literal path is what we
	// expect (auto-quoted Cwd would also work but reads weirdly).
	h := New(`echo -n {{.Tool}}:{{.Path}} > `+shellQuote(out), "")
	h.OnFileEdit(tmp, "foo.go", "edit")

	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("hook didn't write file: %v", err)
	}
	if got := string(data); got != "edit:foo.go" {
		t.Errorf("hook command got %q, want %q", got, "edit:foo.go")
	}
}

func TestOnFileEdit_EmptyConfigIsNoOp(t *testing.T) {
	h := New("", "")
	// Should not panic, should not Warn.
	h.OnFileEdit(t.TempDir(), "x.go", "edit")
}

func TestOnFileEdit_NilReceiverSafe(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("nil *Hooks panicked: %v", r)
		}
	}()
	var h *Hooks
	h.OnFileEdit("/", "x", "edit")
	h.OnSessionEnd("/", "id")
}

func TestOnFileEdit_TemplateErrorWarns(t *testing.T) {
	// Unclosed delimiter — fails at Parse.
	h, msgs, mu := recordingHooks(`echo {{`, "")
	h.OnFileEdit(t.TempDir(), "x.go", "edit")

	mu.Lock()
	defer mu.Unlock()
	if len(*msgs) == 0 || !strings.Contains((*msgs)[0], "template error") {
		t.Errorf("expected template-error warn, got %v", *msgs)
	}
}

func TestOnFileEdit_TimeoutWarns(t *testing.T) {
	h, msgs, mu := recordingHooks(`sleep 5`, "")
	h.Timeout = 100 * time.Millisecond
	start := time.Now()
	h.OnFileEdit(t.TempDir(), "x.go", "edit")
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Errorf("timeout did not kill the process: elapsed=%v", elapsed)
	}

	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, m := range *msgs {
		if strings.Contains(m, "timed out") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected timeout warn, got %v", *msgs)
	}
}

func TestOnFileEdit_NonZeroExitIsSilent(t *testing.T) {
	h, msgs, mu := recordingHooks(`exit 1`, "")
	h.OnFileEdit(t.TempDir(), "x.go", "edit")

	mu.Lock()
	defer mu.Unlock()
	if len(*msgs) != 0 {
		t.Errorf("non-zero exit should be silent, got %v", *msgs)
	}
}

func TestOnSessionEnd_RunsAndExpandsVars(t *testing.T) {
	tmp := t.TempDir()
	out := filepath.Join(tmp, "marker")

	h := New("", `echo -n {{.SessionID}} in {{.Cwd}} > `+shellQuote(out))
	h.OnSessionEnd(tmp, "sess-123")

	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("hook didn't write: %v", err)
	}
	got := string(data)
	if !strings.Contains(got, "sess-123") || !strings.Contains(got, tmp) {
		t.Errorf("vars not expanded: %q", got)
	}
}

// Auto-quoting prevents shell-injection through model-controlled var
// values. A path like `foo;rm -rf $TMP/sentinel` must be passed as a
// single arg to the hook command — never executed as a chained shell
// statement. The test creates a sentinel and confirms it survives.
func TestOnFileEdit_AutoQuotesShellMetacharsInPath(t *testing.T) {
	tmp := t.TempDir()
	sentinel := filepath.Join(tmp, "sentinel")
	if err := os.WriteFile(sentinel, []byte("alive"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(tmp, "out")
	// If quoting fails, the `; rm ...` would delete the sentinel.
	maliciousPath := "foo; rm " + sentinel
	h := New(`echo -n {{.Path}} > `+shellQuote(out), "")
	h.OnFileEdit(tmp, maliciousPath, "edit")

	if _, err := os.Stat(sentinel); os.IsNotExist(err) {
		t.Fatal("sentinel was deleted — shell injection succeeded")
	} else if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(out)
	// The literal path string lands in the file, including the metachars.
	if string(data) != maliciousPath {
		t.Errorf("got %q, want %q", string(data), maliciousPath)
	}
}

// Embedded single quotes must round-trip through the POSIX `'\”`
// escape correctly.
func TestOnFileEdit_AutoQuotesEmbeddedSingleQuote(t *testing.T) {
	tmp := t.TempDir()
	out := filepath.Join(tmp, "out")
	weird := "it's a 'name'"
	h := New(`echo -n {{.Path}} > `+shellQuote(out), "")
	h.OnFileEdit(tmp, weird, "edit")

	data, _ := os.ReadFile(out)
	if string(data) != weird {
		t.Errorf("got %q, want %q", string(data), weird)
	}
}

// {{.Raw.Path}} bypasses auto-quoting — the explicit unsafe path. With
// a `;rm sentinel` payload the sentinel SHOULD get deleted, proving
// the escape hatch actually delivers raw substitution.
func TestOnFileEdit_RawNamespaceIsUnquoted(t *testing.T) {
	tmp := t.TempDir()
	sentinel := filepath.Join(tmp, "sentinel")
	if err := os.WriteFile(sentinel, []byte("alive"), 0o600); err != nil {
		t.Fatal(err)
	}
	maliciousPath := "foo; rm " + sentinel
	h := New(`echo -n {{.Raw.Path}} > /dev/null`, "")
	h.OnFileEdit(tmp, maliciousPath, "edit")

	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Errorf("Raw should NOT auto-quote: sentinel survived (Raw escape hatch broken)")
	}
}
