// SPDX-License-Identifier: AGPL-3.0-or-later

// Command runner is the enso eval harness. It walks eval/tasks/*/task.json,
// runs each task against each requested model via `enso run --format json`,
// scores the resulting event stream, runs each task's check.sh against the
// resulting workdir, and writes a CSV of results.
//
// Usage:
//
//	go run ./eval/cmd/runner \
//	  --enso ./bin/enso \
//	  --models gemma4-31b,qwen3.6-27b \
//	  --tasks-dir eval/tasks \
//	  --output eval/results/run.csv
//
// The named models must exist as [providers] entries in the user's enso
// config. Pass --config <path> to layer in an additional config file.
package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/TaraTheStar/enso/eval/pkg/score"
)

// Task is the on-disk schema for tasks/<id>/task.json.
type Task struct {
	// ID is read from the directory name; task.json may omit it.
	ID string `json:"-"`

	// Description is human-readable; logged but not scored.
	Description string `json:"description"`

	// Prompt is the user message handed to enso run.
	Prompt string `json:"prompt"`

	// MaxTurns caps the agent loop. 0 lets enso pick its default.
	MaxTurns int `json:"max_turns,omitempty"`

	// InitCmds run sequentially in the temp workdir before the agent
	// starts. Use for `git init`, seeding env vars, etc. Each entry is
	// passed to /bin/sh -c.
	InitCmds []string `json:"init_cmds,omitempty"`

	// ExpectedTools is the set of tool names the model should be able to
	// call. Tool calls outside this set are recorded as hallucinations.
	// If empty, hallucination tracking is skipped.
	ExpectedTools []string `json:"expected_tools,omitempty"`

	// Swap, if non-nil, runs a second prompt against the same session
	// after the first run completes — optionally with a different
	// provider, simulating mid-session model switching.
	Swap *SwapStep `json:"swap,omitempty"`
}

// SwapStep is the optional second turn that exercises the resume +
// model-switch path. The runner picks the swap-target model from the CLI's
// --swap-model flag (or, if absent, falls back to the next model in
// --models after the first). NewModelOverride on the task takes precedence.
type SwapStep struct {
	Prompt           string `json:"prompt"`
	MaxTurns         int    `json:"max_turns,omitempty"`
	NewModelOverride string `json:"new_model,omitempty"`
}

// Result is one row of the CSV.
type Result struct {
	Timestamp     time.Time
	TaskID        string
	Model         string
	SwapModel     string // "" if no swap
	Pass          bool
	HitMaxTurns   bool
	WallSeconds   float64
	CheckExitCode int
	CheckOutput   string
	score.Metrics // first-leg metrics
	SwapMetrics   *score.Metrics
}

