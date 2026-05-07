// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/TaraTheStar/enso/internal/bus"
	"github.com/TaraTheStar/enso/internal/daemon"
	"github.com/TaraTheStar/enso/internal/permissions"
)

// RunAttached connects to the daemon, subscribes to the named session, and
// drives a chat TUI from the streamed events. User input is forwarded to the
// daemon via Submit; the local process runs no agent.
//
// Reconnects automatically: if the events stream drops (daemon restart,
// crash, network blip), an exponential-backoff loop redials and resubscribes
// with a `from_seq` cursor so events held in the daemon's recent-event ring
// replay rather than being missed. Submit/Cancel/Respond on the back-channel
// transparently redial-on-failure once.
//
// Permission prompts ARE supported: when the daemon proxies a permission
// request over the socket, the modal opens locally and the decision is
// routed back via PermissionResponse on the submit conn. The daemon
// auto-denies after 60 seconds if no client picks.
func RunAttached(sessionID string) error {
	app := tview.NewApplication()
	layout := NewLayout()
	chatDisp := NewChatDisplay(layout.Chat(), "remote")

	short := sessionID[:min(8, len(sessionID))]
	right := "attach · " + short
	activity := NewActivity()
	refreshStatus := func() {
		layout.SetStatus(activity.Render(), right)
	}
	activity.Set(ActivityConnecting, "")
	refreshStatus()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ac := newAttachClient(sessionID)
	defer ac.close()

	// Spinner ticker: 100ms cadence, animates the activity glyph while
	// busy. Idle when nothing is in flight.
	go func() {
		t := time.NewTicker(100 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if activity.IsBusy() {
					activity.Tick()
					app.QueueUpdateDraw(refreshStatus)
				}
			}
		}
	}()

	// Status / chat notices on connect / disconnect transitions.
	ac.onConnect = func() {
		app.QueueUpdateDraw(func() {
			fmt.Fprintf(layout.Chat(), "[teal](connected)[-]\n\n")
			layout.Chat().ScrollToEnd()
			activity.Set(ActivityReady, "")
			refreshStatus()
		})
	}
	ac.onDisconnect = func(err error) {
		app.QueueUpdateDraw(func() {
			fmt.Fprintf(layout.Chat(), "[yellow](disconnected: %v · reconnecting…)[-]\n\n", err)
			layout.Chat().ScrollToEnd()
			activity.Set(ActivityReconnecting, "")
			refreshStatus()
		})
	}

	// Events goroutine: consumes the reconnecting stream forever until ctx
	// cancels (Ctrl-D, app.Stop). Permission requests get demuxed into the
	// modal handler.
	go func() {
		for evt := range ac.run(ctx) {
			ev := evt
			if ev.Type == "PermissionRequest" {
				handlePermissionRequest(app, layout, ac, sessionID, ev)
				continue
			}
			busEv := toBusEvent(ev)
			changed := updateActivityFromEvent(activity, busEv)
			app.QueueUpdateDraw(func() {
				chatDisp.Render(busEv)
				if changed {
					refreshStatus()
				}
			})
		}
	}()

	var handler *InputHandler
	handler = NewInputHandler(
		layout.Input(),
		func(text string) {
			handler.SetBusy(true)
			activity.Set(ActivitySubmitting, "")
			refreshStatus()
			go func() {
				if err := ac.Submit(daemon.SubmitReq{
					SessionID: sessionID,
					Message:   text,
				}); err != nil {
					app.QueueUpdateDraw(func() {
						fmt.Fprintf(layout.Chat(), "[red]submit: %v[-]\n\n", err)
						layout.Chat().ScrollToEnd()
					})
				}
			}()
		},
		func() {
			_ = ac.Cancel(daemon.CancelReq{SessionID: sessionID})
			handler.SetBusy(false)
		},
		func() {
			cancel()
			app.Stop()
		},
	)

	app.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		return event
	})

	if err := layout.SetRoot(app); err != nil {
		return fmt.Errorf("tui: %w", err)
	}
	return nil
}

// attachClient owns both the events-stream connection (with reconnect loop)
// and the back-channel submit connection (with redial-on-failure).
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

// withSubmitConn lazily dials the back-channel and runs `fn`. On failure it
// closes the conn, redials once, and retries. Concurrency-safe.
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

// Submit, Cancel, Respond mirror daemon.Client's methods but transparently
// reconnect-on-failure.
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

// sleepCtx blocks for d unless ctx cancels first. Returns false if ctx
// cancelled.
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

// toBusEvent converts a wire-format daemon.Event back into a bus.Event so the
// existing ChatDisplay.Render can consume it.
func toBusEvent(e daemon.Event) bus.Event {
	var payload any
	if len(e.Payload) > 0 {
		_ = json.Unmarshal(e.Payload, &payload)
	}
	switch e.Type {
	case "UserMessage":
		s, _ := payload.(string)
		return bus.Event{Type: bus.EventUserMessage, Payload: s}
	case "AssistantDelta":
		s, _ := payload.(string)
		return bus.Event{Type: bus.EventAssistantDelta, Payload: s}
	case "AssistantDone":
		return bus.Event{Type: bus.EventAssistantDone}
	case "Error":
		s, _ := payload.(string)
		return bus.Event{Type: bus.EventError, Payload: fmt.Errorf("%s", s)}
	case "Cancelled":
		return bus.Event{Type: bus.EventCancelled}
	case "ToolCallStart":
		return bus.Event{Type: bus.EventToolCallStart, Payload: payload}
	case "ToolCallProgress":
		return bus.Event{Type: bus.EventToolCallProgress, Payload: payload}
	case "ToolCallEnd":
		return bus.Event{Type: bus.EventToolCallEnd, Payload: payload}
	case "AgentStart":
		return bus.Event{Type: bus.EventAgentStart, Payload: payload}
	case "AgentEnd":
		return bus.Event{Type: bus.EventAgentEnd, Payload: payload}
	case "Compacted":
		return bus.Event{Type: bus.EventCompacted, Payload: payload}
	}
	return bus.Event{}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// handlePermissionRequest decodes a PermissionRequest event from the daemon
// and shows the modal. The user's decision is sent back via the submit
// connection as a PermissionResponse keyed on the request id.
func handlePermissionRequest(
	app *tview.Application,
	layout *Layout,
	ac *attachClient,
	sessionID string,
	evt daemon.Event,
) {
	var p daemon.PermissionRequestPayload
	if err := json.Unmarshal(evt.Payload, &p); err != nil {
		return
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
	}
	app.QueueUpdateDraw(func() {
		// Allow + Remember persistence isn't supported over the socket
		// today (would need to round-trip back to the daemon's checker
		// and config.local.toml), so we pass nil for onRemember.
		ShowPermissionModal(app, layout.Pages(), layout.Input(), nil, req)
	})
}
