// SPDX-License-Identifier: AGPL-3.0-or-later

package bubble

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/TaraTheStar/enso/internal/bus"
	"github.com/TaraTheStar/enso/internal/daemon"
	"github.com/TaraTheStar/enso/internal/permissions"
)

// RunAttached connects to the daemon, subscribes to the named session,
// and drives a chat UI from the streamed events. The local process
// runs no agent — typed input is forwarded to the daemon via Submit;
// permission decisions go back via PermissionResponse on the same
// back-channel.
//
// Reconnects automatically: if the events stream drops (daemon
// restart, network blip), an exponential-backoff loop redials and
// resubscribes with a from_seq cursor so events the daemon's
// recent-event ring buffered while we were gone replay rather than
// being missed. The submit conn redial-on-failure is independent of
// the events stream.
//
// Attach mode disables features that depend on a local agent: the
// inspector overlay (its data source isn't available remotely), slash
// commands (most depend on the local agent / store / config). Vim,
// the file picker, the inline permission flow, and the conversation
// state machine all work unchanged because they only touch local UI
// state or round-trip through the daemon's existing wire protocol.
func RunAttached(sessionID string) error {
	loadAndApplyTheme()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ac := newAttachClient(sessionID)
	defer ac.close()

	// Banner before the live region appears so the user knows which
	// session they're attached to.
	short := sessionID
	if len(short) > 8 {
		short = short[:8]
	}
	fmt.Println(asstStyle.Render("enso") + "  attached · " + short)

	// inputCh mirrors the local-mode shape: the model writes user
	// submissions onto it, a goroutine drains and forwards them to
	// the daemon. Decoupling lets the model be agnostic about whether
	// it's running locally or attached.
	inputCh := make(chan string, 64)

	m := &model{
		inputCh:   inputCh,
		modelName: "remote",
		// slashReg/slashCtx, overlay left nil — features unavailable
		// without a local agent. picker still works against the local
		// cwd because file paths are interpreted on the input side.
		picker: &pickerData{cwd: "."},
	}
	// Permission "remember" / "turn-allow" need to mutate the local
	// checker + persist to local config; in attach the checker lives
	// in the daemon process. Leaving permCheckerCwd zero means
	// resolvePerm returns nil for those branches; for now the user
	// still gets y/n which round-trips correctly. Persisted-allow
	// over the wire is task #4 territory.
	m.permCheckerCwd.checker = nil
	m.permCheckerCwd.cwd = ""

	p := tea.NewProgram(m)

	// Forward typed input to the daemon. Started AFTER p is built
	// because the error path needs to send a bus event back into the
	// program; capturing p here avoids the otherwise-needed shared
	// variable.
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case text, ok := <-inputCh:
				if !ok {
					return
				}
				if err := ac.Submit(daemon.SubmitReq{
					SessionID: sessionID,
					Message:   text,
				}); err != nil {
					p.Send(busEventMsg{ev: bus.Event{
						Type:    bus.EventError,
						Payload: fmt.Errorf("submit: %w", err),
					}})
				}
			}
		}
	}()

	// Wire reconnect notices via tea.Println so they land in
	// scrollback as part of the conversation history. Connect
	// notices are silenced — events arriving is its own confirmation
	// and "(connected)" lines on every reconnect would be noisy.
	ac.onConnect = nil
	ac.onDisconnect = func(err error) {
		p.Println(noticeStyle.Render(fmt.Sprintf("(disconnected: %v · reconnecting…)", err)))
	}

	// Events stream → tea.Msg. Reconnects internally; on each redial
	// the from_seq cursor lets the daemon replay anything we missed.
	go func() {
		for evt := range ac.run(ctx) {
			ev := evt
			if ev.Type == "PermissionRequest" {
				p.Send(busEventMsg{ev: synthesizePermissionEvent(ev, ac, sessionID)})
				continue
			}
			busEv := toBusEvent(ev)
			if busEv.Type == 0 && busEv.Payload == nil {
				continue
			}
			p.Send(busEventMsg{ev: busEv})
		}
	}()

	_, runErr := p.Run()

	cancel()
	close(inputCh)
	if runErr != nil {
		return fmt.Errorf("attach: %w", runErr)
	}
	return nil
}

// synthesizePermissionEvent decodes a wire PermissionRequest into a
// bus.Event whose payload is a *permissions.PromptRequest. The Respond
// chan is read by a goroutine that forwards the Decision back to the
// daemon as a PermissionResponse on the submit connection.
func synthesizePermissionEvent(ev daemon.Event, ac *attachClient, sessionID string) bus.Event {
	var p daemon.PermissionRequestPayload
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		return bus.Event{Type: bus.EventError, Payload: fmt.Errorf("permission decode: %w", err)}
	}
	respCh := make(chan permissions.Decision, 1)
	go func() {
		d := <-respCh
		decision := daemon.PermissionDeny
		if d == permissions.Allow {
			decision = daemon.PermissionAllow
		}
		_ = ac.Respond(daemon.PermissionResponseReq{
			SessionID: sessionID,
			RequestID: p.RequestID,
			Decision:  decision,
		})
	}()
	req := &permissions.PromptRequest{
		ToolName:  p.Tool,
		Args:      p.Args,
		Diff:      p.Diff,
		AgentID:   p.AgentID,
		AgentRole: p.AgentRole,
		Respond:   respCh,
		Deadline:  p.Deadline,
	}
	return bus.Event{Type: bus.EventPermissionRequest, Payload: req}
}

