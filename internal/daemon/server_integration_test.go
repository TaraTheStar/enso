// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !windows

package daemon

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TaraTheStar/enso/internal/config"
	"github.com/TaraTheStar/enso/internal/llm"
	"github.com/TaraTheStar/enso/internal/llm/llmtest"
	"github.com/TaraTheStar/enso/internal/session"
	"github.com/TaraTheStar/enso/internal/tools"
)

// startTestServer constructs a minimally-configured Server, listens on a
// temp unix socket, and runs the accept loop until the test ends. Returns
// the socket path, the mock provider so tests can script responses and
// dial against it, and the server itself for state inspection.
func startTestServer(t *testing.T) (socketPath string, mock *llmtest.Mock, srv *Server) {
	t.Helper()

	tmp := t.TempDir()
	socketPath = filepath.Join(tmp, "test.sock")

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	store, err := session.OpenAt(filepath.Join(tmp, "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	mock = llmtest.NewT(t)

	s := &Server{
		cfg: &config.Config{}, // empty defaults — Permissions.Mode "" → "prompt", but we use Yolo
		provider: &llm.Provider{
			Name:          "test",
			Client:        mock,
			Model:         "fake",
			ContextWindow: 1_000_000,
			Pool:          llm.NewPool(1),
		},
		registry:     tools.NewRegistry(),
		store:        store,
		listener:     listener,
		sessions:     map[string]*sessionState{},
		globalAgents: &atomic.Int64{},
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go s.acceptLoop(ctx)

	// Drop sessions when the test ends so background agent goroutines
	// don't outlive the test.
	t.Cleanup(func() {
		s.mu.Lock()
		for _, sess := range s.sessions {
			sess.cancel()
		}
		s.mu.Unlock()
	})

	return socketPath, mock, s
}

// waitForScripts polls mock.CallCount until the agent goroutine has
// consumed at least `want` scripts. Without this, tests that push
// scripts but don't otherwise observe agent output (e.g. ListSessions)
// race t.Cleanup, which cancels the session before the agent's Chat
// goroutine runs — leaving scripts in the queue and tripping
// llmtest.NewT's "unconsumed scripts" failure.
func waitForScripts(t *testing.T, mock *llmtest.Mock, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if mock.CallCount() >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("waitForScripts: agent consumed %d/%d scripts within deadline", mock.CallCount(), want)
}

// dial opens a fresh control connection to a daemon under test.
func dial(t *testing.T, socketPath string) *Client {
	t.Helper()
	c, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return &Client{conn: c}
}

func TestDaemon_CreateSessionStreamsEvents(t *testing.T) {
	socketPath, mock, _ := startTestServer(t)
	mock.Push(llmtest.Script{Text: "hello back"})

	control := dial(t, socketPath)
	info, err := control.CreateSession(CreateSessionReq{
		Prompt: "say hi",
		Cwd:    t.TempDir(),
		Yolo:   true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if info.ID == "" {
		t.Fatal("got empty session id")
	}

	// Open a second conn to stream events. FromSeq=0 means we'll get
	// any events the agent has already published (replayed from the
	// ring) plus everything that comes after.
	streamConn := dial(t, socketPath)
	events, err := streamConn.Subscribe(SubscribeReq{SessionID: info.ID, FromSeq: 0})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	deadline := time.After(3 * time.Second)
	var sawUser, sawDelta, sawDone bool
	for !(sawUser && sawDelta && sawDone) {
		select {
		case e, ok := <-events:
			if !ok {
				t.Fatalf("event stream closed early: user=%v delta=%v done=%v", sawUser, sawDelta, sawDone)
			}
			// Every event must carry its session_id so multi-session
			// observers can route without keeping outer context.
			if e.SessionID != info.ID {
				t.Errorf("event %q: SessionID=%q, want %q", e.Type, e.SessionID, info.ID)
			}
			switch e.Type {
			case "UserMessage":
				sawUser = true
			case "AssistantDelta":
				sawDelta = true
			case "AssistantDone":
				sawDone = true
			}
		case <-deadline:
			t.Fatalf("timeout: user=%v delta=%v done=%v", sawUser, sawDelta, sawDone)
		}
	}
}

func TestDaemon_ListSessionsReturnsCreated(t *testing.T) {
	socketPath, mock, _ := startTestServer(t)
	mock.Push(llmtest.Script{Text: "ok"})

	createConn := dial(t, socketPath)
	info, err := createConn.CreateSession(CreateSessionReq{
		Prompt: "hello",
		Cwd:    t.TempDir(),
		Yolo:   true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	listConn := dial(t, socketPath)
	sessions, err := listConn.ListSessions()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("want 1 session, got %d (%+v)", len(sessions), sessions)
	}
	if sessions[0].ID != info.ID {
		t.Errorf("listed id = %q, want %q", sessions[0].ID, info.ID)
	}

	// Sync with the agent goroutine before cleanup so the pushed script
	// is actually consumed (otherwise the cancel-on-cleanup races and
	// llmtest reports unconsumed scripts).
	waitForScripts(t, mock, 1)
}

func TestDaemon_FollowupSubmitTriggersSecondTurn(t *testing.T) {
	socketPath, mock, _ := startTestServer(t)
	mock.Push(llmtest.Script{Text: "first reply"})
	mock.Push(llmtest.Script{Text: "second reply"})

	control := dial(t, socketPath)
	info, err := control.CreateSession(CreateSessionReq{
		Prompt: "first",
		Cwd:    t.TempDir(),
		Yolo:   true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Subscribe and wait for the first AssistantDone before submitting
	// the follow-up — otherwise the agent's input channel queues the
	// second prompt and we can't tell which turn produced which event.
	streamConn := dial(t, socketPath)
	events, err := streamConn.Subscribe(SubscribeReq{SessionID: info.ID, FromSeq: 0})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	waitForDone := func() {
		t.Helper()
		deadline := time.After(3 * time.Second)
		for {
			select {
			case e, ok := <-events:
				if !ok {
					t.Fatal("stream closed")
				}
				if e.Type == "AssistantDone" {
					return
				}
			case <-deadline:
				t.Fatal("timeout waiting for AssistantDone")
			}
		}
	}

	waitForDone()

	if err := control.Submit(SubmitReq{SessionID: info.ID, Message: "second"}); err != nil {
		t.Fatalf("submit: %v", err)
	}
	waitForDone()

	if mock.CallCount() != 2 {
		t.Errorf("expected 2 model turns, got %d", mock.CallCount())
	}
}

// TestDaemon_SubmitQueueFullReturnsError: while the agent is mid-turn it
// does not drain inputCh, so a client flooding submits must get a
// "queue full" error frame back — NOT block the connection's read loop
// (which would also wedge Cancel/PermissionResponse on that conn).
func TestDaemon_SubmitQueueFullReturnsError(t *testing.T) {
	socketPath, mock, _ := startTestServer(t)

	// Gate the first (and only) turn so the agent is provably mid-turn
	// and not reading inputCh. Never released: the session cancel in
	// startTestServer's cleanup unblocks the mock's stream goroutine.
	gate := make(chan struct{})
	mock.Push(llmtest.Script{Text: "stuck", Gate: gate})

	control := dial(t, socketPath)
	info, err := control.CreateSession(CreateSessionReq{
		Prompt: "first",
		Cwd:    t.TempDir(),
		Yolo:   true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// The initial prompt is consumed before the turn starts, so once the
	// mock has seen the call, inputCh is empty and nobody is draining it.
	waitForScripts(t, mock, 1)

	// Fill all 16 slots; each succeeds silently (submit has no ack).
	for i := 0; i < 16; i++ {
		if err := control.Submit(SubmitReq{SessionID: info.ID, Message: "queued"}); err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
	}

	// The 17th must come back as an error frame, not block handleConn.
	if err := WriteMessage(control.conn, KindSubmit, SubmitReq{SessionID: info.ID, Message: "overflow"}); err != nil {
		t.Fatalf("write overflow submit: %v", err)
	}
	_ = control.conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	resp, err := ReadMessage(control.conn)
	if err != nil {
		t.Fatalf("read overflow response (read loop wedged?): %v", err)
	}
	if resp.Kind != KindError {
		t.Fatalf("overflow response kind = %s, want %s", resp.Kind, KindError)
	}
	var er ErrorResp
	if err := json.Unmarshal(resp.Body, &er); err != nil {
		t.Fatalf("decode error resp: %v", err)
	}
	if !strings.Contains(er.Message, "input queue full") {
		t.Fatalf("error message = %q, want it to mention the full input queue", er.Message)
	}

	// The same conn must still dispatch — Cancel aborts the gated turn,
	// which also drains the queued submits so no extra model calls fire.
	streamConn := dial(t, socketPath)
	events, err := streamConn.Subscribe(SubscribeReq{SessionID: info.ID, FromSeq: 0})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	_ = control.conn.SetReadDeadline(time.Time{})
	if err := control.Cancel(CancelReq{SessionID: info.ID}); err != nil {
		t.Fatalf("cancel after queue-full error: %v", err)
	}
	// Wait for the post-drain AgentIdle before the test ends: cleanup's
	// session cancel must not race the 16 still-queued inputs (Run's
	// select picks randomly between ctx.Done and a non-empty inputCh,
	// which would start turns the mock has no scripts for).
	deadline := time.After(3 * time.Second)
	for {
		select {
		case e, ok := <-events:
			if !ok {
				t.Fatal("event stream closed before AgentIdle")
			}
			if e.Type == "AgentIdle" {
				return
			}
		case <-deadline:
			t.Fatal("timeout waiting for AgentIdle after cancel")
		}
	}
}
