// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

// Windows stubs for the daemon API. The real implementation uses unix
// sockets, syscall.Kill, and syscall.Setsid — none of which translate
// cleanly to Windows. Rather than emulate them with named pipes for v1
// (real work, narrow audience), we ship a build that compiles on
// Windows but errors out at the entry points. The TUI, single-shot
// `enso run`, sessions, sandbox, LSP, etc. all work; only the daemon
// + attach paths are unavailable.

package daemon

import (
	"context"
	"errors"
)

// errUnsupported is returned by every Windows entry point.
var errUnsupported = errors.New("daemon: not supported on Windows; use enso tui or enso run instead (or run via WSL)")

// SocketPath has no meaningful Windows equivalent. Returns the error so
// callers can surface it instead of attempting a connection.
func SocketPath() (string, error) { return "", errUnsupported }

// PIDPath mirrors SocketPath — same reasoning.
func PIDPath() (string, error) { return "", errUnsupported }

// Run is the daemon entry point. On Windows it reports unsupported and
// returns immediately so `enso daemon` exits with a clear message.
func Run(ctx context.Context, explicitConfig string) error { return errUnsupported }

// Client is the type the rest of the codebase expects. On Windows it
// carries no state and every method returns errUnsupported, so attempts
// to connect to or drive a daemon fail loudly.
type Client struct{}

// Dial would connect to the daemon's unix socket; on Windows there is
// no socket to connect to.
func Dial() (*Client, error) { return nil, errUnsupported }

func (c *Client) Close() error { return nil }
func (c *Client) Send(kind MessageKind, body interface{}) (Message, error) {
	return Message{}, errUnsupported
}
func (c *Client) CreateSession(req CreateSessionReq) (*SessionInfo, error) {
	return nil, errUnsupported
}
func (c *Client) Subscribe(req SubscribeReq) (<-chan Event, error) { return nil, errUnsupported }
func (c *Client) Submit(req SubmitReq) error                       { return errUnsupported }
func (c *Client) Cancel(req CancelReq) error                       { return errUnsupported }
func (c *Client) Respond(req PermissionResponseReq) error          { return errUnsupported }
func (c *Client) ListSessions() ([]SessionInfo, error)             { return nil, errUnsupported }
