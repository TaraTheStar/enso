// SPDX-License-Identifier: AGPL-3.0-or-later

package daemon

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// SocketName is the unix socket filename under $XDG_RUNTIME_DIR/enso.
// Resolved fully via SocketPath() in client.go and server.go.
const SocketName = "daemon.sock"

// PIDFileName is the pid lock filename under $XDG_RUNTIME_DIR/enso.
const PIDFileName = "daemon.pid"

// MessageKind discriminates the wire-format envelope.
type MessageKind string

const (
	KindListSessions       MessageKind = "list_sessions"
	KindCreateSession      MessageKind = "create_session"
	KindSubmit             MessageKind = "submit"
	KindSubscribe          MessageKind = "subscribe"
	KindCancel             MessageKind = "cancel"
	KindPermissionResponse MessageKind = "permission_response"

	KindSessionList MessageKind = "session_list"
	KindSession     MessageKind = "session"
	KindEvent       MessageKind = "event"
	KindAck         MessageKind = "ack"
	KindError       MessageKind = "error"
)

// Message is the single wire envelope. Body holds the kind-specific payload
// as raw JSON so each side decodes lazily.
type Message struct {
	Kind MessageKind     `json:"kind"`
	Body json.RawMessage `json:"body,omitempty"`
}

// CreateSessionReq starts a new session and runs `Prompt` in it. The agent
// uses the daemon's default permission settings. Set Yolo=true to skip
// prompts (recommended — interactive permission prompts over a socket are
// not supported in this MVP).
type CreateSessionReq struct {
	Prompt   string `json:"prompt"`
	Cwd      string `json:"cwd"`
	Yolo     bool   `json:"yolo"`
	MaxTurns int    `json:"max_turns,omitempty"`
}

// SubmitReq submits a follow-up user message into an existing session.
type SubmitReq struct {
	SessionID string `json:"session_id"`
	Message   string `json:"message"`
}

// SubscribeReq asks the server to stream future events for the session, plus
// optionally replay events with seq > FromSeq from the in-memory ring.
type SubscribeReq struct {
	SessionID string `json:"session_id"`
	FromSeq   int64  `json:"from_seq,omitempty"`
}

// CancelReq aborts the in-flight turn for the session.
type CancelReq struct {
	SessionID string `json:"session_id"`
}

// PermissionResponseReq carries the user's decision back to the daemon
// for a permission request the daemon previously fanned out as a
// PermissionRequest event. Decision must be exactly "allow" or "deny";
// any other value (missing, empty, unknown) is treated as Deny by the
// server. The fail-closed default matters: an earlier int encoding
// silently allowed any malformed payload.
type PermissionResponseReq struct {
	SessionID string `json:"session_id"`
	RequestID string `json:"request_id"`
	Decision  string `json:"decision"`
}

// PermissionDecision wire constants. Mirror these on senders to avoid
// typos that would silently flip a user choice to Deny.
const (
	PermissionAllow = "allow"
	PermissionDeny  = "deny"
)

// PermissionRequestPayload is the body of a wire Event of type
// "PermissionRequest" — what subscribers see when the agent is waiting on
// a tool-call decision. Reply via PermissionResponseReq using RequestID.
//
// AgentID and AgentRole are non-empty when a sub-agent (workflow role
// or spawn_agent child) issued the call — the attached UI uses them to
// label the prompt so the user can tell children apart.
type PermissionRequestPayload struct {
	RequestID string                 `json:"request_id"`
	Tool      string                 `json:"tool"`
	Args      map[string]interface{} `json:"args"`
	Diff      string                 `json:"diff,omitempty"`
	AgentID   string                 `json:"agent_id,omitempty"`
	AgentRole string                 `json:"agent_role,omitempty"`
	// Deadline is the wall-clock time at which the daemon will
	// auto-deny if no client decision arrives. Empty (zero-time) on
	// older daemons; the TUI countdown treats that as "no countdown
	// known" and renders the prompt without a timer.
	Deadline time.Time `json:"deadline,omitempty"`
}

// SessionInfo is the daemon's view of one session.
type SessionInfo struct {
	ID        string    `json:"id"`
	Cwd       string    `json:"cwd"`
	CreatedAt time.Time `json:"created_at"`
	Yolo      bool      `json:"yolo"`
}

// SessionList is the response to ListSessions.
type SessionList struct {
	Sessions []SessionInfo `json:"sessions"`
}

// Event is the over-the-wire form of a bus event. Payloads are limited to
// JSON-safe primitives to keep the protocol stable.
type Event struct {
	Seq     int64           `json:"seq"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// AckResp signals success without a body.
type AckResp struct {
	OK bool `json:"ok"`
}

// ErrorResp signals failure.
type ErrorResp struct {
	Message string `json:"message"`
}

// WriteMessage encodes a Message as 4-byte length prefix + JSON.
func WriteMessage(w io.Writer, kind MessageKind, body interface{}) error {
	var raw json.RawMessage
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal body: %w", err)
		}
		raw = b
	}
	buf, err := json.Marshal(Message{Kind: kind, Body: raw})
	if err != nil {
		return fmt.Errorf("marshal envelope: %w", err)
	}
	var prefix [4]byte
	binary.BigEndian.PutUint32(prefix[:], uint32(len(buf)))
	if _, err := w.Write(prefix[:]); err != nil {
		return fmt.Errorf("write prefix: %w", err)
	}
	if _, err := w.Write(buf); err != nil {
		return fmt.Errorf("write body: %w", err)
	}
	return nil
}

// ReadMessage decodes one length-prefixed message from r.
func ReadMessage(r io.Reader) (Message, error) {
	var prefix [4]byte
	if _, err := io.ReadFull(r, prefix[:]); err != nil {
		return Message{}, err
	}
	n := binary.BigEndian.Uint32(prefix[:])
	if n == 0 || n > 8*1024*1024 {
		return Message{}, fmt.Errorf("framing: bad length %d", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return Message{}, fmt.Errorf("read body: %w", err)
	}
	var m Message
	if err := json.Unmarshal(buf, &m); err != nil {
		return Message{}, fmt.Errorf("unmarshal: %w", err)
	}
	return m, nil
}
