// SPDX-License-Identifier: AGPL-3.0-or-later

package backend

import (
	"bytes"
	"encoding/json"
	"io"
	"testing"
)

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

// TestInputBodyImagesRoundTrip locks the wire contract for user image
// attachments: InputImage bytes survive a NewBody → Channel → Unmarshal
// trip (JSON-encoded as base64) so an isolated worker reconstructs the
// exact image the host read.
func TestInputBodyImagesRoundTrip(t *testing.T) {
	pr, pw := io.Pipe()
	host := NewStreamChannelRW(nil, pw, pw)
	worker := NewStreamChannelRW(pr, nil, pr)

	png := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 9, 8, 7}
	body, err := NewBody(InputBody{
		Text:   "look at @shot.png",
		Images: []InputImage{{MIME: "image/png", Data: png}},
	})
	if err != nil {
		t.Fatalf("NewBody: %v", err)
	}
	go func() {
		_ = host.Send(Envelope{Kind: MsgInput, Body: body})
		_ = host.Close()
	}()

	got, err := worker.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	var in InputBody
	if err := json.Unmarshal(got.Body, &in); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if in.Text != "look at @shot.png" {
		t.Errorf("text = %q", in.Text)
	}
	if len(in.Images) != 1 || in.Images[0].MIME != "image/png" || !bytes.Equal(in.Images[0].Data, png) {
		t.Fatalf("image did not round-trip: %+v", in.Images)
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
