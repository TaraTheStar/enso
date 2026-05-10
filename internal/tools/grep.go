// SPDX-License-Identifier: AGPL-3.0-or-later

package tools

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// GrepTool searches file contents by regex.
type GrepTool struct{}

func (t GrepTool) Name() string { return "grep" }
func (t GrepTool) Description() string {
	return "Search file contents by regex. Args: pattern (string), path (string)"
}
func (t GrepTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"pattern": map[string]interface{}{"type": "string"},
			"path":    map[string]interface{}{"type": "string"},
		},
		"required": []string{"pattern"},
	}
}

func (t GrepTool) Run(ctx context.Context, args map[string]interface{}, ac *AgentContext) (Result, error) {
	pattern, _ := args["pattern"].(string)
	searchPath, _ := args["path"].(string)
	if searchPath == "" {
		searchPath = ac.Cwd
	}
	if abs, err := resolveRestricted(searchPath, ac); err != nil {
		return Result{}, fmt.Errorf("grep: %w", err)
	} else {
		searchPath = abs
	}

	if result := tryRG(ctx, searchPath, pattern, ac); result != nil {
		return *result, nil
	}

	re, err := regexp.Compile(pattern)
	if err != nil {
		return Result{}, fmt.Errorf("grep: invalid regex: %w", err)
	}

	var results []string
	err = filepath.Walk(searchPath, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || info.Size() > 1024*1024 {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		lines := strings.Split(string(data), "\n")
		for i, line := range lines {
			if re.MatchString(line) {
				results = append(results, fmt.Sprintf("%s:%d:%s", path, i+1, line))
			}
		}
		return nil
	})
	if err != nil {
		return Result{}, fmt.Errorf("grep walk: %w", err)
	}

	output := strings.Join(results, "\n")
	cacheKey := "grep:" + pattern + ":" + searchPath
	if output == "" {
		output = "no matches found"
		return Result{LLMOutput: output, FullOutput: output, DisplayOutput: output, Meta: ResultMeta{CacheKey: cacheKey}}, nil
	}
	cap := ac.OutputCaps.CapFor("grep")
	truncated, full := capTruncate(output, cap, ac.RecentUserHint)

	return Result{LLMOutput: truncated, FullOutput: full, DisplayOutput: grepDisplay(output), Meta: ResultMeta{CacheKey: cacheKey}}, nil
}

// grepDisplay summarizes ripgrep / walk output (one `path:line:content`
// per line) into "N match(es) in M file(s)". Counting is over the full
// pre-truncation output so the user sees real totals, not the post-cap
// view.
func grepDisplay(output string) string {
	lines := strings.Split(strings.TrimRight(output, "\n"), "\n")
	files := make(map[string]struct{}, len(lines))
	for _, ln := range lines {
		if i := strings.IndexByte(ln, ':'); i > 0 {
			files[ln[:i]] = struct{}{}
		}
	}
	return fmt.Sprintf("%d match%s in %d file%s",
		len(lines), matchPlural(len(lines)),
		len(files), plural(len(files)))
}

func tryRG(ctx context.Context, path, pattern string, ac *AgentContext) *Result {
	// `--` separates flags from positionals so a pattern like `-foo` isn't
	// parsed as a flag by ripgrep.
	cmd := exec.CommandContext(ctx, "rg", "--color", "never", "--line-number", "--", pattern, path)
	setProcessGroup(cmd)
	cmd.Cancel = func() error {
		return killProcessGroup(cmd.Process)
	}
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cacheKey := "grep:" + pattern + ":" + path
	if err := cmd.Run(); err != nil {
		if _, ok := err.(*exec.Error); ok {
			return nil
		}
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return &Result{LLMOutput: "no matches found", FullOutput: "no matches found", Meta: ResultMeta{CacheKey: cacheKey}}
		}
		return &Result{LLMOutput: fmt.Sprintf("rg error: %v", err), FullOutput: fmt.Sprintf("rg error: %v", err), Meta: ResultMeta{CacheKey: cacheKey}}
	}

	output := stdout.String()
	if output == "" {
		return &Result{LLMOutput: "no matches found", FullOutput: "no matches found", Meta: ResultMeta{CacheKey: cacheKey}}
	}
	cap := ac.OutputCaps.CapFor("grep")
	truncated, full := capTruncate(output, cap, ac.RecentUserHint)
	return &Result{LLMOutput: truncated, FullOutput: full, DisplayOutput: grepDisplay(output), Meta: ResultMeta{CacheKey: cacheKey}}
}