func main() {
	var (
		ensoBin   = flag.String("enso", "./bin/enso", "path to the enso binary")
		modelsCSV = flag.String("models", "", "comma-separated provider names (must match [providers] keys in enso config)")
		swapModel = flag.String("swap-model", "", "for tasks with a swap step, switch to this provider after step 1 (default: second entry in --models)")
		tasksDir  = flag.String("tasks-dir", "eval/tasks", "directory containing one subdir per task")
		filter    = flag.String("filter", "", "comma-separated task IDs to run (default: all)")
		output    = flag.String("output", "", "CSV output path (default: eval/results/run-<timestamp>.csv)")
		extraConf = flag.String("config", "", "extra config file passed to enso run via -c")
		verbose   = flag.Bool("v", false, "stream enso run JSON events to stderr while the agent works")
	)
	flag.Parse()

	if *modelsCSV == "" {
		die("--models is required (e.g. --models gemma4-31b,qwen3.6-27b)")
	}
	models := splitCSV(*modelsCSV)
	if *swapModel == "" && len(models) >= 2 {
		*swapModel = models[1]
	}
	if *extraConf == "" {
		die("--config is required: point at a TOML file with [providers] entries matching --models (typically ~/.config/enso/config.toml)")
	}
	absConf, err := filepath.Abs(*extraConf)
	if err != nil {
		die("resolve config path: %v", err)
	}
	if _, err := os.Stat(absConf); err != nil {
		die("config not found: %s", absConf)
	}

	tasks, err := loadTasks(*tasksDir, splitCSV(*filter))
	if err != nil {
		die("load tasks: %v", err)
	}
	if len(tasks) == 0 {
		die("no tasks matched (tasks-dir=%s filter=%s)", *tasksDir, *filter)
	}

	if _, err := os.Stat(*ensoBin); err != nil {
		die("enso binary not found at %s — run `make build` first or pass --enso", *ensoBin)
	}
	absEnso, err := filepath.Abs(*ensoBin)
	if err != nil {
		die("resolve enso path: %v", err)
	}

	outPath := *output
	if outPath == "" {
		outPath = filepath.Join("eval", "results", fmt.Sprintf("run-%s.csv", time.Now().Format("20060102-150405")))
	}
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		die("mkdir results: %v", err)
	}
	outFile, err := os.Create(outPath)
	if err != nil {
		die("create output: %v", err)
	}
	defer outFile.Close()
	w := csv.NewWriter(outFile)
	defer w.Flush()
	if err := w.Write(csvHeader()); err != nil {
		die("write csv header: %v", err)
	}

	probe := append([]string{}, models...)
	if *swapModel != "" {
		probe = append(probe, *swapModel)
	}
	if err := preflight(absConf, probe); err != nil {
		die("preflight: %v", err)
	}

	fmt.Fprintf(os.Stderr, "running %d task(s) × %d model(s) → %s\n", len(tasks), len(models), outPath)

	for _, t := range tasks {
		for _, model := range models {
			if t.Swap != nil && *swapModel == "" {
				fmt.Fprintf(os.Stderr, "  [skip] %s × %s: task has swap but no --swap-model set\n", t.ID, model)
				continue
			}
			res := runOne(absEnso, absConf, model, *swapModel, t, *verbose)
			if err := w.Write(res.toCSV()); err != nil {
				fmt.Fprintf(os.Stderr, "  write csv row: %v\n", err)
			}
			w.Flush()
			fmt.Fprintf(os.Stderr, "  %s\n", res.summary())
		}
	}
}

