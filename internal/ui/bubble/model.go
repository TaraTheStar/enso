// SPDX-License-Identifier: AGPL-3.0-or-later

package bubble

import (
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/TaraTheStar/enso/internal/bus"
	"github.com/TaraTheStar/enso/internal/permissions"
	"github.com/TaraTheStar/enso/internal/slash"
	"github.com/TaraTheStar/enso/internal/ui/blocks"
)

// busEventMsg wraps a bus.Event so it can flow through tea.Update.
type busEventMsg struct{ ev bus.Event }

// elapsedTickMsg fires periodically while a tool call is running so
// the live-region renderBlock can update its "· running 12.3s" badge.
// id pins the tick to a specific Tool block; stale ticks (for tools
// that have since finished) are filtered out in Update.
type elapsedTickMsg struct{ toolID string }

// elapsedTick returns a tea.Cmd that fires elapsedTickMsg after the
// usual 1s cadence — matches the original tui ticker frequency.
func elapsedTick(toolID string) tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg {
		return elapsedTickMsg{toolID: toolID}
	})
}

// permTickMsg fires while a permission prompt with a non-zero
// Deadline is in flight. Each tick: if the deadline has passed, the
// model auto-denies and clears m.perm; otherwise it schedules another
// tick so the live-region "auto-deny in Ns" countdown advances.
type permTickMsg struct{}

// permTick is the same 1s cadence as elapsedTick. A second granularity
// is plenty for a 60s default deadline and avoids burning the cache /
// invalidating selection more often than necessary.
func permTick() tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg {
		return permTickMsg{}
	})
}

// model is the Bubble Tea model for the live region. Past blocks live
// in terminal scrollback (graduated via tea.Println on completion) and
// aren't held here. The most recent in-flight block lives in conv.
type model struct {
	inputCh chan<- string

	// Identity for status line. Only the model name is shown by default;
	// provider/base URL/context window live in /info and the Ctrl-A
	// sidebar overlay (planned for later phases). Showing the provider
	// inline is redundant when the user has named their provider after
	// the model, and load-bearing only when the same model is reachable
	// via multiple providers — that nuance belongs in the on-demand
	// view, not in every-frame chrome.
	modelName string

	// conv tracks the in-flight block; bus events drive HandleEvent and
	// any blocks it returns are graduated to scrollback via tea.Println.
	conv conversation

	// slashReg + slashCtx drive the in-app /command dispatcher. Set by
	// run.go before tea.Program starts.
	slashReg *slash.Registry
	slashCtx *slashCtx

	// overlay holds the data shown by the Ctrl-A alt-screen session
	// inspector. It's an overlay — full-screen alt-screen takeover —
	// not a sidebar; naming reflects that. Set once at construction so
	// the overlay reads from a stable snapshot of the runtime.
	overlay     *overlayData
	overlayOpen bool

	// picker is the @-trigger file picker overlay. Same alt-screen
	// pattern as overlay; only one alt-screen view can be active at a
	// time, so the two are mutually exclusive.
	picker     *pickerData
	pickerOpen bool

	// sessions is the Ctrl-R recent-sessions picker. Same alt-screen
	// pattern as picker / overlay. Selecting a session sets
	// m.pendingSwitch so run.go can syscall.Exec into the new id
	// after p.Run() returns.
	sessions      *sessionsOverlayData
	sessionsOpen  bool
	pendingSwitch string

	// perm is the in-flight permission prompt, if any. While set, the
	// agent is blocked on req.Respond and we route key input to the
	// inline y/n/a/t resolver instead of the regular input handler.
	perm *permPending

	// permCheckerCwd carries the checker + cwd into permPending when a
	// new request arrives. Set once at construction.
	permCheckerCwd struct {
		checker *permissions.Checker
		cwd     string
	}

	statusLine string     // single-line status (tool name, etc.); empty when idle
	input      inputState // user-typed input + cursor + vim state

	width, height int
	quitting      bool
}

