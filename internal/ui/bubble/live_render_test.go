// SPDX-License-Identifier: AGPL-3.0-or-later

package bubble

import (
	"strings"
	"testing"
	"time"

	"github.com/TaraTheStar/enso/internal/ui/blocks"
)

// liveRenderChunks simulates a streaming delta sequence with the shapes
// that exercise the cache's stable/tail boundary: short fragments,
// fragments carrying one or many newlines, blank lines, leading
// newlines, unbreakable long tokens (forced hard wraps), wide runes,
// and a chunk that is only newlines (trailing-\n trimming).
var liveRenderChunks = []string{
	"Hello",
	", world. ",
	"This is a longer sentence that will certainly wrap at narrow widths.\n",
	"\n",
	"- item one\n- item two with a very long unbreakable token: ",
	strings.Repeat("x", 240),
	"\n\npara two — héllo wörld ✓ 漢字テスト",
	" tail keeps growing",
	" and growing\nnew line starts",
	"\n\n\n",
	"after blank lines",
}

// TestLiveRenderMatchesRenderBlock: the cached live renderer must be
// byte-identical to the uncached renderBlock at every step of an
// incremental stream, for every width, including a mid-stream width
// change (terminal resize) and a switch to a different live block.
func TestLiveRenderMatchesRenderBlock(t *testing.T) {
	widths := []int{0, 20, 40, 80, 120}

	t.Run("assistant incremental", func(t *testing.T) {
		for _, w := range widths {
			var cache liveRenderCache
			b := &blocks.Assistant{}
			for i, chunk := range liveRenderChunks {
				b.Text += chunk
				got := cache.render(b, w)
				want := renderBlock(b, w, false)
				if got != want {
					t.Fatalf("width=%d step=%d: cached render diverged\n got: %q\nwant: %q", w, i, got, want)
				}
			}
		}
	})

	t.Run("reasoning incremental", func(t *testing.T) {
		for _, w := range widths {
			var cache liveRenderCache
			b := &blocks.Reasoning{Started: time.Now()}
			for i, chunk := range liveRenderChunks {
				b.Text += chunk
				got := cache.render(b, w)
				want := renderBlock(b, w, false)
				if got != want {
					t.Fatalf("width=%d step=%d: cached render diverged\n got: %q\nwant: %q", w, i, got, want)
				}
			}
			// Closed reasoning (footer) must match too — flushLive can
			// mark the live block closed before the final render.
			b.Closed = true
			b.Duration = 1500 * time.Millisecond
			if got, want := cache.render(b, w), renderBlock(b, w, false); got != want {
				t.Fatalf("width=%d closed: cached render diverged\n got: %q\nwant: %q", w, got, want)
			}
		}
	})

	t.Run("width change invalidates", func(t *testing.T) {
		var cache liveRenderCache
		b := &blocks.Assistant{Text: "alpha beta gamma\ndelta epsilon zeta eta theta iota kappa\nlambda"}
		if got, want := cache.render(b, 80), renderBlock(b, 80, false); got != want {
			t.Fatalf("initial width 80 diverged:\n got: %q\nwant: %q", got, want)
		}
		// Resize narrower, then wider; each must rebuild correctly.
		for _, w := range []int{24, 100, 24} {
			if got, want := cache.render(b, w), renderBlock(b, w, false); got != want {
				t.Fatalf("after resize to %d diverged:\n got: %q\nwant: %q", w, got, want)
			}
		}
	})

	t.Run("block switch invalidates", func(t *testing.T) {
		var cache liveRenderCache
		a := &blocks.Assistant{Text: "first block\nstreams here"}
		if got, want := cache.render(a, 40), renderBlock(a, 40, false); got != want {
			t.Fatalf("first block diverged:\n got: %q\nwant: %q", got, want)
		}
		r := &blocks.Reasoning{Text: "now a reasoning block\nwith two lines", Started: time.Now()}
		if got, want := cache.render(r, 40), renderBlock(r, 40, false); got != want {
			t.Fatalf("switched block diverged:\n got: %q\nwant: %q", got, want)
		}
		a2 := &blocks.Assistant{Text: "and back to a fresh assistant block"}
		if got, want := cache.render(a2, 40), renderBlock(a2, 40, false); got != want {
			t.Fatalf("second assistant block diverged:\n got: %q\nwant: %q", got, want)
		}
	})

	t.Run("non-streaming kinds fall through", func(t *testing.T) {
		var cache liveRenderCache
		cases := []blocks.Block{
			&blocks.Tool{Call: "bash(cmd=ls)", Output: "a\nb\nc"},
			&blocks.User{Text: "hi"},
			&blocks.Error{Msg: "boom"},
			&blocks.Notice{Text: "note"},
		}
		for _, b := range cases {
			if got, want := cache.render(b, 60), renderBlock(b, 60, false); got != want {
				t.Fatalf("%T diverged:\n got: %q\nwant: %q", b, got, want)
			}
		}
	})
}
