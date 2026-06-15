// SPDX-License-Identifier: AGPL-3.0-or-later

package bubble

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/TaraTheStar/enso/internal/backend/workspace"
	"github.com/TaraTheStar/enso/internal/session"
)

// rewindPromptEnv carries the rewound-away turn's text across the
// re-exec so the resumed session pre-fills it into the input box (the
// user can re-send or edit it — mirrors Claude Code's /rewind).
const rewindPromptEnv = "ENSO_REWIND_PROMPT"

// rewindMode is which half (or both) of a checkpoint a /rewind restores.
type rewindMode int

const (
	rewindBoth         rewindMode = iota // files + conversation
	rewindConversation                   // conversation only (keep files)
	rewindCode                           // files only (keep conversation)
)

// pendingRewindReq is the rewind the model hands back to run.go after the
// program quits (mirrors m.pendingSwitch). run.go performs the restore +
// re-exec once the worker is torn down and the terminal is restored.
type pendingRewindReq struct {
	seq    int
	mode   rewindMode
	prompt string // user-message text at seq, re-dropped for conversation/both
}

// rewindOverlayData backs the /rewind alt-screen overlay. Two stages:
// pick a turn (the checkpoint list), then pick what to restore (the mode
// menu). Mirrors sessionsOverlayData's eager-load-once pattern.
type rewindOverlayData struct {
	store     *session.Store
	sessionID string

	loaded  bool
	points  []session.RewindPoint
	loadErr error
	sel     int
	// choosingMode is stage 2: the three-way restore menu for points[sel].
	choosingMode bool
}

func (d *rewindOverlayData) reset() {
	d.loaded = false
	d.points = nil
	d.loadErr = nil
	d.sel = 0
	d.choosingMode = false
}

// load fetches the checkpoint list once. Idempotent until reset().
func (d *rewindOverlayData) load() {
	if d.loaded {
		return
	}
	d.loaded = true
	if d.store == nil || d.sessionID == "" {
		d.loadErr = fmt.Errorf("session store unavailable (running --ephemeral?)")
		return
	}
	pts, err := session.ListRewindPoints(d.store, d.sessionID)
	if err != nil {
		d.loadErr = err
		return
	}
	d.points = pts
	// Default to the most recent checkpoint (the common "undo the last
	// thing" case) so Enter immediately offers the latest turn.
	if len(pts) > 0 {
		d.sel = len(pts) - 1
	}
}

// handleRewindKey routes keys to the /rewind overlay. Stage 1 (list):
// up/down move, Enter advances to the mode menu, Esc closes. Stage 2
// (mode menu): 1/2/3 pick a restore mode and commit (set m.pendingRewind
// + quit so run.go performs it), Esc returns to the list.
func (m *model) handleRewindKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	d := m.rewind
	if d.choosingMode {
		switch msg.String() {
		case "esc":
			d.choosingMode = false
			return m, nil
		case "1":
			return m.commitRewind(rewindBoth)
		case "2":
			return m.commitRewind(rewindConversation)
		case "3":
			return m.commitRewind(rewindCode)
		}
		return m, nil
	}

	switch msg.String() {
	case "esc":
		m.rewindOpen = false
		return m, nil
	case "up", "ctrl+p":
		if d.sel > 0 {
			d.sel--
		}
		return m, nil
	case "down", "ctrl+n":
		if d.sel < len(d.points)-1 {
			d.sel++
		}
		return m, nil
	case "enter":
		if len(d.points) == 0 {
			m.rewindOpen = false
			return m, nil
		}
		d.choosingMode = true
		return m, nil
	}
	return m, nil
}

// commitRewind records the chosen rewind and quits so run.go can apply
// it after teardown (same hand-off as m.pendingSwitch).
func (m *model) commitRewind(mode rewindMode) (tea.Model, tea.Cmd) {
	pt := m.rewind.points[m.rewind.sel]
	m.pendingRewind = &pendingRewindReq{seq: pt.Seq, mode: mode, prompt: pt.Preview}
	m.rewindOpen = false
	m.quitting = true
	return m, tea.Quit
}

