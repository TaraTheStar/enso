// SPDX-License-Identifier: AGPL-3.0-or-later

// Package lsp implements just enough of the Language Server Protocol to
// expose hover / definition / references / diagnostics as agent tools.
// The wire format is JSON-RPC 2.0 over stdio with Content-Length framing,
// rolled by hand to avoid pulling another dep.
package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
)

// lspMaxMessageBytes caps a single LSP message body. Real traffic is
// well under 1 MiB even for huge diagnostic dumps; 64 MiB leaves room
// for outliers (rust-analyzer on giant crates) while preventing a
// runaway server from driving enso into OOM via a bogus Content-Length.
const lspMaxMessageBytes = 64 << 20

// Message is the JSON-RPC envelope. Method+ID = request; Method-only =
// notification; ID+(Result|Error) = response.
type Message struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method,omitempty"`
	Params  json.RawMessage  `json:"params,omitempty"`
	Result  json.RawMessage  `json:"result,omitempty"`
	Error   *RPCError        `json:"error,omitempty"`
}

// RPCError is the JSON-RPC error object.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("rpc %d: %s", e.Code, e.Message)
}

// Conn pairs a reader and writer for a single JSON-RPC stdio link, plus
// dispatching tables for in-flight requests and notification handlers.
type Conn struct {
	w io.Writer
	r *bufio.Reader

	writeMu sync.Mutex

	mu        sync.Mutex
	nextID    int64
	pending   map[int64]chan *Message
	notifyCb  func(method string, params json.RawMessage)
	closedErr error
}

// NewConn wraps a reader/writer pair. The caller is responsible for
// keeping w/r alive until the conn is closed.
func NewConn(w io.Writer, r io.Reader) *Conn {
	return &Conn{
		w:       w,
		r:       bufio.NewReader(r),
		pending: map[int64]chan *Message{},
	}
}

// SetNotificationHandler registers a callback for server-initiated
// notifications. Replaces any previously-set handler.
func (c *Conn) SetNotificationHandler(cb func(method string, params json.RawMessage)) {
	c.mu.Lock()
	c.notifyCb = cb
	c.mu.Unlock()
}

// Run reads messages until the underlying reader returns EOF or an error.
// Routes responses to their pending callers; routes notifications to the
// configured handler. Returns the terminating error (typically io.EOF).
func (c *Conn) Run() error {
	for {
		msg, err := c.readMessage()
		if err != nil {
			c.mu.Lock()
			c.closedErr = err
			for _, ch := range c.pending {
				close(ch)
			}
			c.pending = map[int64]chan *Message{}
			c.mu.Unlock()
			return err
		}
		if msg.ID != nil && msg.Method == "" {
			// Response to one of our requests.
			id, derr := decodeID(msg.ID)
			if derr != nil {
				continue
			}
			c.mu.Lock()
			ch := c.pending[id]
			delete(c.pending, id)
			c.mu.Unlock()
			if ch != nil {
				ch <- msg
			}
			continue
		}
		if msg.Method != "" && msg.ID == nil {
			c.mu.Lock()
			cb := c.notifyCb
			c.mu.Unlock()
			if cb != nil {
				cb(msg.Method, msg.Params)
			}
			continue
		}
		// Server-initiated request (`window/showMessageRequest`,
		// `workspace/configuration`, etc.). We don't implement any of
		// these for v1. Ack with an empty error so the server doesn't
		// stall waiting for a reply.
		if msg.ID != nil && msg.Method != "" {
			_ = c.writeMessage(&Message{
				JSONRPC: "2.0",
				ID:      msg.ID,
				Error:   &RPCError{Code: -32601, Message: "method not implemented by client"},
			})
		}
	}
}

// Call sends a request and blocks for the response (or ctx.Done()).
func (c *Conn) Call(ctx context.Context, method string, params, result any) error {
	c.mu.Lock()
	if c.closedErr != nil {
		err := c.closedErr
		c.mu.Unlock()
		return err
	}
	c.nextID++
	id := c.nextID
	ch := make(chan *Message, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	idJSON, _ := json.Marshal(id)
	rawID := json.RawMessage(idJSON)
	req := &Message{JSONRPC: "2.0", ID: &rawID, Method: method}
	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			return fmt.Errorf("marshal params: %w", err)
		}
		req.Params = raw
	}
	if err := c.writeMessage(req); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return err
	}

	select {
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return ctx.Err()
	case msg, ok := <-ch:
		if !ok {
			return errors.New("connection closed")
		}
		if msg.Error != nil {
			return msg.Error
		}
		if result != nil && len(msg.Result) > 0 {
			if err := json.Unmarshal(msg.Result, result); err != nil {
				return fmt.Errorf("unmarshal result: %w", err)
			}
		}
		return nil
	}
}

// Notify sends a notification (no response expected).
func (c *Conn) Notify(method string, params any) error {
	msg := &Message{JSONRPC: "2.0", Method: method}
	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			return fmt.Errorf("marshal params: %w", err)
		}
		msg.Params = raw
	}
	return c.writeMessage(msg)
}

func (c *Conn) writeMessage(msg *Message) error {
	body, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal message: %w", err)
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if _, err := fmt.Fprintf(c.w, "Content-Length: %d\r\n\r\n", len(body)); err != nil {
		return err
	}
	_, err = c.w.Write(body)
	return err
}

func (c *Conn) readMessage() (*Message, error) {
	contentLen := -1
	for {
		line, err := c.r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break // end of header
		}
		if strings.HasPrefix(strings.ToLower(line), "content-length:") {
			v := strings.TrimSpace(line[len("content-length:"):])
			n, err := strconv.Atoi(v)
			if err != nil {
				return nil, fmt.Errorf("invalid Content-Length %q", v)
			}
			contentLen = n
		}
	}
	if contentLen < 0 {
		return nil, errors.New("missing Content-Length header")
	}
	if contentLen > lspMaxMessageBytes {
		// A buggy or hostile server saying Content-Length: 99999999999
		// would otherwise drive enso into an OOM. Real LSP traffic is
		// well under 1 MiB even on giant diagnostics; 64 MiB leaves
		// generous headroom for outlier rust-analyzer responses.
		return nil, fmt.Errorf("Content-Length %d exceeds %d-byte cap", contentLen, lspMaxMessageBytes)
	}
	body := make([]byte, contentLen)
	if _, err := io.ReadFull(c.r, body); err != nil {
		return nil, err
	}
	var msg Message
	if err := json.Unmarshal(body, &msg); err != nil {
		return nil, fmt.Errorf("unmarshal message: %w", err)
	}
	return &msg, nil
}

func decodeID(raw *json.RawMessage) (int64, error) {
	if raw == nil {
		return 0, errors.New("nil id")
	}
	var n int64
	if err := json.Unmarshal(*raw, &n); err == nil {
		return n, nil
	}
	// Some servers echo the id as a string; try that too.
	var s string
	if err := json.Unmarshal(*raw, &s); err == nil {
		return strconv.ParseInt(s, 10, 64)
	}
	return 0, errors.New("unrecognised id type")
}
