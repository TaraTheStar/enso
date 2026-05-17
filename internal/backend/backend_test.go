// SPDX-License-Identifier: AGPL-3.0-or-later

package backend

import (
	"encoding/json"
	"io"
	"testing"
)

// pipeRWC bundles an io.PipeReader/Writer pair into a ReadWriteCloser
// for one direction of the seam.
type pipeRWC struct {
	io.ReadCloser
	io.WriteCloser
}

func (p pipeRWC) Close() error {
	_ = p.ReadCloser.Close()
	return p.WriteCloser.Close()
}

func TestStreamChannelRoundTrip(t *testing.T) {
	// host writes -> worker reads, over an os.Pipe-like in-memory pipe.
	pr, pw := io.Pipe()
	host := NewStreamChannelRW(nil, pw, pw)
	worker := NewStreamChannelRW(pr, nil, pr)

	body, err := NewBody(InputBody{Text: "hello worker"})
	if err != nil {
		t.Fatalf("NewBody: %v", err)
	}
	want := Envelope{Kind: MsgInput, Corr: "c1", Body: body}

	go func() {
		if err := host.Send(want); err != nil {
			t.Errorf("Send: %v", err)
		}
		_ = host.Close()
	}()

	got, err := worker.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if got.Kind != MsgInput || got.Corr != "c1" {
		t.Fatalf("envelope mismatch: kind=%q corr=%q", got.Kind, got.Corr)
	}
	var in InputBody
	if err := json.Unmarshal(got.Body, &in); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if in.Text != "hello worker" {
		t.Fatalf("body text = %q, want %q", in.Text, "hello worker")
	}
}

func TestStreamChannelRejectsOversize(t *testing.T) {
	_, pw := io.Pipe()
	ch := &streamChannel{w: pw, c: pw}
	huge := make([]byte, maxFrame+1)
	env := Envelope{Kind: MsgEvent, Body: huge} // raw json bytes; marshals larger than cap
	if err := ch.Send(env); err == nil {
		t.Fatal("expected oversize envelope to be rejected")
	}
}
