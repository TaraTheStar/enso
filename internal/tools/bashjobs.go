// SPDX-License-Identifier: AGPL-3.0-or-later

package tools

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"
)

// maxBackgroundBuf is the per-job retained output ceiling. A long-running
// background server can emit megabytes; we keep only the most recent
// window so a forgotten job can't grow the host's memory without bound.
// The read cursor is adjusted when the head is trimmed so bash_output
// never returns a torn or shifted slice.
const maxBackgroundBuf = 1 << 20 // 1 MiB

// BashJobs is a registry of background shell commands started via the
// bash tool's run_in_background mode. Safe for concurrent use. Each agent
// owns its own registry (see AgentContext.BashJobs).
type BashJobs struct {
	mu     sync.Mutex
	nextID int
	jobs   map[string]*bashJob
}

// NewBashJobs returns an empty registry.
func NewBashJobs() *BashJobs {
	return &BashJobs{jobs: map[string]*bashJob{}}
}

// bashJob tracks one background command: its process, a bounded combined
// stdout+stderr buffer with a per-reader cursor, and completion state.
type bashJob struct {
	id     string
	cmdStr string
	cmd    *exec.Cmd

	mu     sync.Mutex
	out    []byte // combined stdout+stderr, trimmed to maxBackgroundBuf
	cursor int    // bytes already returned by Output
	done   bool
	exit   int
}

// Write appends combined output, trimming the oldest bytes past the cap
// and shifting the read cursor to match. Satisfies io.Writer so it can
// back a progressWriter for live streaming, like the foreground path.
func (j *bashJob) Write(p []byte) (int, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.out = append(j.out, p...)
	if over := len(j.out) - maxBackgroundBuf; over > 0 {
		j.out = j.out[over:]
		if j.cursor -= over; j.cursor < 0 {
			j.cursor = 0
		}
	}
	return len(p), nil
}

// Start launches cmdStr in its own process group, registers the job, and
// returns immediately with the assigned id. A goroutine reaps the process
// and records its exit status.
func (r *BashJobs) Start(cmdStr string, ac *AgentContext) (Result, error) {
	cmd := exec.Command("sh", "-c", cmdStr)
	cmd.Dir = ac.Cwd
	// Scrub secret-shaped vars from the child env (S9), exactly like the
	// foreground path — otherwise run_in_background + bash_output would be
	// a trivial bypass of the foreground scrub.
	cmd.Env = scrubbedBashEnv(os.Environ())
	// Own process group so Kill/KillAll reaps the whole pipeline, matching
	// the foreground path's cancel semantics.
	setProcessGroup(cmd)
	// Stdout/Stderr are in-process writers, so Wait blocks until the pipe
	// copy goroutines see EOF. A grandchild that escaped the process group
	// (setsid, nohup &) keeps the write end open past the group kill;
	// WaitDelay force-closes the pipes after the grace window so the reaper
	// goroutine below can't leak.
	cmd.WaitDelay = 5 * time.Second

	r.mu.Lock()
	r.nextID++
	id := fmt.Sprintf("bg_%d", r.nextID)
	r.mu.Unlock()

	job := &bashJob{id: id, cmdStr: cmdStr, cmd: cmd}
	cmd.Stdout = newProgressWriter(job, ac.Bus, ac.CurrentToolID, "stdout")
	cmd.Stderr = newProgressWriter(job, ac.Bus, ac.CurrentToolID, "stderr")

	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("bash: start background: %w", err)
	}

	r.mu.Lock()
	r.jobs[id] = job
	r.mu.Unlock()

	go func() {
		_ = cmd.Wait()
		job.mu.Lock()
		job.done = true
		if cmd.ProcessState != nil {
			job.exit = cmd.ProcessState.ExitCode()
		}
		job.mu.Unlock()
	}()

	return Result{
		LLMOutput: fmt.Sprintf("started background job %s\n"+
			"Read its output with bash_output id=%q; stop it with bash_kill id=%q.",
			id, id, id),
		Meta: ResultMeta{CacheKey: "bash_bg:" + id},
	}, nil
}

