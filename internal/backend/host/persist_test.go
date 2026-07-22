// SPDX-License-Identifier: AGPL-3.0-or-later

package host_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/TaraTheStar/azoth/llm"
	"github.com/TaraTheStar/enso/internal/backend"
	"github.com/TaraTheStar/enso/internal/backend/host"
	"github.com/TaraTheStar/enso/internal/backend/wire"
	"github.com/TaraTheStar/enso/internal/bus"
	"github.com/TaraTheStar/enso/internal/session"
)

// TestSessionPersistEnvelopesLandInHostStore covers the host leg of
// isolated-mode session persistence (the loop's MsgPersist* handling):
// for podman/lima this is the ONLY route session data reaches the host
// DB. It asserts:
//
//   - each MsgPersistMessage lands with the right seq / agent_id, and
//     the explicit wire Reasoning field is re-attached (llm.Message's
//     Reasoning is `json:"-"` and would otherwise be dropped);
//   - MsgPersistMessageUsage attaches to that agent's LAST APPLIED
//     message seq, per-agent, so interleaved sub-agent appends do not
//     cross-attribute;
//   - MsgPersistToolCall lands intact;
//   - the FAILURE path: when an AppendMessage fails host-side, the
//     cursor for that agent is invalidated so the following usage record
//     is SKIPPED rather than misattributed to the previous message.
func TestSessionPersistEnvelopesLandInHostStore(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "test.db")
	store, err := session.OpenAt(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	sid := "sess-persist"
	if _, err := session.NewSessionWithID(store, sid, "m", "test", tmp); err != nil {
		t.Fatalf("create session: %v", err)
	}
	writer, err := session.AttachWriter(store, sid)
	if err != nil {
		t.Fatalf("attach writer: %v", err)
	}

	usageTop := llm.MessageUsage{InputTokens: 100, OutputTokens: 50, TotalTokens: 150}
	usageSub := llm.MessageUsage{InputTokens: 7, OutputTokens: 3, TotalTokens: 10}
	// Distinctive counts: if the failure-path usage shows up ANYWHERE in
	// the DB the cursor invalidation regressed.
	usagePoisoned := llm.MessageUsage{InputTokens: 999, OutputTokens: 999, TotalTokens: 1998}

	// gate holds the worker before the failure leg so the test can first
	// observe the happy-path rows and then kill the store.
	gate := make(chan struct{})
	agentFn := func(ctx context.Context, _ backend.TaskSpec, ch backend.Channel) error {
		send := func(kind backend.MsgKind, v any) error {
			body, err := backend.NewBody(v)
			if err != nil {
				return err
			}
			return ch.Send(backend.Envelope{Kind: kind, Body: body})
		}
		asst := llm.Message{Role: "assistant", Content: "hello"}
		steps := []struct {
			kind backend.MsgKind
			v    any
		}{
			// Top-level turn: user, assistant (+reasoning), usage.
			{backend.MsgPersistMessage, wire.PersistMessage{Msg: llm.Message{Role: "user", Content: "hi"}}},
			{backend.MsgPersistMessage, wire.PersistMessage{Msg: asst, Reasoning: "chain-of-thought"}},
			{backend.MsgPersistMessageUsage, wire.PersistMessageUsage{Usage: usageTop}},
			// Interleaved sub-agent append: its usage must attach to ITS
			// message (seq 3), not the top-level cursor.
			{backend.MsgPersistMessage, wire.PersistMessage{Msg: llm.Message{Role: "assistant", Content: "sub work"}, AgentID: "sub1"}},
			{backend.MsgPersistMessageUsage, wire.PersistMessageUsage{Usage: usageSub, AgentID: "sub1"}},
			{backend.MsgPersistToolCall, wire.PersistToolCall{
				CallID: "c1", Name: "bash", Args: map[string]any{"cmd": "ls"},
				LLMOutput: "out", FullOutput: "full", Status: "ok",
			}},
		}
		for _, s := range steps {
			if err := send(s.kind, s.v); err != nil {
				return err
			}
		}

		<-gate // test closes the store, then opens the gate

		// Failure leg: AppendMessage fails host-side (store closed). The
		// usage that follows belongs to THIS failed message and must be
		// skipped, never attributed to seq 2.
		if err := send(backend.MsgPersistMessage, wire.PersistMessage{Msg: llm.Message{Role: "assistant", Content: "lost"}}); err != nil {
			return err
		}
		return send(backend.MsgPersistMessageUsage, wire.PersistMessageUsage{Usage: usagePoisoned})
	}

	busInst := bus.New()
	defer busInst.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	spec := backend.TaskSpec{TaskID: "persist", Cwd: tmp, SessionID: sid,
		Isolation: backend.IsolationSpec{Kind: "container"}}
	sess, err := host.Start(ctx, &capBackend{agent: agentFn}, spec, nil, busInst,
		host.WithWriter(writer))
	if err != nil {
		t.Fatalf("host.Start: %v", err)
	}
	defer sess.Close()

	// Wait until the happy-path tail (the tool call) is applied — the
	// loop applies in receipt order, so everything before it is in too.
	deadline := time.Now().Add(10 * time.Second)
	for {
		var n int
		_ = store.DB.QueryRow(`SELECT COUNT(*) FROM tool_calls WHERE session_id = ?`, sid).Scan(&n)
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the persisted tool call to land")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Kill the store so the next AppendMessage fails, then release the
	// failure leg.
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	close(gate)

	if err := sess.Wait(); err != nil {
		t.Fatalf("session ended with error: %v", err)
	}

	// Reopen and verify what the host actually persisted.
	store2, err := session.OpenAt(dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer store2.Close()

	rows, err := store2.DB.Query(
		`SELECT seq, role, content, agent_id, reasoning FROM messages WHERE session_id = ? ORDER BY seq`, sid)
	if err != nil {
		t.Fatalf("query messages: %v", err)
	}
	defer rows.Close()
	type msgRow struct {
		seq                               int
		role, content, agentID, reasoning string
	}
	var msgs []msgRow
	for rows.Next() {
		var m msgRow
		if err := rows.Scan(&m.seq, &m.role, &m.content, &m.agentID, &m.reasoning); err != nil {
			t.Fatalf("scan message: %v", err)
		}
		msgs = append(msgs, m)
	}
	want := []msgRow{
		{1, "user", "hi", "", ""},
		{2, "assistant", "hello", "", "chain-of-thought"},
		{3, "assistant", "sub work", "sub1", ""},
	}
	if len(msgs) != len(want) {
		t.Fatalf("persisted %d messages, want %d (the failed append must not land): %+v", len(msgs), len(want), msgs)
	}
	for i, w := range want {
		if msgs[i] != w {
			t.Errorf("message %d = %+v, want %+v", i, msgs[i], w)
		}
	}

	// Usage attribution: exactly the two happy-path rows, attached to the
	// per-agent last-applied seqs. The poisoned usage (whose message
	// failed to append) must appear NOWHERE — in particular not on seq 2.
	urows, err := store2.DB.Query(
		`SELECT seq, agent_id, input_tokens, output_tokens, total_tokens
		 FROM message_usage WHERE session_id = ? ORDER BY seq`, sid)
	if err != nil {
		t.Fatalf("query usage: %v", err)
	}
	defer urows.Close()
	type usageRow struct {
		seq            int
		agentID        string
		in, out, total int
	}
	var usages []usageRow
	for urows.Next() {
		var u usageRow
		if err := urows.Scan(&u.seq, &u.agentID, &u.in, &u.out, &u.total); err != nil {
			t.Fatalf("scan usage: %v", err)
		}
		usages = append(usages, u)
	}
	wantUsage := []usageRow{
		{2, "", 100, 50, 150},
		{3, "sub1", 7, 3, 10},
	}
	if len(usages) != len(wantUsage) {
		t.Fatalf("persisted %d usage rows, want %d (post-failure usage must be skipped): %+v",
			len(usages), len(wantUsage), usages)
	}
	for i, w := range wantUsage {
		if usages[i] != w {
			t.Errorf("usage %d = %+v, want %+v", i, usages[i], w)
		}
		if usages[i].in == 999 {
			t.Errorf("poisoned usage misattributed to seq %d", usages[i].seq)
		}
	}

	// Tool call round trip.
	var callID, name, args, llmOut, fullOut, status string
	err = store2.DB.QueryRow(
		`SELECT call_id, name, args, llm_output, full_output, status
		 FROM tool_calls WHERE session_id = ?`, sid).
		Scan(&callID, &name, &args, &llmOut, &fullOut, &status)
	if err != nil {
		t.Fatalf("query tool call: %v", err)
	}
	if callID != "c1" || name != "bash" || llmOut != "out" || fullOut != "full" || status != "ok" {
		t.Errorf("tool call = %q/%q/%q/%q/%q", callID, name, llmOut, fullOut, status)
	}
	if args != `{"cmd":"ls"}` {
		t.Errorf("tool args = %q, want %q", args, `{"cmd":"ls"}`)
	}
}
