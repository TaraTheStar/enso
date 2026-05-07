// SPDX-License-Identifier: AGPL-3.0-or-later

package lsp

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// fixture wires a client Conn to a "server" reader/writer pair without
// running the server-side Conn's Run loop. The test goroutine reads from
// `serverIn` (the bytes the client wrote) and writes to `serverOut` (which
// the client reads). Lets us hand-craft responses without implementing a
// dispatcher.
type fixture struct {
	client    *Conn
	serverIn  io.Reader // what the client sent
	serverOut io.Writer // what the client will receive
	stop      func()
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	cr, sw := io.Pipe() // client reads, server writes
	sr, cw := io.Pipe() // server reads, client writes
	c := NewConn(cw, cr)
	clientDone := make(chan struct{})
	go func() { _ = c.Run(); close(clientDone) }()

	// Drain the server-side pipe so the client's outgoing writes don't
	// deadlock: io.Pipe.Write blocks until the data is consumed, and
	// nobody else is reading from sr in these tests.
	drainDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, sr)
		close(drainDone)
	}()

	stop := func() {
		_ = cr.Close()
		_ = sw.Close()
		_ = cw.Close()
		_ = sr.Close()
		select {
		case <-clientDone:
		case <-time.After(time.Second):
		}
		select {
		case <-drainDone:
		case <-time.After(time.Second):
		}
	}
	return &fixture{client: c, serverIn: sr, serverOut: sw, stop: stop}
}

// writeServerMessage Content-Length-frames a Message and pushes it down
// the pipe so the client's Run loop will read it.
func writeServerMessage(t *testing.T, w io.Writer, msg *Message) {
	t.Helper()
	body, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, "Content-Length: "); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, jsonInt(len(body))+"\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(body); err != nil {
		t.Fatal(err)
	}
}

func jsonInt(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func TestRequestResponseRoundtrip(t *testing.T) {
	f := newFixture(t)
	defer f.stop()

	go func() {
		// First Call() in the client gets id=1; reply with a known result.
		time.Sleep(50 * time.Millisecond)
		idJSON := json.RawMessage("1")
		writeServerMessage(t, f.serverOut, &Message{
			JSONRPC: "2.0",
			ID:      &idJSON,
			Result:  json.RawMessage(`{"echo":"hi"}`),
		})
	}()

	type pingResult struct {
		Echo string `json:"echo"`
	}
	var got pingResult
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := f.client.Call(ctx, "ping", map[string]string{"echo": "hi"}, &got); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if got.Echo != "hi" {
		t.Errorf("echo = %q, want hi", got.Echo)
	}
}

func TestNotificationsArriveAtHandler(t *testing.T) {
	f := newFixture(t)
	defer f.stop()

	var got struct {
		mu     sync.Mutex
		method string
		params json.RawMessage
	}
	done := make(chan struct{})
	f.client.SetNotificationHandler(func(method string, params json.RawMessage) {
		got.mu.Lock()
		got.method = method
		got.params = params
		got.mu.Unlock()
		close(done)
	})

	go func() {
		writeServerMessage(t, f.serverOut, &Message{
			JSONRPC: "2.0",
			Method:  "textDocument/publishDiagnostics",
			Params:  json.RawMessage(`{"uri":"file:///x"}`),
		})
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("notification timeout")
	}
	got.mu.Lock()
	defer got.mu.Unlock()
	if got.method != "textDocument/publishDiagnostics" {
		t.Errorf("method = %q", got.method)
	}
	if !strings.Contains(string(got.params), "file:///x") {
		t.Errorf("params = %s", got.params)
	}
}

func TestCallReturnsRPCError(t *testing.T) {
	f := newFixture(t)
	defer f.stop()

	go func() {
		time.Sleep(50 * time.Millisecond)
		idJSON := json.RawMessage("1")
		writeServerMessage(t, f.serverOut, &Message{
			JSONRPC: "2.0",
			ID:      &idJSON,
			Error:   &RPCError{Code: -32601, Message: "boom"},
		})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := f.client.Call(ctx, "explode", nil, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("wrong error: %v", err)
	}
}

// A buggy / hostile LSP server can claim a body so large that
// allocating it would OOM the agent. readMessage must reject before
// touching `make`.
func TestReadMessage_RejectsOversizedContentLength(t *testing.T) {
	// One byte over the cap is enough; we never allocate or read.
	header := "Content-Length: " + jsonInt(lspMaxMessageBytes+1) + "\r\n\r\n"
	c := NewConn(io.Discard, strings.NewReader(header))
	if _, err := c.readMessage(); err == nil {
		t.Fatal("expected error on oversized Content-Length")
	} else if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("wrong error: %v", err)
	}
}

// strconv.Atoi happily parses negative ints. Without an explicit guard
// `make([]byte, -1)` panics; the existing `< 0` check catches the
// "header missing" sentinel but a parsed negative value would slip
// through if someone moved the guard. Belt+suspenders test.
func TestReadMessage_RejectsNegativeContentLength(t *testing.T) {
	header := "Content-Length: -42\r\n\r\n"
	c := NewConn(io.Discard, strings.NewReader(header))
	if _, err := c.readMessage(); err == nil {
		t.Fatal("expected error on negative Content-Length")
	}
}

// Just-under-cap values still parse normally (header only — we don't
// actually feed a real 64 MiB body).
func TestReadMessage_AtCapHeaderParsesBodyEOFs(t *testing.T) {
	header := "Content-Length: " + jsonInt(lspMaxMessageBytes) + "\r\n\r\n"
	c := NewConn(io.Discard, strings.NewReader(header))
	// readMessage will then try ReadFull on the body — short reader
	// means io.ErrUnexpectedEOF, NOT the cap-exceeded error.
	_, err := c.readMessage()
	if err == nil {
		t.Fatal("expected EOF error reading body")
	}
	if strings.Contains(err.Error(), "exceeds") {
		t.Errorf("at-cap should not trip the cap check, got: %v", err)
	}
}
