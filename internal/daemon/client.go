// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !windows

package daemon

import (
	"encoding/json"
	"fmt"
	"net"
)

// Client is a thin wrapper around a unix-socket connection speaking the
// length-prefixed JSON protocol.
type Client struct {
	conn net.Conn
}

// Dial connects to the daemon's unix socket.
func Dial() (*Client, error) {
	path, err := SocketPath()
	if err != nil {
		return nil, err
	}
	conn, err := net.Dial("unix", path)
	if err != nil {
		return nil, fmt.Errorf("dial daemon: %w", err)
	}
	return &Client{conn: conn}, nil
}

// Close drops the connection.
func (c *Client) Close() error { return c.conn.Close() }

// Send writes a request and decodes the next response.
func (c *Client) Send(kind MessageKind, body interface{}) (Message, error) {
	if err := WriteMessage(c.conn, kind, body); err != nil {
		return Message{}, err
	}
	return ReadMessage(c.conn)
}

// CreateSession submits a new prompt and returns the session info plus the
// underlying connection (kept open for streaming).
func (c *Client) CreateSession(req CreateSessionReq) (*SessionInfo, error) {
	if err := WriteMessage(c.conn, KindCreateSession, req); err != nil {
		return nil, err
	}
	resp, err := ReadMessage(c.conn)
	if err != nil {
		return nil, err
	}
	if resp.Kind == KindError {
		var er ErrorResp
		_ = json.Unmarshal(resp.Body, &er)
		return nil, fmt.Errorf("daemon: %s", er.Message)
	}
	if resp.Kind != KindSession {
		return nil, fmt.Errorf("daemon: unexpected response %s", resp.Kind)
	}
	var info SessionInfo
	if err := json.Unmarshal(resp.Body, &info); err != nil {
		return nil, fmt.Errorf("decode session: %w", err)
	}
	return &info, nil
}

// Subscribe asks the server to stream events for the given session and
// returns the read-only channel of events. Closes the channel when the
// connection terminates. Errors mid-stream cause the channel to close;
// inspect the connection error via Close().
func (c *Client) Subscribe(req SubscribeReq) (<-chan Event, error) {
	if err := WriteMessage(c.conn, KindSubscribe, req); err != nil {
		return nil, err
	}
	out := make(chan Event, 32)
	go func() {
		defer close(out)
		for {
			msg, err := ReadMessage(c.conn)
			if err != nil {
				return
			}
			if msg.Kind != KindEvent {
				continue
			}
			var e Event
			if err := json.Unmarshal(msg.Body, &e); err != nil {
				return
			}
			out <- e
		}
	}()
	return out, nil
}

// Submit sends a follow-up user message into an existing session. The reply
// is one ack or error.
func (c *Client) Submit(req SubmitReq) error {
	if err := WriteMessage(c.conn, KindSubmit, req); err != nil {
		return err
	}
	// No ack for now; submit is fire-and-forget. The agent's response will
	// arrive via the active subscribe stream.
	return nil
}

// Cancel aborts the in-flight turn.
func (c *Client) Cancel(req CancelReq) error {
	return WriteMessage(c.conn, KindCancel, req)
}

// Respond sends the user's permission decision back to the daemon.
// Decision must be PermissionAllow or PermissionDeny; anything else is
// treated as Deny by the server.
func (c *Client) Respond(req PermissionResponseReq) error {
	return WriteMessage(c.conn, KindPermissionResponse, req)
}

// ListSessions returns the daemon's view of running sessions.
func (c *Client) ListSessions() ([]SessionInfo, error) {
	if err := WriteMessage(c.conn, KindListSessions, nil); err != nil {
		return nil, err
	}
	resp, err := ReadMessage(c.conn)
	if err != nil {
		return nil, err
	}
	if resp.Kind != KindSessionList {
		return nil, fmt.Errorf("unexpected: %s", resp.Kind)
	}
	var list SessionList
	if err := json.Unmarshal(resp.Body, &list); err != nil {
		return nil, err
	}
	return list.Sessions, nil
}