// Output returns the output appended since the last Output call for this
// job, plus its current status. Unknown ids return a normal (non-error)
// Result so the model can recover.
func (r *BashJobs) Output(id string) (Result, error) {
	r.mu.Lock()
	job := r.jobs[id]
	r.mu.Unlock()
	if job == nil {
		return Result{LLMOutput: fmt.Sprintf("no background job with id %q", id)}, nil
	}

	job.mu.Lock()
	var fresh string
	if job.cursor < len(job.out) {
		fresh = string(job.out[job.cursor:])
		job.cursor = len(job.out)
	}
	status := "running"
	if job.done {
		status = fmt.Sprintf("exited %d", job.exit)
	}
	job.mu.Unlock()

	body := fresh
	if body == "" {
		body = "(no new output)"
	}
	return Result{
		LLMOutput:  fmt.Sprintf("[%s] %s\n%s", id, status, body),
		FullOutput: body,
	}, nil
}

// Kill SIGKILLs the job's process group. Already-finished or unknown jobs
// return an informational (non-error) Result.
func (r *BashJobs) Kill(id string) (Result, error) {
	r.mu.Lock()
	job := r.jobs[id]
	r.mu.Unlock()
	if job == nil {
		return Result{LLMOutput: fmt.Sprintf("no background job with id %q", id)}, nil
	}

	job.mu.Lock()
	done := job.done
	job.mu.Unlock()
	if done {
		return Result{LLMOutput: fmt.Sprintf("background job %s already finished", id)}, nil
	}
	if err := killProcessGroup(job.cmd.Process); err != nil {
		return Result{LLMOutput: fmt.Sprintf("failed to kill job %s: %v", id, err)}, nil
	}
	return Result{LLMOutput: fmt.Sprintf("killed background job %s", id)}, nil
}

// KillAll SIGKILLs every still-running job. Called from the agent's
// teardown so local-backend background jobs don't outlive the session.
// nil-safe so callers needn't guard.
func (r *BashJobs) KillAll() {
	if r == nil {
		return
	}
	r.mu.Lock()
	jobs := make([]*bashJob, 0, len(r.jobs))
	for _, j := range r.jobs {
		jobs = append(jobs, j)
	}
	r.mu.Unlock()

	for _, j := range jobs {
		j.mu.Lock()
		alive := !j.done && j.cmd != nil
		j.mu.Unlock()
		if alive {
			_ = killProcessGroup(j.cmd.Process)
		}
	}
}

// BashOutputTool reads new output from a background bash job.
type BashOutputTool struct{}

func (BashOutputTool) Name() string { return "bash_output" }
func (BashOutputTool) Description() string {
	return "Read newly produced output (and status) from a background bash job started with run_in_background. Args: id (string). Returns only output since the previous bash_output call."
}
func (BashOutputTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id": map[string]any{"type": "string", "description": "Background job id returned by bash run_in_background"},
		},
		"required": []string{"id"},
	}
}
func (BashOutputTool) Run(_ context.Context, args map[string]any, ac *AgentContext) (Result, error) {
	id, _ := args["id"].(string)
	if id == "" {
		return Result{}, fmt.Errorf("bash_output: id required")
	}
	if ac.BashJobs == nil {
		return Result{LLMOutput: "background jobs are not available in this context"}, nil
	}
	return ac.BashJobs.Output(id)
}

// BashKillTool stops a background bash job.
type BashKillTool struct{}

func (BashKillTool) Name() string { return "bash_kill" }
func (BashKillTool) Description() string {
	return "Stop (SIGKILL) a background bash job started with run_in_background. Args: id (string)."
}
func (BashKillTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id": map[string]any{"type": "string", "description": "Background job id returned by bash run_in_background"},
		},
		"required": []string{"id"},
	}
}
func (BashKillTool) Run(_ context.Context, args map[string]any, ac *AgentContext) (Result, error) {
	id, _ := args["id"].(string)
	if id == "" {
		return Result{}, fmt.Errorf("bash_kill: id required")
	}
	if ac.BashJobs == nil {
		return Result{LLMOutput: "background jobs are not available in this context"}, nil
	}
	return ac.BashJobs.Kill(id)
}