// runOne executes a single (task, model) cell and returns its Result.
// It never returns an error — failures (process error, scorer error, check
// failure) are recorded on the Result and the run continues.
func runOne(ensoBin, extraConf, model, swapModel string, t Task, verbose bool) Result {
	res := Result{
		Timestamp: time.Now(),
		TaskID:    t.ID,
		Model:     model,
	}
	if t.Swap != nil {
		// Resolved swap target: task-level override > CLI default.
		target := swapModel
		if t.Swap.NewModelOverride != "" {
			target = t.Swap.NewModelOverride
		}
		res.SwapModel = target
	}

	workdir, err := os.MkdirTemp("", "enso-eval-work-*")
	if err != nil {
		res.CheckOutput = fmt.Sprintf("mkdir workdir: %v", err)
		return res
	}
	// HOME is isolated per cell so each run has its own session DB and
	// cannot see the user's real config/data/state dirs. The user's full config gets
	// installed into the homedir's XDG location so it loads as the
	// "user" config layer, leaving the highest-priority -c slot free
	// for the eval's own overrides (sandbox=off, etc).
	homedir, err := os.MkdirTemp("", "enso-eval-home-*")
	if err != nil {
		os.RemoveAll(workdir)
		res.CheckOutput = fmt.Sprintf("mkdir homedir: %v", err)
		return res
	}
	if err := installUserConfig(homedir, extraConf); err != nil {
		os.RemoveAll(workdir)
		os.RemoveAll(homedir)
		res.CheckOutput = fmt.Sprintf("install user config: %v", err)
		return res
	}
	overridePath, err := writeOverrideConfig(homedir)
	if err != nil {
		os.RemoveAll(workdir)
		os.RemoveAll(homedir)
		res.CheckOutput = fmt.Sprintf("write override config: %v", err)
		return res
	}
	// Keep both dirs for postmortem on failure; clean up on pass.
	defer func() {
		if res.Pass {
			_ = os.RemoveAll(workdir)
			_ = os.RemoveAll(homedir)
		} else {
			fmt.Fprintf(os.Stderr, "    [keep] workdir=%s home=%s\n", workdir, homedir)
		}
	}()

	if err := copyDir(filepath.Join(taskDir(t.ID), "fixtures"), workdir); err != nil && !errors.Is(err, fs.ErrNotExist) {
		res.CheckOutput = fmt.Sprintf("copy fixtures: %v", err)
		return res
	}

	for _, cmd := range t.InitCmds {
		if err := runShell(workdir, cmd); err != nil {
			res.CheckOutput = fmt.Sprintf("init cmd %q: %v", cmd, err)
			return res
		}
	}

	// First leg.
	leg1Start := time.Now()
	stream1, stderr1, err := runEnso(ensoBin, overridePath, homedir, workdir, model, "", t.Prompt, t.MaxTurns, verbose)
	res.WallSeconds += time.Since(leg1Start).Seconds()
	if err != nil && !isExpectedToolError(err) {
		res.CheckOutput = fmt.Sprintf("enso run leg1: %v\nstderr: %s", err, strings.TrimSpace(string(stderr1)))
		res.HadError = true
		res.FinalError = firstLine(stderr1)
		return res
	}
	m1, perr := score.ParseStream(bytes.NewReader(stream1), t.ExpectedTools)
	if perr != nil {
		res.CheckOutput = fmt.Sprintf("parse leg1: %v\nstderr: %s", perr, strings.TrimSpace(string(stderr1)))
		return res
	}
	res.Metrics = m1
	// If we got nothing from the model — no session_start, no events at
	// all — the spawn failed before any work happened. Don't run
	// check.sh against unmodified fixtures and call it a content fail.
	if m1.SessionID == "" {
		res.CheckOutput = fmt.Sprintf("no events from enso run; stderr:\n%s", strings.TrimSpace(string(stderr1)))
		res.HadError = true
		res.FinalError = firstLine(stderr1)
		return res
	}
	if t.MaxTurns > 0 && m1.TurnCount >= t.MaxTurns {
		res.HitMaxTurns = true
	}

	// Optional swap leg.
	if t.Swap != nil && m1.SessionID != "" {
		leg2Start := time.Now()
		target := swapModel
		if t.Swap.NewModelOverride != "" {
			target = t.Swap.NewModelOverride
		}
		stream2, stderr2, err := runEnso(ensoBin, overridePath, homedir, workdir, target, m1.SessionID, t.Swap.Prompt, t.Swap.MaxTurns, verbose)
		res.WallSeconds += time.Since(leg2Start).Seconds()
		if err != nil && !isExpectedToolError(err) {
			res.CheckOutput = fmt.Sprintf("enso run leg2 (swap): %v\nstderr: %s", err, strings.TrimSpace(string(stderr2)))
			res.HadError = true
			res.FinalError = firstLine(stderr2)
			return res
		}
		m2, perr := score.ParseStream(bytes.NewReader(stream2), t.ExpectedTools)
		if perr != nil {
			res.CheckOutput = fmt.Sprintf("parse leg2: %v\nstderr: %s", perr, strings.TrimSpace(string(stderr2)))
			return res
		}
		res.SwapMetrics = &m2
		if t.Swap.MaxTurns > 0 && m2.TurnCount >= t.Swap.MaxTurns {
			res.HitMaxTurns = true
		}
	}

	// Run check.sh against final workdir state.
	checkPath := filepath.Join(taskDir(t.ID), "check.sh")
	if _, err := os.Stat(checkPath); err == nil {
		out, code := runCheck(workdir, checkPath)
		res.CheckExitCode = code
		res.CheckOutput = strings.TrimSpace(out)
		res.Pass = code == 0
	} else {
		res.CheckOutput = "(no check.sh — task is unverified)"
	}
	return res
}

