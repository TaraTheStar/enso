// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !windows

package daemon

import (
	"context"
	"net"
	"path/filepath"
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
// the socket path and the mock provider so tests can script responses
// and dial against it.
func startTestServer(t *testing.T) (socketPath string, mock *llmtest.Mock) {
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

	return socketPath, mock
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
	socketPath, mock := startTestServer(t)
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
	socketPath, mock := startTestServer(t)
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
	socketPath, mock := startTestServer(t)
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
