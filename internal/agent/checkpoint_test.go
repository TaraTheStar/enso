// SPDX-License-Identifier: AGPL-3.0-or-later

package agent

import "testing"

func TestCheckpointRequest_RoundTrip(t *testing.T) {
	a := &Agent{}
	a.RequestCheckpoint("finished step 1")

	ok, reason := a.consumeCheckpointRequest()
	if !ok {
		t.Fatal("first consume after request: want ok=true")
	}
	if reason != "finished step 1" {
		t.Errorf("reason=%q, want %q", reason, "finished step 1")
	}

	// Second consume returns nothing — the flag is one-shot.
	ok2, reason2 := a.consumeCheckpointRequest()
	if ok2 {
		t.Errorf("second consume: want ok=false, got reason=%q", reason2)
	}
}

func TestCheckpointRequest_EmptyReason(t *testing.T) {
	a := &Agent{}
	a.RequestCheckpoint("")

	ok, reason := a.consumeCheckpointRequest()
	if !ok {
		t.Fatal("want ok=true for empty-reason request")
	}
	if reason != "" {
		t.Errorf("reason=%q, want empty", reason)
	}
}

func TestCheckpointRequest_OverwritesPrior(t *testing.T) {
	// Two requests before a consume: the latest reason wins. Mirrors
	// the model calling checkpoint twice in one assistant turn — we
	// don't queue compactions, we collapse them.
	a := &Agent{}
	a.RequestCheckpoint("first")
	a.RequestCheckpoint("second")

	ok, reason := a.consumeCheckpointRequest()
	if !ok {
		t.Fatal("want ok=true")
	}
	if reason != "second" {
		t.Errorf("reason=%q, want %q", reason, "second")
	}
}

func TestBuildSummariseRequest_SeedRendersOutsideFence(t *testing.T) {
	_, user, nonce := buildSummariseRequest(nil, "finished refactor of foo.go")
	open := "<conversation-" + nonce + ">"
	idxFence := indexOf(user, open)
	idxSeed := indexOf(user, "finished refactor of foo.go")
	if idxSeed < 0 {
		t.Fatalf("seed missing from user prompt: %s", user)
	}
	if idxFence < 0 {
		t.Fatalf("open fence missing: %s", user)
	}
	if idxSeed >= idxFence {
		t.Errorf("seed must render before the fenced conversation block, got seed@%d fence@%d", idxSeed, idxFence)
	}
}

func TestBuildSummariseRequest_EmptySeedSuppressed(t *testing.T) {
	_, user, _ := buildSummariseRequest(nil, "")
	if indexOf(user, "step boundary") >= 0 {
		t.Errorf("empty seed should not emit boundary preamble, got: %s", user)
	}
}

// indexOf is strings.Index inlined to avoid an import in a tiny test file.
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