// runEnso spawns `enso run` with the given parameters and returns the raw
// JSON event stream from stdout, plus whatever was written to stderr.
// HOME is set to homedir so the spawned process gets its own XDG dirs
// (session DB, log) isolated from the user's.
func runEnso(ensoBin, extraConf, homedir, cwd, provider, sessionID, prompt string, maxTurns int, verbose bool) ([]byte, []byte, error) {
	args := []string{"run", "--format", "json", "--yolo", "--trust-project", "--provider", provider, "-c", extraConf}
	if sessionID != "" {
		args = append(args, "--session", sessionID)
	}
	if maxTurns > 0 {
		args = append(args, "--max-turns", strconv.Itoa(maxTurns))
	}
	args = append(args, prompt)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, ensoBin, args...)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), "HOME="+homedir, "ENSO_TRUST_PROJECT=1")
	var stdout, stderr bytes.Buffer
	if verbose {
		cmd.Stdout = io.MultiWriter(&stdout, os.Stderr)
		cmd.Stderr = io.MultiWriter(&stderr, os.Stderr)
	} else {
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
	}
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

// runCheck runs <task>/check.sh in the workdir and returns its combined
// output and exit code.
func runCheck(workdir, checkPath string) (string, int) {
	abs, err := filepath.Abs(checkPath)
	if err != nil {
		return fmt.Sprintf("abs check.sh: %v", err), 127
	}
	cmd := exec.Command("/bin/sh", abs)
	cmd.Dir = workdir
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ee, ok := errors.AsType[*exec.ExitError](err); ok {
			return string(out), ee.ExitCode()
		}
		return fmt.Sprintf("%s\n(exec err: %v)", out, err), 127
	}
	return string(out), 0
}

func runShell(cwd, cmdline string) error {
	cmd := exec.Command("/bin/sh", "-c", cmdline)
	cmd.Dir = cwd
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// isExpectedToolError matches the "one or more tool calls errored" exit
// from enso run — we record tool errors as data, not as harness failures.
func isExpectedToolError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "tool calls errored")
}

func loadTasks(dir string, filter []string) ([]Task, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	allow := make(map[string]bool, len(filter))
	for _, id := range filter {
		allow[id] = true
	}
	var out []Task
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		id := e.Name()
		if len(allow) > 0 && !allow[id] {
			continue
		}
		path := filepath.Join(dir, id, "task.json")
		b, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "skip %s: %v\n", id, err)
			continue
		}
		var t Task
		if err := json.Unmarshal(b, &t); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		t.ID = id
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func taskDir(id string) string { return filepath.Join("eval", "tasks", id) }

func copyDir(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode().Perm())
	})
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func csvHeader() []string {
	return []string{
		"timestamp", "task", "model", "swap_model", "pass",
		"hit_max_turns", "wall_s", "check_exit",
		"turns", "tool_calls", "tool_errors", "hallucinated_tools",
		"assistant_bytes", "reasoning_bytes", "think_leak", "had_error", "final_error",
		"swap_turns", "swap_tool_calls", "swap_tool_errors", "swap_hallucinated", "swap_think_leak",
		"session_id", "check_output",
	}
}

