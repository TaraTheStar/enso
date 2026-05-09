// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bufio"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/TaraTheStar/enso/internal/daemon"
	"github.com/TaraTheStar/enso/internal/llm"
	"github.com/TaraTheStar/enso/internal/ui"
)

// pickDefaultProviderName mirrors agent.pickDefaultProvider so call
// sites can resolve the configured default before constructing an
// Agent. Empty `requested` falls back to alphabetical-first.
func pickDefaultProviderName(providers map[string]*llm.Provider, requested string) (string, error) {
	if len(providers) == 0 {
		return "", fmt.Errorf("no providers configured")
	}
	if requested != "" {
		if _, ok := providers[requested]; !ok {
			return "", fmt.Errorf("default_provider %q not in [providers]", requested)
		}
		return requested, nil
	}
	names := make([]string, 0, len(providers))
	for name := range providers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names[0], nil
}

// pickAndAttach is the body of `enso attach` with no arguments. It lists
// sessions on the daemon, lets the user pick one by number, and then
// hands off to ui.RunAttached. Newest session is option 1.
func pickAndAttach() error {
	c, err := daemon.Dial()
	if err != nil {
		return fmt.Errorf("daemon not reachable (start with `enso daemon --detach`): %w", err)
	}
	sessions, listErr := c.ListSessions()
	c.Close()
	if listErr != nil {
		return fmt.Errorf("list sessions: %w", listErr)
	}
	if len(sessions) == 0 {
		fmt.Fprintln(os.Stderr, "no sessions on daemon (start one with `enso run --detach \"<prompt>\"`)")
		return nil
	}

	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].CreatedAt.After(sessions[j].CreatedAt)
	})

	fmt.Println("Sessions on daemon:")
	for i, s := range sessions {
		yolo := ""
		if s.Yolo {
			yolo = "  yolo"
		}
		fmt.Printf("  %d. %s  %s  %s%s\n",
			i+1, shortID(s.ID), relTime(s.CreatedAt), s.Cwd, yolo)
	}

	fmt.Printf("Pick [1-%d, q to cancel]: ", len(sessions))
	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil {
		return nil // EOF / Ctrl-D — same as cancel
	}
	line = strings.TrimSpace(line)
	if line == "" || line == "q" || line == "Q" {
		return nil
	}
	idx, err := strconv.Atoi(line)
	if err != nil || idx < 1 || idx > len(sessions) {
		return fmt.Errorf("invalid selection %q", line)
	}
	return ui.RunAttached(sessions[idx-1].ID)
}

func shortID(id string) string {
	if len(id) >= 8 {
		return id[:8]
	}
	return id
}

func relTime(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	default:
		return t.Format("2006-01-02")
	}
}
