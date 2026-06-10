// SPDX-License-Identifier: AGPL-3.0-or-later

package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/TaraTheStar/enso/internal/bus"
)

func TestLooksNonTerminating(t *testing.T) {
	flagged := []string{
		"tail -f /var/log/app.log",
		"tail -F app.log",
		"tail --follow=name x.log",
		"watch -n1 ls",
		"watch kubectl get pods",
		"journalctl -f",
		"journalctl -u nginx -f",
		"kubectl logs -f mypod",
		"docker logs --follow web",
		"npm run dev",
		"npm start",
		"pnpm dev",
		"yarn serve",
		"vite",
		"next dev",
		"ng serve",
		"hugo server -D",
		"jekyll serve",
		"python -m http.server 8000",
		"python3 -m http.server",
		"php -S localhost:8080",
		"rails server",
		"bin/rails s",
		"flask run",
		"./manage.py runserver 0:8000",
		"FOO=bar npm run dev", // env prefix
	}
	for _, c := range flagged {
		if _, ok := looksNonTerminating(c); !ok {
			t.Errorf("expected %q to be flagged as non-terminating", c)
		}
	}

	notFlagged := []string{
		"echo hello",
		"ls -la",
		"go test ./...",
		"tail -n 100 app.log",       // bounded read, not follow
		"grep -rf pattern .",        // -f here is grep's file flag, not tail
		"cat watch.go",              // filename, not the watch command
		"timeout 5 tail -f app.log", // already bounded by a wrapper
		"tail -f app.log | head -5", // pipe into a terminating consumer
		"npm run build",
		"npm test",
		"nohup npm run dev &",       // explicitly detached
		"npm run dev &",             // backgrounded
		"vitest run",                // vitest, not vite
		"git commit -m 'add watch'", // 'watch' inside a quoted arg, mid-token
	}
	for _, c := range notFlagged {
		if reason, ok := looksNonTerminating(c); ok {
			t.Errorf("expected %q NOT to be flagged, got %q", c, reason)
		}
	}
}

func TestLooksLikePolling(t *testing.T) {
	flagged := []string{
		"sleep 25 && cat output.log", // the classic poll-then-check
		"sleep 30",                   // a bare long wait
		"sleep 10",                   // exactly at the threshold
		"sleep 1m",                   // 60s via suffix
		"sleep 2h",                   // hours
		"SLEEP 15",                   // case-insensitive
		"while ! curl -s localhost:8080; do sleep 2; done", // poll loop, short sleeps chained
		"until nc -z localhost 5432; do sleep 1; done",     // until poll loop
	}
	for _, c := range flagged {
		if _, ok := looksLikePolling(c); !ok {
			t.Errorf("expected %q to be flagged as polling", c)
		}
	}

	notFlagged := []string{
		"echo hello",
		"sleep 2 && curl localhost:8080", // short startup wait — under threshold
		"sleep 5",                        // under threshold
		"sleep 0.5",                      // fractional, under threshold
		"go build ./... && sleep 30",     // trailing sleep (keep-alive), not leading
		"timeout 60 sleep 120",           // bounded by a wrapper
		"sleep 30 &",                     // backgrounded, doesn't block
		"for i in 1 2 3; do curl x; sleep 1; done", // finite for-loop, not open polling
		"cat sleep.txt", // 'sleep' is a filename
	}
	for _, c := range notFlagged {
		if reason, ok := looksLikePolling(c); ok {
			t.Errorf("expected %q NOT to be flagged, got %q", c, reason)
		}
	}
}

func TestLeadingSleepSeconds(t *testing.T) {
	cases := []struct {
		cmd  string
		secs float64
		ok   bool
	}{
		{"sleep 25", 25, true},
		{"sleep 1m", 60, true},
		{"sleep 2h", 7200, true},
		{"sleep 1d", 86400, true},
		{"sleep 0.5", 0.5, true},
		{"  sleep 3s ", 3, true},
		{"echo hi", 0, false},
		{"go build && sleep 5", 0, false}, // not leading
	}
	for _, tc := range cases {
		secs, ok := leadingSleepSeconds(tc.cmd)
		if ok != tc.ok || (ok && secs != tc.secs) {
			t.Errorf("leadingSleepSeconds(%q) = (%v,%v), want (%v,%v)", tc.cmd, secs, ok, tc.secs, tc.ok)
		}
	}
}

// TestBashTool_NudgesPollingSleep asserts a foreground polling sleep is not
// executed and the nudge points at bash_output.
func TestBashTool_NudgesPollingSleep(t *testing.T) {
	ac := &AgentContext{Cwd: t.TempDir(), Bus: bus.New(), BashJobs: NewBashJobs()}
	res, err := BashTool{}.Run(
		context.Background(),
		map[string]any{"cmd": "sleep 30 && cat out.log"},
		ac,
	)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.HasPrefix(res.LLMOutput, "not run") {
		t.Errorf("expected a not-run nudge, got %q", res.LLMOutput)
	}
	if !strings.Contains(res.LLMOutput, "bash_output") {
		t.Errorf("nudge should point at bash_output, got %q", res.LLMOutput)
	}
}

// TestBashTool_NudgesNonTerminating asserts the foreground nudge fires
// (the command is NOT executed) and points at run_in_background.
func TestBashTool_NudgesNonTerminating(t *testing.T) {
	ac := &AgentContext{Cwd: t.TempDir(), Bus: bus.New(), BashJobs: NewBashJobs()}
	res, err := BashTool{}.Run(
		context.Background(),
		map[string]any{"cmd": "tail -f /etc/hostname"},
		ac,
	)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.HasPrefix(res.LLMOutput, "not run") {
		t.Errorf("expected a not-run nudge, got %q", res.LLMOutput)
	}
	if !strings.Contains(res.LLMOutput, "run_in_background") {
		t.Errorf("nudge should point at run_in_background, got %q", res.LLMOutput)
	}
}

// TestBashTool_ExplicitTimeoutSkipsNudge confirms an explicit `timeout`
// arg means "I accept the bound, run it anyway" — the nudge is skipped and
// the command actually runs (and is killed at the short bound).
func TestBashTool_ExplicitTimeoutSkipsNudge(t *testing.T) {
	ac := &AgentContext{Cwd: t.TempDir(), Bus: bus.New(), BashJobs: NewBashJobs()}
	res, err := BashTool{}.Run(
		context.Background(),
		map[string]any{"cmd": "tail -f /etc/hostname", "timeout": 1},
		ac,
	)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.HasPrefix(res.LLMOutput, "not run") {
		t.Errorf("explicit timeout should bypass the nudge and run the command, got %q", res.LLMOutput)
	}
	if !strings.Contains(res.LLMOutput, "timed out") {
		t.Errorf("expected the bounded command to time out, got %q", res.LLMOutput)
	}
}
