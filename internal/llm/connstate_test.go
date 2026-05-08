// SPDX-License-Identifier: AGPL-3.0-or-later

package llm

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// seqRT is a RoundTripper that returns a scripted sequence of
// (response, error) pairs. Each round-trip pops the next step;
// extra calls beyond the script return an explicit "exhausted" error
// so a too-many-retries bug surfaces as a test failure rather than a
// hang. Tracks call count for assertions.
type seqRT struct {
	steps []seqStep
	calls atomic.Int32
}

type seqStep struct {
	resp *http.Response
	err  error
}

func (s *seqRT) RoundTrip(*http.Request) (*http.Response, error) {
	i := int(s.calls.Add(1)) - 1
	if i >= len(s.steps) {
		return nil, errors.New("seqRT: script exhausted")
	}
	step := s.steps[i]
	return step.resp, step.err
}

// connRefused fabricates a transport-class error indistinguishable
// from a real ECONNREFUSED dial failure for classifyTransportError's
// purposes.
func connRefused() error {
	return &net.OpError{Op: "dial", Net: "tcp", Err: &os.SyscallError{Err: syscall.ECONNREFUSED}}
}

func okResp(body string) *http.Response {
	return &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
}

func newTestClient(rt http.RoundTripper) *Client {
	return &Client{
		Endpoint:      "http://x",
		HTTPClient:    &http.Client{Transport: rt},
		RetryBackoff:  func(int) time.Duration { return 1 * time.Millisecond },
		ProbeInterval: 5 * time.Millisecond,
	}
}

func TestConnState_String(t *testing.T) {
	cases := map[ConnState]string{
		StateConnected:    "connected",
		StateReconnecting: "reconnecting",
		StateDisconnected: "disconnected",
	}
	for s, want := range cases {
		if got := s.String(); got != want {
			t.Errorf("State(%d).String() = %q, want %q", s, got, want)
		}
	}
}

func TestDoChatRequest_TransportRetryThenSuccess(t *testing.T) {
	rt := &seqRT{steps: []seqStep{
		{err: connRefused()},
		{err: connRefused()},
		{resp: okResp("ok")},
	}}
	c := newTestClient(rt)

	resp, err := c.doChatRequest(context.Background(), []byte("{}"))
	if err != nil {
		t.Fatalf("expected retry-recovery success, got %v", err)
	}
	resp.Body.Close()

	if calls := rt.calls.Load(); calls != 3 {
		t.Errorf("calls=%d, want 3 (initial + 2 retries)", calls)
	}
	// doChatRequest leaves state at Reconnecting on success — flipping
	// to Connected is Chat()'s responsibility once it commits to the
	// returned response.
	if got := c.conn.get(); got != StateReconnecting {
		t.Errorf("state=%v, want Reconnecting (Chat finalises Connected)", got)
	}
}

func TestDoChatRequest_TransportRetryExhausted(t *testing.T) {
	rt := &seqRT{steps: []seqStep{
		{err: connRefused()},
		{err: connRefused()},
		{err: connRefused()},
	}}
	c := newTestClient(rt)

	_, err := c.doChatRequest(context.Background(), []byte("{}"))
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	if calls := rt.calls.Load(); calls != int32(maxChatRetries+1) {
		t.Errorf("calls=%d, want %d", calls, maxChatRetries+1)
	}
	// Disconnected + probe start happen in Chat(), not doChatRequest —
	// here we only assert Reconnecting was set during the retry loop.
	if got := c.conn.get(); got != StateReconnecting {
		t.Errorf("state=%v, want Reconnecting at exhaustion", got)
	}
}

func TestDoChatRequest_NonTransportNoRetry(t *testing.T) {
	rt := &seqRT{steps: []seqStep{
		// Bare error that classifyTransportError refuses to recognize:
		// no retry should happen.
		{err: errors.New("some random non-network thing")},
	}}
	c := newTestClient(rt)

	_, err := c.doChatRequest(context.Background(), []byte("{}"))
	if err == nil {
		t.Fatal("expected error to surface")
	}
	if calls := rt.calls.Load(); calls != 1 {
		t.Errorf("calls=%d, want 1 (non-transport errors must not retry)", calls)
	}
	if got := c.conn.get(); got != StateConnected {
		t.Errorf("state=%v, want Connected (transport never registered as failed)", got)
	}
}

func TestDoChatRequest_ContextCancelNoRetry(t *testing.T) {
	rt := &seqRT{steps: []seqStep{
		{err: context.Canceled},
	}}
	c := newTestClient(rt)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := c.doChatRequest(ctx, []byte("{}"))
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err=%v, want context.Canceled preserved", err)
	}
	if calls := rt.calls.Load(); calls != 1 {
		t.Errorf("calls=%d, want 1 (cancel must not retry)", calls)
	}
}

func TestProbeOnce_TransportFailureFalse(t *testing.T) {
	rt := &seqRT{steps: []seqStep{{err: connRefused()}}}
	c := newTestClient(rt)
	if c.probeOnce() {
		t.Error("probeOnce on refused connection returned true")
	}
}