// Lipgloss styles live in styles.go and are resolved from the shared
// theme palette (internal/ui/theme). run.go calls loadAndApplyTheme()
// at startup so the user's ~/.enso/theme.toml overrides are picked up.

// Init runs once on program start. Nothing to schedule — bus events
// arrive via forwardBus's program.Send.
func (m *model) Init() tea.Cmd { return nil }

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)

	case busEventMsg:
		return m.handleBusEvent(msg.ev)

	case elapsedTickMsg:
		// If the live block is still the same Tool, re-render (View()
		// runs after Update returns and renderBlock recomputes
		// Elapsed) and schedule another tick. Otherwise the tool has
		// ended; drop the tick.
		if t, ok := m.conv.Live().(*blocks.Tool); ok && t.ID == msg.toolID && t.Running() {
			return m, elapsedTick(msg.toolID)
		}
		return m, nil

	case permTickMsg:
		// No prompt pending or no deadline: drop the tick.
		if m.perm == nil || m.perm.req == nil || m.perm.req.Deadline.IsZero() {
			return m, nil
		}
		if time.Now().After(m.perm.req.Deadline) {
			// Auto-deny — same path as the user typing 'n'.
			req := m.perm.req
			m.perm = nil
			go func() { req.Respond <- permissions.Deny }()
			return m, tea.Println(noticeStyle.Render("(permission auto-denied: deadline expired)"))
		}
		// Still in flight — re-render (View picks up new countdown)
		// and schedule another tick.
		return m, permTick()
	}
	return m, nil
}