// renderRewindOverlay draws the /rewind alt-screen view for the current
// stage. height bounds the visible list so a long checkpoint history
// scrolls with the selection.
func renderRewindOverlay(d *rewindOverlayData, width, height int) string {
	_ = width
	title := lipgloss.NewStyle().Foreground(paletteHex("lavender")).Bold(true)
	footerStyle := lipgloss.NewStyle().Foreground(paletteHex("comment")).Faint(true)

	if d.loadErr != nil {
		return lipgloss.JoinVertical(lipgloss.Left,
			title.Render("⏵ rewind"), "",
			errorStyle.Render("error: "+d.loadErr.Error()), "",
			footerStyle.Render("Esc  close"))
	}
	if len(d.points) == 0 {
		return lipgloss.JoinVertical(lipgloss.Left,
			title.Render("⏵ rewind"), "",
			statusStyle.Render("(no checkpoints yet — they're captured per turn as you work)"), "",
			footerStyle.Render("Esc  close"))
	}

	// Stage 2: the restore-mode menu for the selected turn.
	if d.choosingMode {
		pt := d.points[d.sel]
		opts := strings.Join([]string{
			"  " + lipgloss.NewStyle().Foreground(paletteHex("mauve")).Render("[1]") + " restore code + conversation",
			"  " + lipgloss.NewStyle().Foreground(paletteHex("mauve")).Render("[2]") + " restore conversation only (keep files)",
			"  " + lipgloss.NewStyle().Foreground(paletteHex("mauve")).Render("[3]") + " restore code only (keep conversation)",
		}, "\n")
		return lipgloss.JoinVertical(lipgloss.Left,
			title.Render(fmt.Sprintf("⏵ rewind to turn %d", pt.Seq)), "",
			statusStyle.Render(previewLine(pt.Preview)), "",
			opts, "",
			footerStyle.Render("1/2/3 choose · Esc back"))
	}

	// Stage 1: the checkpoint list.
	var rows []string
	for i, pt := range d.points {
		line := fmt.Sprintf("%s  %s  %s",
			statusStyle.Render(fmt.Sprintf("turn %d", pt.Seq)),
			previewLine(pt.Preview),
			statusStyle.Render(relTime(pt.CreatedAt)))
		if i == d.sel {
			line = lipgloss.NewStyle().Foreground(paletteHex("mauve")).Bold(true).Render("› ") + line
		} else {
			line = "  " + line
		}
		rows = append(rows, line)
	}
	rows, above, below := windowList(rows, d.sel, height-sessionsOverlayChrome)

	footer := footerStyle.Render(
		fmt.Sprintf("Enter choose · ↑/↓ move · Esc cancel    %d checkpoint%s",
			len(d.points), plural(len(d.points))) + scrollSuffix(above, below))

	return lipgloss.JoinVertical(lipgloss.Left,
		title.Render("⏵ rewind · pick a turn to undo to"), "",
		strings.Join(rows, "\n"), "",
		footer)
}

// previewLine flattens and truncates a user message for one-line display.
func previewLine(s string) string {
	p := strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	if p == "" {
		return "(no message text)"
	}
	return clipRunes(p, 60)
}

// performRewind applies a chosen rewind after the program has quit and
// the worker is torn down, then re-execs into the (same, possibly
// truncated) session — reloading the worker's in-memory history from the
// DB and re-entering the TUI. Called from run.go, the analogue of
// execIntoSession for m.pendingSwitch. Never returns on success
// (syscall.Exec).
func performRewind(req pendingRewindReq, store *session.Store, sessionID, cwd string) error {
	applyRewind(req, store, sessionID, cwd)
	return execIntoSession(sessionID)
}

// applyRewind performs the filesystem + conversation mutations of a
// rewind (everything except the terminal re-exec), so the orchestration
// is unit-testable without replacing the process image.
//
// Restore is FS-level on the host project dir (correct for the LOCAL
// backend, where the worker shares it). Conversation rewind backs up the
// full pre-rewind session first (redo safety) before the destructive
// truncate, and stages the rewound prompt in rewindPromptEnv for the
// re-exec'd process to pre-fill. Best-effort: a step that fails logs to
// stderr and the rewind proceeds with the rest, never leaving the user
// stranded.
func applyRewind(req pendingRewindReq, store *session.Store, sessionID, cwd string) {
	ctx := context.Background()

	if req.mode == rewindBoth || req.mode == rewindCode {
		if dir, err := session.CheckpointStoreDir(sessionID); err == nil {
			snap := filepath.Join(dir, strconv.Itoa(req.seq))
			if _, err := workspace.RestoreTree(ctx, snap, cwd); err != nil {
				fmt.Fprintf(os.Stderr, "rewind: restore files failed: %v\n", err)
			}
		}
	}

	if (req.mode == rewindBoth || req.mode == rewindConversation) && store != nil {
		// Preserve the rewound-away conversation as a separate session so
		// it stays resumable (the snapshots cover the file side).
		if backupID, err := session.Fork(store, sessionID); err == nil {
			fmt.Fprintf(os.Stdout, "rewind: previous conversation saved — resume with `enso --session %s`\n", backupID)
		} else {
			fmt.Fprintf(os.Stderr, "rewind: backup fork failed: %v\n", err)
		}
		if err := session.TruncateAfter(store, sessionID, req.seq-1); err != nil {
			fmt.Fprintf(os.Stderr, "rewind: truncate conversation failed: %v\n", err)
		}
		// Drop now-orphaned checkpoints (and their on-disk snapshots).
		if freed, err := session.DeleteCheckpointsAfter(store, sessionID, req.seq-1); err == nil {
			if dir, derr := session.CheckpointStoreDir(sessionID); derr == nil {
				for _, id := range freed {
					_ = os.RemoveAll(filepath.Join(dir, id))
				}
			}
		}
		if strings.TrimSpace(req.prompt) != "" {
			_ = os.Setenv(rewindPromptEnv, req.prompt)
		}
	}
}
