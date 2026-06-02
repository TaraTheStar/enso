// SPDX-License-Identifier: AGPL-3.0-or-later

package bubble

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/TaraTheStar/enso/internal/backend/workspace"
	"github.com/TaraTheStar/enso/internal/llm"
	"github.com/TaraTheStar/enso/internal/session"
)

func rewindModel() *model {
	return &model{
		rewind: &rewindOverlayData{
			loaded: true,
			points: []session.RewindPoint{
				{Seq: 1, Preview: "first question"},
				{Seq: 3, Preview: "second question"},
			},
			sel: 1,
		},
		rewindOpen: true,
	}
}

func TestRewindOverlay_NavigateAndChooseMode(t *testing.T) {
	m := rewindModel()

	// Up moves the selection off the last item.
	m.handleRewindKey(tea.KeyPressMsg{Code: tea.KeyUp})
	if m.rewind.sel != 0 {
		t.Fatalf("up: sel = %d, want 0", m.rewind.sel)
	}
	// Down returns to the last.
	m.handleRewindKey(tea.KeyPressMsg{Code: tea.KeyDown})
	if m.rewind.sel != 1 {
		t.Fatalf("down: sel = %d, want 1", m.rewind.sel)
	}
	// Enter advances to the mode menu (stage 2) without committing.
	m.handleRewindKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	if !m.rewind.choosingMode {
		t.Fatal("enter should advance to the mode menu")
	}
	if m.pendingRewind != nil {
		t.Fatal("enter must not commit a rewind on its own")
	}
}

func TestRewindOverlay_CommitConversationMode(t *testing.T) {
	m := rewindModel()
	m.rewind.sel = 1 // "second question", seq 3
	m.rewind.choosingMode = true

	// "2" = restore conversation only.
	m.handleRewindKey(tea.KeyPressMsg{Code: '2', Text: "2"})
	if m.pendingRewind == nil {
		t.Fatal("mode key should commit a pending rewind")
	}
	if m.pendingRewind.seq != 3 || m.pendingRewind.mode != rewindConversation {
		t.Errorf("pendingRewind = %+v, want seq=3 mode=conversation", *m.pendingRewind)
	}
	if m.pendingRewind.prompt != "second question" {
		t.Errorf("re-drop prompt = %q, want %q", m.pendingRewind.prompt, "second question")
	}
	if !m.quitting || m.rewindOpen {
		t.Error("commit should close the overlay and quit")
	}
}

func TestRewindOverlay_EscBacksOutOfModeMenu(t *testing.T) {
	m := rewindModel()
	m.rewind.choosingMode = true
	m.handleRewindKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.rewind.choosingMode {
		t.Error("esc in mode menu should return to the list")
	}
	if !m.rewindOpen {
		t.Error("esc in mode menu should NOT close the overlay")
	}
	// Esc again (in the list) closes the overlay.
	m.handleRewindKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.rewindOpen {
		t.Error("esc in the list should close the overlay")
	}
}

func TestRenderRewindOverlay(t *testing.T) {
	m := rewindModel()
	list := ansi.Strip(renderRewindOverlay(m.rewind, 80, 24))
	for _, want := range []string{"rewind", "turn 1", "turn 3", "first question", "Enter choose"} {
		if !strings.Contains(list, want) {
			t.Errorf("list view missing %q:\n%s", want, list)
		}
	}

	m.rewind.choosingMode = true
	menu := ansi.Strip(renderRewindOverlay(m.rewind, 80, 24))
	for _, want := range []string{
		"rewind to turn 3",
		"restore code + conversation",
		"restore conversation only",
		"restore code only",
	} {
		if !strings.Contains(menu, want) {
			t.Errorf("mode menu missing %q:\n%s", want, menu)
		}
	}

	// Empty state.
	empty := ansi.Strip(renderRewindOverlay(&rewindOverlayData{loaded: true}, 80, 24))
	if !strings.Contains(empty, "no checkpoints") {
		t.Errorf("empty view missing the no-checkpoints hint:\n%s", empty)
	}
}

// TestApplyRewind_CodeOnly restores files from the snapshot but leaves
// the conversation intact.
func TestApplyRewind_CodeOnly(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	store := openRewindStore(t)
	sid, cwd := seedSessionWithCheckpoint(t, store)

	// Mutate the file after the checkpoint, then rewind code-only.
	if err := os.WriteFile(filepath.Join(cwd, "f.txt"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	applyRewind(pendingRewindReq{seq: 1, mode: rewindCode}, store, sid, cwd)

	if got, _ := os.ReadFile(filepath.Join(cwd, "f.txt")); string(got) != "v1" {
		t.Errorf("code-only rewind did not restore file: %q", got)
	}
	// Conversation untouched.
	state, _ := session.Load(store, sid)
	if len(state.History) == 0 {
		t.Error("code-only rewind must not truncate the conversation")
	}
}

// TestApplyRewind_ConversationOnly truncates history (with a backup) and
// stages the re-drop prompt, leaving files untouched.
func TestApplyRewind_ConversationOnly(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv(rewindPromptEnv, "")
	store := openRewindStore(t)
	sid, cwd := seedSessionWithCheckpoint(t, store)

	// Add later turns so there's something to rewind away.
	w, _ := session.AttachWriter(store, sid)
	_, _ = w.AppendMessage(llm.Message{Role: "assistant", Content: "a1"}, "")
	_, _ = w.AppendMessage(llm.Message{Role: "user", Content: "q2"}, "")

	if err := os.WriteFile(filepath.Join(cwd, "f.txt"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	applyRewind(pendingRewindReq{seq: 1, mode: rewindConversation, prompt: "q1"}, store, sid, cwd)

	// Conversation rewound to before seq 1 (empty).
	state, _ := session.Load(store, sid)
	if len(state.History) != 0 {
		t.Errorf("conversation-only rewind to seq 1 should empty history, got %d", len(state.History))
	}
	// Files NOT restored.
	if got, _ := os.ReadFile(filepath.Join(cwd, "f.txt")); string(got) != "changed" {
		t.Errorf("conversation-only rewind must not touch files, got %q", got)
	}
	// Re-drop prompt staged.
	if os.Getenv(rewindPromptEnv) != "q1" {
		t.Errorf("re-drop prompt env = %q, want q1", os.Getenv(rewindPromptEnv))
	}
	// A backup session exists (more than just our session row).
	all, _ := session.ListRecent(store, "", 10)
	if len(all) < 2 {
		t.Errorf("conversation rewind should have forked a backup session, sessions=%d", len(all))
	}
}

func openRewindStore(t *testing.T) *session.Store {
	t.Helper()
	s, err := session.OpenAt(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// seedSessionWithCheckpoint creates a session with one user turn at seq 1
// and a checkpoint snapshot of cwd (containing f.txt="v1") for that turn.
func seedSessionWithCheckpoint(t *testing.T, store *session.Store) (sessionID, cwd string) {
	t.Helper()
	cwd = t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "f.txt"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	w, err := session.NewSession(store, "qwen", "local", cwd)
	if err != nil {
		t.Fatal(err)
	}
	sessionID = w.SessionID()
	if _, err := w.AppendMessage(llm.Message{Role: "user", Content: "q1"}, ""); err != nil {
		t.Fatal(err)
	}
	dir, err := session.CheckpointStoreDir(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := workspace.SnapshotTree(context.Background(), cwd, filepath.Join(dir, "1")); err != nil {
		t.Fatal(err)
	}
	if err := w.RecordCheckpoint(1, "1"); err != nil {
		t.Fatal(err)
	}
	return sessionID, cwd
}