func (m *model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// File picker handling: when open, all keys route to the picker.
	if m.pickerOpen {
		return m.handlePickerKey(msg)
	}

	// Recent-sessions picker: same routing pattern.
	if m.sessionsOpen {
		return m.handleSessionsKey(msg)
	}

	// Permission prompt handling: while pending, agent is blocked on
	// req.Respond and we intercept all keys for the y/n/a/t resolver.
	if m.perm != nil {
		decision, decided, cmd := resolvePerm(m.perm, msg.String())
		if !decided {
			// Unrecognised key — leave the prompt active.
			return m, nil
		}
		req := m.perm.req
		m.perm = nil
		// Send the decision in a goroutine so a slow agent reader
		// doesn't pin the tea event loop.
		go func() { req.Respond <- decision }()
		return m, cmd
	}

	// Session-inspector overlay handling: Ctrl-A toggles, Esc dismisses
	// while open. While open we swallow other keys so accidental typing
	// doesn't enter the input buffer behind the overlay.
	switch msg.String() {
	case "ctrl+a":
		if m.overlayOpen {
			m.overlayOpen = false
			return m, tea.ExitAltScreen
		}
		if m.overlay != nil {
			m.overlayOpen = true
			return m, tea.EnterAltScreen
		}
	case "ctrl+r":
		if m.sessions != nil {
			m.sessions.reset()
			m.sessions.load()
			m.sessionsOpen = true
			return m, tea.EnterAltScreen
		}
	case "esc":
		if m.overlayOpen {
			m.overlayOpen = false
			return m, tea.ExitAltScreen
		}
	}
	if m.overlayOpen {
		return m, nil
	}

	key := msg.String()

	// Vim normal-mode handling: in normal mode, intercept keys before
	// they reach the regular insert/edit path. Esc in insert mode flips
	// back to normal. handled=true means the vim layer consumed the key.
	if m.input.vim {
		if !m.input.vimNormal && key == "esc" {
			m.input.vimNormal = true
			return m, nil
		}
		if m.input.vimNormal {
			handled, exitNormal := handleVimNormalKey(&m.input, key, msg.Runes)
			if exitNormal {
				m.input.vimNormal = false
			}
			if handled {
				return m, nil
			}
		}
	}

	switch key {
	case "ctrl+d":
		// Empty input: quit. Non-empty: clear the line (matches readline).
		if m.input.buf == "" {
			m.quitting = true
			return m, tea.Quit
		}
		m.input.reset()
		return m, nil

	case "ctrl+c":
		// Phase 1: Ctrl-C also quits. Later phases distinguish "cancel
		// current turn" from "quit app" the way tui does.
		m.quitting = true
		return m, tea.Quit

	case "enter":
		text := m.input.trimSpace()
		if text == "" {
			return m, nil
		}
		m.input.reset()

		// Slash commands run in-process; their output goes to scrollback
		// without touching the agent's input channel.
		if strings.HasPrefix(text, "/") && m.slashReg != nil && m.slashCtx != nil {
			return m, dispatchSlash(m.slashReg, m.slashCtx, text)
		}

		// Submit to agent. The buffered channel makes this almost-never
		// block; if it does, the agent's input loop is stalled and
		// dropping wouldn't help, so we wait.
		select {
		case m.inputCh <- text:
		default:
			// Highly unlikely — buffered to 64. Surface as inline notice.
			return m, tea.Println(noticeStyle.Render("(input dropped: agent input channel full)"))
		}
		// Echo via the shared block renderer so the user message in
		// scrollback matches the styling used everywhere else. Also
		// append to conversation history so /find and replay see it.
		ub := &blocks.User{Text: text}
		m.conv.Append(ub)
		return m, tea.Println(renderBlock(ub))

	case "backspace":
		m.input.backspace()
		return m, nil

	case "left":
		m.input.left()
		return m, nil
	case "right":
		m.input.right()
		return m, nil
	case "home", "ctrl+a": // ctrl+a is consumed earlier when overlay is closed; this is the readline binding
		// (Note: ctrl+a as overlay toggle takes precedence above; this
		// branch only matches if the overlay binding pre-empt didn't
		// fire — which today it always does. Kept for clarity if the
		// overlay binding ever moves.)
		m.input.home()
		return m, nil
	case "end", "ctrl+e":
		m.input.end()
		return m, nil
	case "ctrl+left":
		m.input.wordBack()
		return m, nil
	case "ctrl+right":
		m.input.wordForward()
		return m, nil
	}

	// Append printable runes. Bubble Tea delivers Runes for ordinary
	// keypresses; control keys arrive as named strings handled above.
	if len(msg.Runes) > 0 {
		// @-trigger: open the file picker if @ would start a new
		// token. Mid-token @s (emails, URLs) fall through to insertion.
		if msg.Runes[0] == '@' && m.input.atIsTokenStart() && m.picker != nil {
			m.picker.ensureWalked()
			m.picker.reset()
			m.pickerOpen = true
			return m, tea.EnterAltScreen
		}
		m.input.insertString(string(msg.Runes))
	}
	return m, nil
}

// handleSessionsKey routes keys to the Ctrl-R recent-sessions
// overlay. Up/Down navigate, Enter signals a session switch (run.go
// picks up m.pendingSwitch and syscall.Execs the new session), Esc
// cancels.
func (m *model) handleSessionsKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "ctrl+r":
		m.sessionsOpen = false
		return m, tea.ExitAltScreen
	case "up", "ctrl+p":
		if m.sessions.sel > 0 {
			m.sessions.sel--
		}
		return m, nil
	case "down", "ctrl+n":
		if m.sessions.sel < len(m.sessions.sessions)-1 {
			m.sessions.sel++
		}
		return m, nil
	case "enter":
		if len(m.sessions.sessions) == 0 {
			m.sessionsOpen = false
			return m, tea.ExitAltScreen
		}
		m.pendingSwitch = m.sessions.sessions[m.sessions.sel].ID
		m.sessionsOpen = false
		m.quitting = true
		// tea.Sequence ensures we exit alt-screen before quitting so
		// the terminal is in a clean state for syscall.Exec.
		return m, tea.Sequence(tea.ExitAltScreen, tea.Quit)
	}
	return m, nil
}

