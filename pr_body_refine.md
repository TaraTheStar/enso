
## Summary
This PR performs a series of architectural refinements to improve stability, security, and performance. It consolidates history management, hardens the process lifecycle, and optimizes the TUI event loop.

## Key Changes

### 🛠️ Stability & Security: Exestage Locking
Implemented an advisory `flock` mechanism in `exestage`.
- **Problem:** A concurrent `enso prune` could delete a staged binary while a long-running worker was still executing it.
- **Fix:** `Acquire` takes a shared lock on a `.inuse.lock` file; `Sweep` attempts an exclusive lock and skips any snapshot currently being held.

### ⚡ Performance: TUI Event Coalescing
Introduced `coalesceDeltas` in the bus forwarding logic.
- **Problem:** High-frequency streaming (>100 tokens/sec) can flood the Bubble Tea `Update()` loop.
- **Fix:** Consecutive streaming delta events are merged into a single event per 16ms window (~60fps), significantly smoothing the UI.

### 🧹 Refinement: Unified History Management
Removed the in-memory `Transcripts` registry.
- **Reasoning:** History capture is now fully integrated into the `session.Writer` (persisted in the database). This prevents "split-brain" history where some subagent logs exist only in memory and others in the DB.

### 📦 Efficiency: Audit Log Optimization
Added truncation for bulky tool outputs in the audit (`events`) table.
- **Optimization:** Captures a summary (capped at 256 chars) in the audit logs to prevent database bloat, while preserving the full output in the primary `messages` and `tool_calls` tables.

### 🚀 Lifecycle Improvements
- **Lima Worker:** Refined the reaper logic to ensure a single `cmd.Wait()` call and correct process group cleanup (`SIGTERM` $	o$ `SIGKILL`).
- **Secret Scrubbing:** Expanded the list of environment variable patterns treated as secrets (e.g., `DATABASE_URL`, `MONGO_URI`).

## Verification Performed
- `go test ./...` passed.
- `go vet ./...` passed.
- `make check` passed.
