// SPDX-License-Identifier: AGPL-3.0-or-later

package bubble

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTemp(t *testing.T, dir, name string, data []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestResolveImageMentions_AttachesImage: an `@path.png` mention to an
// existing image is read into an InputImage with the right mime + bytes.
func TestResolveImageMentions_AttachesImage(t *testing.T) {
	cwd := t.TempDir()
	png := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 1, 2, 3}
	writeTemp(t, cwd, "shot.png", png)

	imgs, attached, problems := resolveImageMentions("look at @shot.png please", cwd)
	if len(problems) != 0 {
		t.Fatalf("unexpected problems: %v", problems)
	}
	if len(imgs) != 1 {
		t.Fatalf("want 1 image, got %d", len(imgs))
	}
	if imgs[0].MIME != "image/png" {
		t.Errorf("mime: got %q want image/png", imgs[0].MIME)
	}
	if !bytes.Equal(imgs[0].Data, png) {
		t.Errorf("image bytes not read verbatim")
	}
	// Confirmation lists the attached ref.
	if len(attached) != 1 || attached[0] != "shot.png" {
		t.Fatalf("attached refs = %v, want [shot.png]", attached)
	}
	if notice := imageAttachNotice(attached); !strings.Contains(notice, "📎") || !strings.Contains(notice, "shot.png") {
		t.Fatalf("confirmation notice = %q", notice)
	}
	if imageAttachNotice(nil) != "" {
		t.Errorf("empty attach should produce no notice")
	}
}

// TestResolveImageMentions_IgnoresNonImageAndMissing: a non-image
// mention and a missing image path produce no attachment and no notice
// (they stay plain text — the common case).
func TestResolveImageMentions_IgnoresNonImageAndMissing(t *testing.T) {
	cwd := t.TempDir()
	writeTemp(t, cwd, "main.go", []byte("package main"))

	imgs, attached, problems := resolveImageMentions("edit @main.go and @ghost.png", cwd)
	if len(imgs) != 0 {
		t.Fatalf("want 0 images (non-image + missing), got %d", len(imgs))
	}
	if len(attached) != 0 || len(problems) != 0 {
		t.Fatalf("missing/non-image should be silent, got attached=%v problems=%v", attached, problems)
	}
}

// TestResolveImageMentions_TooLargeNotice: an image-extension file over
// the cap is NOT attached but DOES produce a user-facing notice so the
// user understands why the model can't see it.
func TestResolveImageMentions_TooLargeNotice(t *testing.T) {
	cwd := t.TempDir()
	big := make([]byte, imageAttachMaxBytes+1)
	writeTemp(t, cwd, "huge.jpg", big)

	imgs, attached, problems := resolveImageMentions("@huge.jpg", cwd)
	if len(imgs) != 0 || len(attached) != 0 {
		t.Fatalf("oversized image should not attach, got imgs=%d attached=%v", len(imgs), attached)
	}
	if len(problems) != 1 || !strings.Contains(problems[0], "huge.jpg") || !strings.Contains(problems[0], "limit") {
		t.Fatalf("want a too-large problem mentioning the file, got %v", problems)
	}
}

// TestResolveImageMentions_DedupAndMultiple: the same image twice is
// attached once; distinct images each attach.
func TestResolveImageMentions_DedupAndMultiple(t *testing.T) {
	cwd := t.TempDir()
	writeTemp(t, cwd, "a.png", []byte{1})
	writeTemp(t, cwd, "b.webp", []byte{2})

	imgs, _, _ := resolveImageMentions("@a.png vs @b.webp vs @a.png again", cwd)
	if len(imgs) != 2 {
		t.Fatalf("want 2 (deduped), got %d", len(imgs))
	}
	if imgs[0].MIME != "image/png" || imgs[1].MIME != "image/webp" {
		t.Errorf("mimes: got %q,%q", imgs[0].MIME, imgs[1].MIME)
	}
}

// TestResolveImageMentions_TrailingPunctuation: a mention followed by a
// sentence punctuation mark still resolves (the dot before the extension
// is preserved).
func TestResolveImageMentions_TrailingPunctuation(t *testing.T) {
	cwd := t.TempDir()
	writeTemp(t, cwd, "diagram.gif", []byte{0})

	imgs, _, _ := resolveImageMentions("see @diagram.gif.", cwd)
	if len(imgs) != 1 || imgs[0].MIME != "image/gif" {
		t.Fatalf("trailing punctuation broke resolution: %+v", imgs)
	}
}
