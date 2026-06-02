// SPDX-License-Identifier: AGPL-3.0-or-later

package bubble

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/TaraTheStar/enso/internal/backend"
)

// imageAttachMaxBytes caps a single attached image. Mirrors the read
// tool's inline ceiling (readFullMaxBytes, 10 MiB): a vision model won't
// accept arbitrarily large images, and the bytes cross the worker seam
// as base64, so an unbounded attach would bloat one envelope.
const imageAttachMaxBytes = 10 << 20

// imageMentionMIME maps a recognised image file extension to its IANA
// type. Only the formats the provider adapters actually emit as image
// blocks (see llm marshaling + the read tool's imageMIME) — png, jpeg,
// gif, webp. Anything else is treated as a normal text mention.
var imageMentionMIME = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
}

// resolveImageMentions scans typed input for `@path` mentions that point
// at an image file and reads each into a backend.InputImage (host-side,
// so an isolated worker that can't see the host FS still gets the bytes).
// The `@path` text is left in the message as a human reference; only the
// bytes are added out-of-band.
//
// Returns the attachments, the display refs of what attached (for a
// "📎 attached …" confirmation), and any problems — emitted only when an
// image-EXTENSION mention existed but couldn't be attached (too large,
// unreadable). A non-image mention, or a path that doesn't exist, is
// silently left as plain text (the overwhelmingly common case: `@` is
// also a normal file reference the model may choose to read itself).
func resolveImageMentions(text, cwd string) (images []backend.InputImage, attached []string, problems []string) {
	seen := map[string]bool{}

	for _, tok := range strings.Fields(text) {
		if !strings.HasPrefix(tok, "@") || len(tok) == 1 {
			continue
		}
		ref := strings.TrimPrefix(tok, "@")
		// Trim trailing punctuation a sentence might append ("look at
		// @diagram.png.") without eating an extension dot.
		ref = strings.TrimRight(ref, ".,;:!?)")
		mime, ok := imageMentionMIME[strings.ToLower(filepath.Ext(ref))]
		if !ok {
			continue // not an image extension → ordinary text mention
		}

		abs := ref
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(cwd, ref)
		}
		abs = filepath.Clean(abs)
		if seen[abs] {
			continue
		}
		seen[abs] = true

		info, err := os.Stat(abs)
		if err != nil || info.IsDir() {
			continue // missing / not a file → leave as plain text
		}
		if info.Size() > imageAttachMaxBytes {
			problems = append(problems, fmt.Sprintf(
				"image @%s not attached: %s exceeds the %dMB limit",
				ref, humanSize(info.Size()), imageAttachMaxBytes>>20))
			continue
		}
		data, err := os.ReadFile(abs)
		if err != nil {
			problems = append(problems, fmt.Sprintf("image @%s not attached: %v", ref, err))
			continue
		}
		images = append(images, backend.InputImage{MIME: mime, Data: data})
		attached = append(attached, ref)
	}
	return images, attached, problems
}

// imageAttachNotice renders the "📎 attached …" confirmation for the
// successfully-attached image refs, or "" when nothing attached.
func imageAttachNotice(attached []string) string {
	if len(attached) == 0 {
		return ""
	}
	return "📎 attached " + strings.Join(attached, ", ")
}

// humanSize renders a byte count as a compact MB/KB string for notices.
func humanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1fMB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1fKB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%dB", n)
	}
}
