// SPDX-License-Identifier: AGPL-3.0-or-later

package tools

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/TaraTheStar/enso/internal/llm"
)

const (
	// readFullMaxBytes is the size at/under which a file is read whole
	// into memory (the fast path: exact line counts + image inlining).
	// Above it, read streams a bounded window instead of slurping — a
	// multi-GB file or log would otherwise OOM the process at
	// os.ReadFile, before any output cap could run.
	readFullMaxBytes = 10 << 20 // 10 MiB
	// readStreamMaxBytes / readStreamMaxLines bound the window returned
	// when streaming a large file, so even an unbounded range (no
	// last_line) on a huge file stays memory-safe. The output cap then
	// trims further for the model.
	readStreamMaxBytes = 10 << 20 // 10 MiB
	readStreamMaxLines = 50000
)

// ReadTool reads a file or a line range.
type ReadTool struct{}

func (t ReadTool) Name() string { return "read" }
func (t ReadTool) Description() string {
	return "Read a file or a line range. Also reads images (png, jpeg, gif, webp) — pass an image path and a vision-capable model sees the image directly. Args: path (string), first_line (int), last_line (int)"
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

	info, err := os.Stat(abs)
	if err != nil {
		return Result{}, fmt.Errorf("read %s: %w", abs, err)
	}
	if info.IsDir() {
		return Result{}, fmt.Errorf("read %s: is a directory", abs)
	}

	// Large files take the streaming path: reading them whole would
	// OOM before any output cap runs. The fast path below keeps exact
	// line counts and image inlining for normal-sized files.
	if info.Size() > readFullMaxBytes {
		return t.runLarge(abs, info.Size(), args, ac)
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

// runLarge handles files above readFullMaxBytes by streaming only the
// requested line window, so a multi-GB file can't OOM the process. It
// never inlines images (a >10 MiB image is rejected by every provider
// anyway — surfaced as a notice so the model doesn't get binary garbage)
// and reports line numbers without an exact total (counting every line
// would defeat the point of not reading the whole file).
func (t ReadTool) runLarge(abs string, size int64, args map[string]interface{}, ac *AgentContext) (Result, error) {
	ac.ReadSet[abs] = true

	// Refuse to inline a large image rather than streaming its bytes as
	// numbered "text" (which would be garbage).
	if _, isImg := imageExtMIME(abs); isImg {
		msg := fmt.Sprintf("[image too large to inline: %s, %s]", filepath.Base(abs), humanBytes(int(size)))
		return Result{
			LLMOutput:     msg,
			FullOutput:    msg,
			DisplayOutput: "image too large to inline",
			Meta:          ResultMeta{PathsRead: []string{abs}, CacheKey: fmt.Sprintf("read:%s:bigimage", abs)},
		}, nil
	}

	first := 1
	if fl, ok := args["first_line"].(float64); ok && int(fl) > first {
		first = int(fl)
	}
	last := math.MaxInt
	if ll, ok := args["last_line"].(float64); ok {
		last = int(ll)
	}
	if first > last {
		first, last = last, first
	}
	if first < 1 {
		first = 1
	}

	lines, capped, err := readFileWindow(abs, first, last, readStreamMaxBytes, readStreamMaxLines)
	if err != nil {
		return Result{}, fmt.Errorf("read %s: %w", abs, err)
	}

	var sb strings.Builder
	for i, ln := range lines {
		fmt.Fprintf(&sb, "%6d  %s\n", first+i, ln)
	}
	truncated, full := truncateWithRecovery(ac, "read", sb.String())

	if ac.InstructionResolver != nil {
		if reminder := ac.InstructionResolver.ResolveOnRead(abs); reminder != "" {
			truncated = truncated + "\n\n" + reminder
		}
	}

	returned := len(lines)
	end := first + returned - 1
	note := ""
	if capped {
		note = ", capped"
	}
	display := fmt.Sprintf("lines %d-%d (large file, %s%s)", first, end, humanBytes(int(size)), note)

	return Result{
		LLMOutput:     truncated,
		FullOutput:    full,
		DisplayOutput: display,
		Meta: ResultMeta{
			PathsRead: []string{abs},
			CacheKey:  fmt.Sprintf("read:%s:%d-%d:big", abs, first, end),
		},
	}, nil
}

// readFileWindow streams [first,last] (1-based, inclusive) from path
// without loading the whole file. It bounds the result to maxBytes and
// maxLines (capped=true when either trips, or a single line is clipped),
// and uses bufio.Reader (not Scanner) so a single pathologically long
// line is clipped rather than erroring with bufio.ErrTooLong.
func readFileWindow(path string, first, last, maxBytes, maxLines int) (lines []string, capped bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()

	r := bufio.NewReader(f)
	budget := maxBytes
	lineNo := 0
	for {
		line, rerr := r.ReadString('\n')
		if line != "" {
			lineNo++
			if lineNo >= first && lineNo <= last {
				line = strings.TrimRight(line, "\r\n")
				if len(line) > budget {
					line = line[:budget]
					capped = true
				}
				budget -= len(line)
				lines = append(lines, line)
				if len(lines) >= maxLines || budget <= 0 {
					capped = true
					break
				}
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				break
			}
			return nil, false, rerr
		}
		if lineNo >= last {
			break
		}
	}
	return lines, capped, nil
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
	if mime, ok := imageExtMIME(path); ok {
		return mime, true
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

// imageExtMIME maps a recognised image file extension to its MIME type
// (the cheap, name-based half of imageMIME — no bytes needed). Used by
// the large-file path to refuse inlining an oversized image without
// reading it.
func imageExtMIME(path string) (string, bool) {
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
	return "", false
}