func TestProbeOnce_AnyResponseTrue(t *testing.T) {
	// Even a 500 means TLS+TCP succeeded; probe should call it
	// recovered. The TUI cares about transport, not API health.
	rt := &seqRT{steps: []seqStep{
		{resp: &http.Response{
			StatusCode: 500,
			Body:       io.NopCloser(strings.NewReader("upstream broken")),
			Header:     make(http.Header),
		}},
	}}
	c := newTestClient(rt)
	if !c.probeOnce() {
		t.Error("probeOnce on 500 response should be true (transport is fine)")
	}
}

func TestProbeLoop_RecoversToConnected(t *testing.T) {
	rt := &seqRT{steps: []seqStep{
		{err: connRefused()}, // first probe still down
		{resp: okResp("ok")}, // second probe succeeds
	}}
	c := newTestClient(rt)
	c.conn.set(StateDisconnected)
	c.startRecoveryProbe()

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if c.conn.get() == StateConnected {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("probe never recovered: state=%v", c.conn.get())
}

func TestProbeLoop_StopsWhenStateChanged(t *testing.T) {
	// If something else (e.g. a successful Chat call) moves us out of
	// Disconnected, the probe goroutine should notice and exit on its
	// next tick rather than continuing to ping forever.
	rt := &seqRT{steps: []seqStep{{err: connRefused()}, {err: connRefused()}}}
	c := newTestClient(rt)
	c.conn.set(StateDisconnected)
	c.startRecoveryProbe()

	// External transition.
	c.conn.set(StateConnected)

	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		c.conn.mu.Lock()
		probing := c.conn.probing
		c.conn.mu.Unlock()
		if !probing {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("probe goroutine did not exit after external state transition")
}

func TestChat_TransportExhaustionDisconnectsAndStartsProbe(t *testing.T) {
	// All three attempts fail with transport errors; Chat should mark
	// the tracker Disconnected and arm the probe goroutine. Probe will
	// keep failing (script-exhausted error counts as transport-shaped
	// to the tracker, but the test only cares about the initial flip).
	rt := &seqRT{steps: []seqStep{
		{err: connRefused()},
		{err: connRefused()},
		{err: connRefused()},
	}}
	c := newTestClient(rt)
	c.Model = "test"

	_, err := c.Chat(context.Background(), ChatRequest{})
	if err == nil {
		t.Fatal("expected error from exhausted Chat")
	}
	if got := c.conn.get(); got != StateDisconnected {
		t.Errorf("state=%v, want Disconnected after retry exhaustion", got)
	}
	c.conn.mu.Lock()
	probing := c.conn.probing
	c.conn.mu.Unlock()
	if !probing {
		t.Error("recovery probe should be running after Disconnected transition")
	}

	// Cleanup: let the probe exit by flipping to Connected externally.
	c.conn.set(StateConnected)
	time.Sleep(20 * time.Millisecond)
}

func TestChat_HTTPErrorDoesNotMarkDisconnected(t *testing.T) {
	// An HTTP 500 means TLS+TCP succeeded — the indicator must stay
	// healthy. Rate-limit / provider-error rendering is a separate
	// concern (TODO P2 #11).
	rt := &seqRT{steps: []seqStep{
		{resp: &http.Response{
			StatusCode: 500,
			Body:       io.NopCloser(strings.NewReader("internal error")),
			Header:     make(http.Header),
		}},
	}}
	c := newTestClient(rt)
	c.Model = "test"

	_, err := c.Chat(context.Background(), ChatRequest{})
	if err == nil {
		t.Fatal("expected HTTP-status error to surface")
	}
	if got := c.conn.get(); got != StateConnected {
		t.Errorf("state=%v, want Connected (HTTP errors are not transport failures)", got)
	}
}

func TestStartRecoveryProbe_Idempotent(t *testing.T) {
	// claimProbe ensures that even if the disconnect path runs twice
	// (e.g. concurrent retry exhaustion from two parallel Chat calls
	// once we add concurrency at the agent level), we only spawn one
	// goroutine. With a one-step success script, three parallel start
	// calls must still produce exactly one round-trip — a second
	// goroutine would error on the exhausted script.
	rt := &seqRT{steps: []seqStep{{resp: okResp("ok")}}}
	c := newTestClient(rt)
	c.conn.set(StateDisconnected)

	c.startRecoveryProbe()
	c.startRecoveryProbe()
	c.startRecoveryProbe()

	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if c.conn.get() == StateConnected {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if got := c.conn.get(); got != StateConnected {
		t.Fatalf("probe never recovered: state=%v", got)
	}
	// Settle: any extra goroutine would tick at ProbeInterval and hit
	// the exhausted script.
	time.Sleep(20 * time.Millisecond)
	if calls := rt.calls.Load(); calls != 1 {
		t.Errorf("calls=%d, want 1 (only one probe goroutine should run)", calls)
	}
}