func (r Result) toCSV() []string {
	row := []string{
		r.Timestamp.Format(time.RFC3339),
		r.TaskID,
		r.Model,
		r.SwapModel,
		strconv.FormatBool(r.Pass),
		strconv.FormatBool(r.HitMaxTurns),
		strconv.FormatFloat(r.WallSeconds, 'f', 2, 64),
		strconv.Itoa(r.CheckExitCode),
		strconv.Itoa(r.TurnCount),
		strconv.Itoa(r.ToolCalls),
		strconv.Itoa(r.ToolErrors),
		strings.Join(r.HallucinatedTools, "|"),
		strconv.Itoa(r.AssistantBytes),
		strconv.Itoa(r.ReasoningBytes),
		strconv.FormatBool(r.ThinkLeak),
		strconv.FormatBool(r.HadError),
		truncate(r.FinalError, 200),
	}
	if r.SwapMetrics != nil {
		s := r.SwapMetrics
		row = append(row,
			strconv.Itoa(s.TurnCount),
			strconv.Itoa(s.ToolCalls),
			strconv.Itoa(s.ToolErrors),
			strings.Join(s.HallucinatedTools, "|"),
			strconv.FormatBool(s.ThinkLeak),
		)
	} else {
		row = append(row, "", "", "", "", "")
	}
	row = append(row, r.SessionID, truncate(r.CheckOutput, 500))
	return row
}

func (r Result) summary() string {
	verdict := "FAIL"
	if r.Pass {
		verdict = "PASS"
	}
	swap := ""
	if r.SwapModel != "" {
		swap = " → " + r.SwapModel
	}
	return fmt.Sprintf("%s  %s × %s%s  turns=%d tools=%d errs=%d wall=%.1fs",
		verdict, r.TaskID, r.Model, swap, r.TurnCount, r.ToolCalls, r.ToolErrors, r.WallSeconds)
}

func firstLine(b []byte) string {
	s := string(b)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// installUserConfig copies the user's config into the per-cell HOME at
// $XDG_CONFIG_HOME/enso/config.toml so it loads as the "user" layer.
// We deliberately do NOT pass the user's config via -c, because -c is
// the highest-priority layer and we need that slot for our own overrides.
func installUserConfig(homedir, userConfig string) error {
	target := filepath.Join(homedir, ".config", "enso", "config.toml")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	data, err := os.ReadFile(userConfig)
	if err != nil {
		return err
	}
	return os.WriteFile(target, data, 0o600)
}

// writeOverrideConfig writes the eval's -c layer: forces the LOCAL
// backend (per-cell tempdir isolation already provides directory
// isolation, and rootless podman bootstrapping per fresh HOME exceeds
// the 60s Ensure timeout). Must pin [backend] type explicitly — it is
// the sole backend selector, so without this the eval would inherit
// the user's podman/lima backend.
func writeOverrideConfig(homedir string) (string, error) {
	path := filepath.Join(homedir, "eval-overrides.toml")
	body := `# generated by eval/cmd/runner — overrides on top of the user's config.
[backend]
type = "local"
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// preflight grep-checks the config for `[providers.<name>]` headers
// matching every requested model so we fail fast on misnamed providers
// instead of burning the full matrix on the same misconfiguration.
//
// This is a literal substring check, not a real TOML parse — quoted
// keys (`[providers."weird-name"]`) won't match. If that becomes a
// problem we can pull in a TOML parser.
func preflight(extraConf string, models []string) error {
	confBytes, err := os.ReadFile(extraConf)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	conf := string(confBytes)
	var missing []string
	for _, m := range models {
		if !strings.Contains(conf, "[providers."+m+"]") {
			missing = append(missing, m)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var present []string
	for line := range strings.SplitSeq(conf, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "[providers.") || !strings.HasSuffix(line, "]") {
			continue
		}
		// Take the top-level table name only: `[providers.foo]` → "foo",
		// `[providers.foo.sampler]` → skip (it's a sub-table of foo).
		body := strings.TrimSuffix(strings.TrimPrefix(line, "[providers."), "]")
		if strings.Contains(body, ".") {
			continue
		}
		if !seen[body] {
			seen[body] = true
			present = append(present, body)
		}
	}
	sort.Strings(present)
	return fmt.Errorf("provider(s) %v not found in %s\nproviders defined: %v",
		missing, extraConf, present)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "runner: "+format+"\n", args...)
	os.Exit(1)
}