// toBusEvent converts a wire-format daemon.Event back into a
// bus.Event so the conversation state machine + block renderer can
// consume it.
func toBusEvent(e daemon.Event) bus.Event {
	// Delegate to the single source of truth so an attached daemon
	// session and a local-backend run reconstruct events identically.
	if ev, ok := bus.FromWire(e.Type, e.Payload); ok {
		return ev
	}
	return bus.Event{}
}

// attachClient owns both the events-stream connection (with reconnect
// loop) and the back-channel submit connection (with redial-on-
// failure). Mirrored from the original tui implementation.
type attachClient struct {
	sessionID string

	onConnect    func()
	onDisconnect func(error)

	mu         sync.Mutex
	submitConn *daemon.Client
}

func newAttachClient(sessionID string) *attachClient {
	return &attachClient{sessionID: sessionID}
}

// run starts the reconnect loop and returns a channel of events. The
// channel closes when ctx cancels.
func (ac *attachClient) run(ctx context.Context) <-chan daemon.Event {
	out := make(chan daemon.Event, 64)
	go func() {
		defer close(out)
		var fromSeq int64
		backoff := 500 * time.Millisecond
		const maxBackoff = 5 * time.Second
		for {
			if ctx.Err() != nil {
				return
			}

			c, err := daemon.Dial()
			if err != nil {
				ac.fireDisconnect(err)
				if !sleepCtx(ctx, backoff) {
					return
				}
				if backoff < maxBackoff {
					backoff *= 2
					if backoff > maxBackoff {
						backoff = maxBackoff
					}
				}
				continue
			}
			backoff = 500 * time.Millisecond

			events, err := c.Subscribe(daemon.SubscribeReq{
				SessionID: ac.sessionID,
				FromSeq:   fromSeq,
			})
			if err != nil {
				_ = c.Close()
				ac.fireDisconnect(err)
				if !sleepCtx(ctx, backoff) {
					return
				}
				continue
			}
			ac.fireConnect()

			for evt := range events {
				if evt.Seq > fromSeq {
					fromSeq = evt.Seq
				}
				select {
				case out <- evt:
				case <-ctx.Done():
					_ = c.Close()
					return
				}
			}
			_ = c.Close()
			ac.fireDisconnect(fmt.Errorf("stream closed"))
		}
	}()
	return out
}

func (ac *attachClient) fireConnect() {
	if ac.onConnect != nil {
		ac.onConnect()
	}
}

func (ac *attachClient) fireDisconnect(err error) {
	if ac.onDisconnect != nil {
		ac.onDisconnect(err)
	}
}

// withSubmitConn lazily dials the back-channel and runs `fn`. On
// failure it closes the conn, redials once, and retries.
// Concurrency-safe.
func (ac *attachClient) withSubmitConn(fn func(*daemon.Client) error) error {
	ac.mu.Lock()
	defer ac.mu.Unlock()

	for attempt := 0; attempt < 2; attempt++ {
		if ac.submitConn == nil {
			c, err := daemon.Dial()
			if err != nil {
				if attempt == 1 {
					return err
				}
				continue
			}
			ac.submitConn = c
		}
		if err := fn(ac.submitConn); err != nil {
			_ = ac.submitConn.Close()
			ac.submitConn = nil
			if attempt == 1 {
				return err
			}
			continue
		}
		return nil
	}
	return fmt.Errorf("submit conn unreachable")
}

// Submit, Cancel, Respond mirror daemon.Client's methods but
// transparently reconnect-on-failure.
func (ac *attachClient) Submit(req daemon.SubmitReq) error {
	return ac.withSubmitConn(func(c *daemon.Client) error { return c.Submit(req) })
}
func (ac *attachClient) Cancel(req daemon.CancelReq) error {
	return ac.withSubmitConn(func(c *daemon.Client) error { return c.Cancel(req) })
}
func (ac *attachClient) Respond(req daemon.PermissionResponseReq) error {
	return ac.withSubmitConn(func(c *daemon.Client) error { return c.Respond(req) })
}

func (ac *attachClient) close() {
	ac.mu.Lock()
	defer ac.mu.Unlock()
	if ac.submitConn != nil {
		_ = ac.submitConn.Close()
		ac.submitConn = nil
	}
}

// sleepCtx blocks for d unless ctx cancels first. Returns false if
// ctx cancelled.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
