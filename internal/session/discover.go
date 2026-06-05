// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/TaraTheStar/enso/internal/llm"
)

// BashCmdStat aggregates the recorded output of one normalised bash command
// across the store. It powers the /discover report (R3): commands with a
// lot of accumulated raw output but no compression filter are the prime
// candidates to write a filter for next.
type BashCmdStat struct {
	// Command is the normalised invocation (argv[0] plus a subcommand when
	// present), e.g. "go test", "git status", "ls".
	Command string
	// Runs is how many times this command was recorded.
	Runs int
	// RawTokens is the summed token estimate of the full (pre-truncation,
	// pre-compression) output — the bloat a filter could attack.
	RawTokens int
	// ModelTokens is the summed token estimate of what the model actually
	// saw (the LLMOutput) — already reduced by truncation/compression.
	ModelTokens int
}

// ComputeBashOutputStats walks the recorded bash tool calls across all
// sessions (optionally limited to those updated since `since`) and returns
// per-command output aggregates, sorted by RawTokens descending (biggest
// bloat first). Token counts use the same 4-chars-per-token heuristic as
// llm.Estimate.
func ComputeBashOutputStats(s *Store, since time.Time) ([]BashCmdStat, error) {
	if s == nil || s.DB == nil {
		return nil, fmt.Errorf("discover: no store")
	}

	sinceUnix := int64(0)
	if !since.IsZero() {
		sinceUnix = since.Unix()
	}

	rows, err := s.DB.Query(
		`SELECT t.args, t.llm_output, t.full_output
		   FROM tool_calls t
		   JOIN sessions   se ON se.id = t.session_id
		  WHERE t.name = 'bash' AND se.updated_at >= ?`, sinceUnix,
	)
	if err != nil {
		return nil, fmt.Errorf("query bash tool_calls: %w", err)
	}
	defer rows.Close()

	agg := map[string]*BashCmdStat{}
	for rows.Next() {
		var argsJSON, llmOut, fullOut string
		if err := rows.Scan(&argsJSON, &llmOut, &fullOut); err != nil {
			return nil, err
		}
		cmd := bashCmdFromArgs(argsJSON)
		if cmd == "" {
			continue
		}
		key := NormalizeBashCommand(cmd)
		st := agg[key]
		if st == nil {
			st = &BashCmdStat{Command: key}
			agg[key] = st
		}
		st.Runs++
		// full_output is empty when it equalled llm_output (no
		// truncation); fall back to llm_output so the bloat estimate
		// still reflects the real size.
		raw := fullOut
		if raw == "" {
			raw = llmOut
		}
		st.RawTokens += estTokensStr(raw)
		st.ModelTokens += estTokensStr(llmOut)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]BashCmdStat, 0, len(agg))
	for _, st := range agg {
		out = append(out, *st)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].RawTokens != out[j].RawTokens {
			return out[i].RawTokens > out[j].RawTokens
		}
		return out[i].Command < out[j].Command
	})
	return out, nil
}

// bashCmdFromArgs pulls the `cmd` string out of a tool_calls.args JSON blob.
func bashCmdFromArgs(argsJSON string) string {
	if argsJSON == "" {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &m); err != nil {
		return ""
	}
	cmd, _ := m["cmd"].(string)
	return cmd
}

// NormalizeBashCommand reduces a full command line to the granularity a
// compression filter keys on: argv[0], plus the first non-flag argument
// when it reads like a subcommand (git status, go test, npm install). Pipes
// and shell operators are cut at the first segment so "git diff | head"
// normalises to "git diff".
func NormalizeBashCommand(cmd string) string {
	cmd = strings.TrimSpace(cmd)
	// Take the first pipeline/command segment only.
	if i := strings.IndexAny(cmd, "|&;\n"); i >= 0 {
		cmd = cmd[:i]
	}
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return ""
	}
	base := fields[0]
	// Drop an env-var prefix like FOO=bar before the real argv[0].
	for len(fields) > 1 && strings.Contains(base, "=") {
		fields = fields[1:]
		base = fields[0]
	}
	// Only fold in a subcommand for a bare command name (git, go, npm) —
	// a path-like argv[0] (./scripts/run.sh, /usr/bin/x) is specific
	// enough on its own.
	if len(fields) > 1 && !strings.Contains(base, "/") {
		sub := fields[1]
		if sub != "" && !strings.HasPrefix(sub, "-") && !strings.Contains(sub, "/") {
			return base + " " + sub
		}
	}
	return base
}

// estTokensStr mirrors the local 4-chars/token heuristic on a raw string.
func estTokensStr(s string) int {
	return llm.Estimate([]llm.Message{{Content: s}})
}