// handlePickerKey routes keys to the @ file picker overlay. Filter
// typing edits picker.filter, ↑/↓ move the selection, Enter inserts
// the picked path at the input cursor (with a trailing space), Esc
// cancels.
func (m *model) handlePickerKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.pickerOpen = false
		return m, tea.ExitAltScreen

	case "enter":
		matches := m.picker.matches()
		if len(matches) == 0 {
			m.pickerOpen = false
			return m, tea.ExitAltScreen
		}
		path := matches[m.picker.sel]
		m.input.insertString(path + " ")
		m.pickerOpen = false
		return m, tea.ExitAltScreen

	case "up", "ctrl+p":
		if m.picker.sel > 0 {
			m.picker.sel--
		}
		return m, nil
	case "down", "ctrl+n":
		matches := m.picker.matches()
		if m.picker.sel < len(matches)-1 {
			m.picker.sel++
		}
		return m, nil

	case "backspace":
		if n := len(m.picker.filter); n > 0 {
			r := []rune(m.picker.filter)
			m.picker.filter = string(r[:len(r)-1])
			m.picker.sel = 0
		}
		return m, nil
	}

	if len(msg.Runes) > 0 {
		m.picker.filter += string(msg.Runes)
		m.picker.sel = 0
	}
	return m, nil
}

// handleBusEvent updates the status line for tool-call lifecycle events,
// emits inline notices for cross-turn annotations (subagent lifecycle),
// and delegates chat-block state mutation to the conversation. Any
// blocks returned by HandleEvent are rendered and emitted to scrollback
// in order via a single tea.Println so they preserve sequence.
func (m *model) handleBusEvent(ev bus.Event) (tea.Model, tea.Cmd) {
	switch ev.Type {
	case bus.EventToolCallStart:
		if d, ok := ev.Payload.(map[string]any); ok {
			if name := getString(d, "name"); name != "" {
				m.statusLine = "running " + name
			}
		}
	case bus.EventToolCallEnd,
		bus.EventAgentIdle,
		bus.EventCancelled,
		bus.EventError:
		m.statusLine = ""
	}

	// Permission prompt: an EventPermissionRequest is the agent
	// blocking on a tool-call decision. Print the inline prompt; the
	// next y/n/a/t keystroke (handled in handleKey) sends the
	// Decision back through req.Respond. If req.Deadline is set
	// (attach mode), kick off a 1s countdown ticker so the user sees
	// the auto-deny clock.
	if ev.Type == bus.EventPermissionRequest {
		req, ok := ev.Payload.(*permissions.PromptRequest)
		if !ok || req == nil {
			return m, nil
		}
		if m.perm == nil {
			// Local mode requires a checker for "remember"/"turn"; in
			// attach mode permCheckerCwd.checker is nil and resolvePerm
			// degrades the a/t branches to a "once" allow with notice.
			m.perm = &permPending{
				req:     req,
				checker: m.permCheckerCwd.checker,
				cwd:     m.permCheckerCwd.cwd,
			}
			print := startPermPrompt(req)
			if !req.Deadline.IsZero() {
				return m, tea.Batch(print, permTick())
			}
			return m, print
		}
		// Defensive: if a prompt is already in flight, deny this one
		// so the agent doesn't wedge.
		go func() { req.Respond <- permissions.Deny }()
		return m, nil
	}

	// Cross-turn inline notices for subagent lifecycle. Top-level
	// agent runs (no parent / depth 0) skip the notice — that "agent"
	// is the user's session, surfacing it as a notice would be noise.
	if notice := subagentNotice(ev); notice != "" {
		return m, tea.Println(noticeStyle.Render(notice))
	}

	graduate := m.conv.HandleEvent(ev)

	// If a Tool block just became live, kick off the elapsed-time
	// ticker so its badge advances while it runs.
	var startTick tea.Cmd
	if t, ok := m.conv.Live().(*blocks.Tool); ok && t.Running() && ev.Type == bus.EventToolCallStart {
		startTick = elapsedTick(t.ID)
	}

	if len(graduate) == 0 {
		return m, startTick
	}
	var lines []string
	for _, b := range graduate {
		if s := renderBlock(b); s != "" {
			lines = append(lines, s)
		}
	}
	if len(lines) == 0 {
		return m, startTick
	}
	printCmd := tea.Println(strings.Join(lines, "\n"))
	if startTick != nil {
		return m, tea.Batch(printCmd, startTick)
	}
	return m, printCmd
}

