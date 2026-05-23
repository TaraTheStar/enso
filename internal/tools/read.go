// SPDX-License-Identifier: AGPL-3.0-or-later

package tools

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/TaraTheStar/enso/internal/llm"
)

// ReadTool reads a file or a line range.
type ReadTool struct{}

func (t ReadTool) Name() string { return "read" }
func (t ReadTool) Description() string {
	return "Read a file or a line range. Args: path (string), first_line (int), last_line (int)"
}
func (t ReadTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"path":       map[string]interface{}{"type": "string"},
			"first_line": map[string]interface{}{"type": "integer"},
			"last_line":  map[string]interface{}{"type": "integer"},
		},
		"required": []string{"path"},
	}
}

func (t ReadTool) Run(ctx context.Context, args map[string]interface{}, ac *AgentContext) (Result, error) {
	path, _ := args["path"].(string)
	abs, err := resolveRestricted(path, ac)
	if err != nil {
		return Result{}, fmt.Errorf("read: %w", err)
	}

	data, err := os.ReadFile(abs)
	if err != nil {
		return Result{}, fmt.Errorf("read %s: %w", abs, err)
	}

	ac.ReadSet[abs] = true

	// Image short-circuit: when the file is a recognised image type
	// we hand the bytes to the model as a multimodal Part instead of
	// rendering binary as numbered text. The model gets to "see" the
	// image directly on adapters that support multimodal; adapters
	// that don't will surface a clear error rather than a corrupt
	// transcript.
	if mime, ok := imageMIME(abs, data); ok {
		summary := fmt.Sprintf("[image: %s, mime=%s, %d bytes]", filepath.Base(abs), mime, len(data))
		return Result{
			LLMOutput:     summary,
			FullOutput:    summary,
			DisplayOutput: fmt.Sprintf("image, %s", humanBytes(len(data))),
			Parts: []llm.MessagePart{
				llm.NewImagePart(mime, data),
			},
			Meta: ResultMeta{
				PathsRead: []string{abs},
				CacheKey:  fmt.Sprintf("read:%s:image", abs),
			},
		}, nil
	}

	lines := strings.Split(string(data), "\n")

	first := 1
	if fl, ok := args["first_line"].(float64); ok {
		first = int(fl)
	}
	last := len(lines)
	if ll, ok := args["last_line"].(float64); ok {
		last = int(ll)
	}

	if first > last {
		first, last = last, first
	}
	if first < 1 {
		first = 1
	}
	if last > len(lines) {
		last = len(lines)
	}

	var sb strings.Builder
	for i := first - 1; i < last; i++ {
		sb.WriteString(fmt.Sprintf("%6d  %s\n", i+1, lines[i]))
	}

	content := sb.String()
	truncated, full := truncateWithRecovery(ac, "read", content)

	// Contextual injection: when the read surfaces a file living under
	// a subdir with its own ENSO.md / AGENTS.md that the static system
	// prompt didn't include, append that content as a system reminder.
	// The resolver tracks per-session "already injected" so the same
	// file's instructions only land once.
	if ac.InstructionResolver != nil {
		if reminder := ac.InstructionResolver.ResolveOnRead(abs); reminder != "" {
			truncated = truncated + "\n\n" + reminder
		}
	}

	// File contents are huge; the call signature already shows the path
	// (and any range args). Scrollback gets a count instead of pages of
	// numbered source. The model still receives `truncated` as before.
	totalLines := len(lines)
	returned := last - first + 1
	var display string
	if first == 1 && last == totalLines {
		display = fmt.Sprintf("%d line%s", returned, plural(returned))
	} else {
		display = fmt.Sprintf("lines %d-%d (of %d)", first, last, totalLines)
	}

	cacheKey := fmt.Sprintf("read:%s:%d-%d", abs, first, last)
	return Result{
		LLMOutput:     truncated,
		FullOutput:    full,
		DisplayOutput: display,
		Meta: ResultMeta{
			PathsRead: []string{abs},
			CacheKey:  cacheKey,
		},
	}, nil
}

// imageMIME tries the file extension first (cheap, source-of-truth
// for what the user named the file) and falls back to a magic-byte
// sniff (handles renamed-PNG-with-jpg-extension, etc.). Restricted to
// formats Bedrock + Anthropic + Vertex all accept; uncommon types
// (heic, avif, tiff) fail the test on purpose so they route through
// the normal text-read path with its visible "binary garbage" symptom
// instead of getting silently passed to a model that may or may not
// handle them.
func imageMIME(path string, data []byte) (string, bool) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png":
		return "image/png", true
	case ".jpg", ".jpeg":
		return "image/jpeg", true
	case ".gif":
		return "image/gif", true
	case ".webp":
		return "image/webp", true
	}
	// Sniff fallback: http.DetectContentType reads the first 512
	// bytes. Same supported set as the extension switch so the
	// behaviour is consistent either way.
	sniff := http.DetectContentType(data)
	switch sniff {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return sniff, true
	}
	return "", false
}
