// SPDX-License-Identifier: AGPL-3.0-or-later

package bubble

import (
	"fmt"
	"strings"
	"time"

	"charm.land/lipgloss/v2"

	"github.com/TaraTheStar/enso/internal/session"
)

// sessionsOverlayData is the state for the Ctrl-R recent-sessions
// picker. The list is loaded eagerly when the overlay opens (cheap —
// LIMIT 50 against the local SQLite store) and held until close, so
// arrow navigation doesn't re-query.
type sessionsOverlayData struct {
	store *session.Store

	loaded   bool
	sessions []session.SessionInfoWithStats
	loadErr  error
	sel      int
}

func (s *sessionsOverlayData) reset() {
	s.loaded = false
	s.sessions = nil
	s.loadErr = nil
	s.sel = 0
}

// load eagerly fetches the recent-sessions list. Idempotent — once
// loaded the cached slice is reused until reset().
func (s *sessionsOverlayData) load() {
	if s.loaded {
		return
	}
	s.loaded = true
	if s.store == nil {
		s.loadErr = fmt.Errorf("session store unavailable (running --ephemeral?)")
		return
	}
	infos, err := session.ListRecentWithStats(s.store, 50)
	if err != nil {
		s.loadErr = err
		return
	}
	s.sessions = infos
}

// renderSessionsOverlay produces the alt-screen view for the recent
// sessions picker. Format mirrors what the splash and /sessions show:
// short id · last user message preview · time · cwd.
func renderSessionsOverlay(d *sessionsOverlayData, width, height int) string {
	_ = width
	_ = height

	title := lipgloss.NewStyle().
		Foreground(paletteHex("lavender")).
		Bold(true).
		Render("⏵ recent sessions")

	if d.loadErr != nil {
		return lipgloss.JoinVertical(lipgloss.Left,
			title, "",
			errorStyle.Render("error: "+d.loadErr.Error()),
			"",
			lipgloss.NewStyle().Foreground(paletteHex("comment")).Faint(true).Render("Esc  close"),
		)
	}
	if len(d.sessions) == 0 {
		return lipgloss.JoinVertical(lipgloss.Left,
			title, "",
			statusStyle.Render("(no sessions yet)"),
			"",
			lipgloss.NewStyle().Foreground(paletteHex("comment")).Faint(true).Render("Esc  close"),
		)
	}

	var rows []string
	for i, info := range d.sessions {
		short := info.ID
		if len(short) > 8 {
			short = short[:8]
		}
		preview := strings.ReplaceAll(strings.TrimSpace(info.LastUserMessage), "\n", " ")
		if preview == "" {
			preview = "(no user message yet)"
		}
		const previewMax = 60
		if len(preview) > previewMax {
			preview = preview[:previewMax-1] + "…"
		}
		rel := relTime(info.UpdatedAt)
		flag := ""
		if info.Interrupted {
			flag = errorStyle.Render(" [interrupted]")
		}
		line := fmt.Sprintf("%s  %s  %s  %s%s",
			statusStyle.Render(short),
			preview,
			statusStyle.Render(rel),
			statusStyle.Render(info.Cwd),
			flag,
		)
		if i == d.sel {
			line = lipgloss.NewStyle().Foreground(paletteHex("mauve")).Bold(true).Render("› ") + line
		} else {
			line = "  " + line
		}
		rows = append(rows, line)
	}

	footer := lipgloss.NewStyle().
		Foreground(paletteHex("comment")).
		Faint(true).
		Render(fmt.Sprintf("Enter switch · ↑/↓ move · Esc cancel    %d session%s", len(d.sessions), plural(len(d.sessions))))

	return lipgloss.JoinVertical(lipgloss.Left,
		title, "",
		strings.Join(rows, "\n"),
		"",
		footer,
	)
}

// relTime renders a wall-clock time as "Nm ago" / "Nh ago" / "Nd ago"
// for compact display in the sessions list.
func relTime(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}
