// SPDX-License-Identifier: AGPL-3.0-or-later

// Package ui is the framework-agnostic UI surface for enso. It exposes
// a small entry-point API (Run, RunAttached, Options) that cmd/ and
// the rest of the app call without ever importing a TUI framework
// directly. The active backend is the scrollback-native Bubble Tea
// implementation in internal/ui/bubble; if a future backend is added
// it slots in here without spreading framework imports across the
// codebase.
package ui

import "github.com/TaraTheStar/enso/internal/ui/bubble"

// Options configures a UI run. Mirrors today's bubble.Options. Adding
// fields here means adding them to the backend; keep it small.
type Options struct {
	Yolo      bool
	Session   string
	Ephemeral bool
	MaxTurns  int
	Config    string
	Agent     string
}

// Run launches the interactive UI.
func Run(opts Options) error {
	return bubble.Run(bubble.Options{
		Yolo:      opts.Yolo,
		Session:   opts.Session,
		Ephemeral: opts.Ephemeral,
		MaxTurns:  opts.MaxTurns,
		Config:    opts.Config,
		Agent:     opts.Agent,
	})
}

// RunAttached opens an attached session.
func RunAttached(sessionID string) error {
	return bubble.RunAttached(sessionID)
}