// subagentNotice returns a single-line inline annotation for nested
// agent lifecycle events, or "" for top-level / unrelated events.
// Format mirrors the plan's examples: ▸ for start, ✓ for clean end,
// ✘ for failed end.
func subagentNotice(ev bus.Event) string {
	d, ok := ev.Payload.(map[string]any)
	if !ok {
		return ""
	}
	parent := getString(d, "parent_id")
	if parent == "" {
		// Top-level run — that's the user's session, not a subagent.
		return ""
	}
	id := getString(d, "id")
	short := id
	if len(short) > 8 {
		short = short[:8]
	}
	switch ev.Type {
	case bus.EventAgentStart:
		return "▸ subagent " + short + " started"
	case bus.EventAgentEnd:
		if errStr := getString(d, "error"); errStr != "" {
			return "✘ subagent " + short + " failed: " + errStr
		}
		return "✓ subagent " + short + " done"
	}
	return ""
}

// View renders the live region: the in-flight block (if any), a
// single-line status, and the input prompt. Past blocks are NOT
// rendered here — they live in terminal scrollback after tea.Println.
//
// When the inspector overlay is open the view switches to a full-
// screen alt-screen render instead. Bubble Tea routes the View output
// to the alt-screen buffer when EnterAltScreen has fired, so the same
// View function serves both modes.
func (m *model) View() string {
	if m.quitting {
		return ""
	}
	if m.overlayOpen {
		return renderOverlay(m.overlay, m.width, m.height)
	}
	if m.pickerOpen {
		return renderPicker(m.picker, m.width, m.height)
	}
	if m.sessionsOpen {
		return renderSessionsOverlay(m.sessions, m.width, m.height)
	}
	var sb strings.Builder

	if live := m.conv.Live(); live != nil {
		if rendered := renderBlock(live); rendered != "" {
			sb.WriteString(rendered)
			if !strings.HasSuffix(rendered, "\n") {
				sb.WriteByte('\n')
			}
		}
	}

	status := m.statusLine
	if status == "" {
		status = m.modelName
	}
	// While a permission prompt with a deadline is pending, surface
	// the countdown next to the model name so the user can see how
	// long they have before auto-deny.
	if m.perm != nil && m.perm.req != nil && !m.perm.req.Deadline.IsZero() {
		remaining := time.Until(m.perm.req.Deadline)
		if remaining > 0 {
			status = status + "  · auto-deny in " + fmtCountdown(remaining)
		}
	}
	sb.WriteString(statusStyle.Render(status))
	sb.WriteByte('\n')

	sb.WriteString(m.input.render())
	return sb.String()
}

// fmtCountdown renders a remaining-time duration as the compact
// "Ns" / "Mm Ss" form the live region wants. Sub-minute values drop
// to whole seconds (the user doesn't need millisecond resolution
// when deciding y/n).
func fmtCountdown(d time.Duration) string {
	secs := int(d.Seconds())
	if secs < 60 {
		return fmtIntS(secs) + "s"
	}
	mins := secs / 60
	rem := secs % 60
	return fmtIntS(mins) + "m" + fmtIntS(rem) + "s"
}

func fmtIntS(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
