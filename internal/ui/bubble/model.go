// SPDX-License-Identifier: AGPL-3.0-or-later

package bubble

import (
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/TaraTheStar/enso/internal/backend/host"
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

// egressTickMsg drives the egress-prompt auto-deny countdown — the
// network analogue of permTickMsg. Unlike permission prompts (which only
// auto-deny in attach mode, where the daemon sets a deadline), an egress
// prompt always gets a deadline: a sealed box blocked on a target the
// user never answers would otherwise pin the prompt — and the
// connection — indefinitely. Deny is the safe default here (it blocks
// one connection the model can retry), so a default deadline is pure
// upside.
type egressTickMsg struct{}

// egressPromptDeadline is the default time the interactive TUI gives the
// user to answer an egress prompt before auto-denying, when the broker
// didn't set its own. Matches the permission prompt's 60s feel.
const egressPromptDeadline = 60 * time.Second

func egressTick() tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg {
		return egressTickMsg{}
	})
}

// spinTickMsg drives the contextual live-status spinner above the input.
// While any block is live (reasoning, assistant streaming, tool running)
// the tick fires every spinFrameMs so the spinner glyph advances and the
// "(Ns)" elapsed counter stays current — even when the underlying event
// stream is sparse (e.g. a model that emits one reasoning delta every
// few seconds).
type spinTickMsg struct{}

const spinFrameMs = 150

func spinTick() tea.Cmd {
	return tea.Tick(spinFrameMs*time.Millisecond, func(time.Time) tea.Msg {
		return spinTickMsg{}
	})
}

// model is the Bubble Tea model for the live region. Past blocks live
// in terminal scrollback (graduated via tea.Println on completion) and
// aren't held here. The most recent in-flight block lives in conv.
type model struct {
	inputCh chan<- string

	// cancelTurn aborts the in-flight agent turn (sess.Cancel: ships
	// MsgCancel to the worker, which calls agt.Cancel() and emits
	// bus.EventCancelled). Bound by run.go; nil-safe so tests can
	// construct a bare model. Triggered by double-Esc when the input
	// buffer is empty and a turn is in progress.
	cancelTurn func()

	// Identity for status line. Only the model name is shown by default;
	// provider/base URL/context window live in /info and the Ctrl-Space
	// session inspector overlay. Showing the provider
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

	// overlay holds the data shown by the Ctrl-Space alt-screen session
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

	// pendingNew, set when the /new command fires, tells run.go to
	// re-exec into a fresh session (no --session) after p.Run() returns.
	pendingNew bool

	// palette is the `/`-trigger slash-command palette (U2). Same
	// alt-screen pattern as picker / sessions; opens when `/` is typed
	// on an empty input line and reads the live command registry.
	palette     *slashPaletteData
	paletteOpen bool

	// rewind is the /rewind overlay (per-message checkpoint/undo). Same
	// alt-screen pattern; selecting a turn + restore mode sets
	// m.pendingRewind so run.go applies the restore and re-execs into the
	// session after p.Run() returns.
	rewind        *rewindOverlayData
	rewindOpen    bool
	pendingRewind *pendingRewindReq

	// perm is the in-flight permission prompt, if any. While set, the
	// agent is blocked on req.Respond and we route key input to the
	// inline y/n/a/t resolver instead of the regular input handler.
	perm *permPending

	// egress is the in-flight interactive egress prompt, if any. Same
	// modal-key discipline as perm: the host InteractiveBroker blocks on
	// req.Respond until a y/t/n keystroke resolves it.
	egress *egressPending

	// permCheckerCwd carries the checker + cwd + worker session into
	// permPending when a new request arrives. Set once at construction.
	//
	// checker is the host-side DISPLAY mirror (keeps /perms + /info in
	// sync). sess is the seam to the worker's REAL enforcing checker:
	// "always"/"turn" grants RPC through it (mirroring /yolo) so they
	// actually gate future calls. Both nil only in true attach mode,
	// where resolvePerm degrades a/t to allow-once.
	permCheckerCwd struct {
		checker *permissions.Checker
		cwd     string
		sess    *host.Session
	}

	statusLine string     // single-line status (tool name, etc.); empty when idle
	input      inputState // user-typed input + cursor + vim state

	// liveStarted timestamps when the current live block began. Used by
	// the bottom contextual indicator to render its "(Ns)" elapsed
	// counter. Reset to zero whenever the live block clears or
	// transitions to a different type. Tools and reasoning blocks carry
	// their own start times too, but assistant streaming has none —
	// liveStarted serves uniformly for all three.
	liveStarted time.Time
	// spinning is true while a spinTick is in flight. Prevents stacking
	// duplicate tick chains when multiple events arrive between frames.
	spinning bool

	// busy tracks "the agent has accepted a user message and is working
	// on a turn", but no live block exists yet (or hasn't yet reopened
	// between tool-call rounds). Without this, the gap between Enter
	// and the first delta — common on cold starts, model switches, and
	// slow reasoning models — shows nothing but the model name and
	// looks frozen. Cleared on EventAgentIdle / EventCancelled /
	// EventError.
	busy      bool
	busySince time.Time

	// lastEscAt is the time of the previous Esc keystroke; used to
	// detect a double-Esc within escDoubleWindow that clears the input
	// line. Reset to zero at the top of every handleKey call so any
	// non-Esc key in between breaks the chord.
	lastEscAt time.Time

	// lastCancelAt is the time of the previous Ctrl-C while busy. A
	// second Ctrl-C within escDoubleWindow force-quits even if the
	// turn-cancel path itself is stuck (provider not honouring ctx,
	// daemon RPC wedged). Cleared by any non-Ctrl-C key.
	lastCancelAt time.Time

	// lastQuitAt is the time of the previous *idle* quit keystroke
	// (Ctrl-C while not busy, or Ctrl-D on an empty line). Quitting now
	// requires a confirming second press within quitConfirmWindow so a
	// reflexive tap doesn't discard the session with no warning. Cleared
	// by any key other than Ctrl-C / Ctrl-D.
	lastQuitAt time.Time

	width, height int
	quitting      bool
}

// Lipgloss styles live in styles.go and are resolved from the shared
// theme palette (internal/ui/theme). run.go calls loadAndApplyTheme()
// at startup so the user's $XDG_CONFIG_HOME/enso/theme.toml overrides are picked up.

// Init runs once on program start. Nothing to schedule — bus events
// arrive via forwardBus's program.Send.
func (m *model) Init() tea.Cmd { return nil }

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case tea.KeyPressMsg:
		return m.handleKey(msg)

	case tea.PasteMsg:
		// Terminal bracketed paste — Ctrl-Shift-V, Cmd-V, or
		// middle-click X11 PRIMARY. bubbletea v2 enables bracketed
		// paste by default, so pasted text arrives here (NOT as
		// keystrokes); without this case it was silently dropped, which
		// is why pasting "didn't work". (Plain Ctrl-V is not a terminal
		// paste in raw mode and intentionally does nothing.)
		//
		// Ignore while a modal/prompt owns the keyboard, exactly as
		// typed text is. Preserve newlines verbatim — the input
		// supports multi-line buffers (shift+enter / alt+enter / ctrl+j
		// insert literal \n; render soft-wraps on \n and scrolls up to
		// maxInputLines rows). Only \r\n and bare \r are normalised to
		// \n so the buffer's line breaks are consistent regardless of
		// the source platform's line endings.
		if m.pickerOpen || m.sessionsOpen || m.paletteOpen || m.rewindOpen || m.perm != nil || m.egress != nil || m.overlayOpen {
			return m, nil
		}
		if m.input.vim && m.input.vimNormal {
			return m, nil // normal mode is not a text-entry context
		}
		text := strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(msg.Content)
		if text != "" {
			m.input.insertString(text)
		}
		return m, nil

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

	case spinTickMsg:
		// Reschedule while anything is live OR while we're waiting for
		// the agent to respond, so the spinner glyph and elapsed
		// counter keep advancing; otherwise let the tick chain die so
		// we're not waking up while idle.
		if m.conv.Live() != nil || m.busy {
			return m, spinTick()
		}
		m.spinning = false
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

	case egressTickMsg:
		// No egress prompt pending: drop the tick.
		if m.egress == nil || m.egress.req == nil {
			return m, nil
		}
		if !m.egress.deadline.IsZero() && time.Now().After(m.egress.deadline) {
			// Auto-deny — same path as the user typing 'n'.
			req := m.egress.req
			m.egress = nil
			go func() {
				defer func() { _ = recover() }()
				req.Respond <- permissions.EgressDeny
			}()
			return m, tea.Println(noticeStyle.Render("(egress auto-denied: deadline expired)"))
		}
		return m, egressTick()
	}
	return m, nil
}

// escDoubleWindow is how recently the previous Esc must have arrived
// for the next Esc to count as the second half of a double-Esc clear.
// Tight enough that an accidental double-tap from key chatter doesn't
// wipe a half-typed message; loose enough for a deliberate two-tap.
const escDoubleWindow = 500 * time.Millisecond

// quitConfirmWindow is how long the "press again to quit" arm stays live
// after an idle Ctrl-C / empty-line Ctrl-D. Longer than escDoubleWindow:
// this is a deliberate confirm-to-quit, not an accidental-double-tap
// guard, so the user has time to register the hint and press again.
const quitConfirmWindow = 3 * time.Second

func (m *model) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	// Capture and clear the prior Esc timestamp so any non-Esc key
	// that runs through this handler breaks the double-Esc chord.
	// The Esc case below re-sets it for the *next* call to compare
	// against.
	prevEscAt := m.lastEscAt
	m.lastEscAt = time.Time{}

	// Same chord-break for the Ctrl-C "press again to force quit"
	// affordance: any non-Ctrl-C key resets the timer. The Ctrl-C
	// case below re-sets it.
	prevCancelAt := m.lastCancelAt
	if msg.String() != "ctrl+c" {
		m.lastCancelAt = time.Time{}
	}

	// Chord-break for the idle-quit confirmation (Ctrl-C while not busy /
	// Ctrl-D on an empty line): any key other than those two clears the
	// arm so a stray quit keystroke doesn't stay primed behind unrelated
	// typing. The quit cases below re-set it.
	prevQuitAt := m.lastQuitAt
	switch msg.String() {
	case "ctrl+c", "ctrl+d":
	default:
		m.lastQuitAt = time.Time{}
	}

	// File picker handling: when open, all keys route to the picker.
	if m.pickerOpen {
		return m.handlePickerKey(msg)
	}

	// Recent-sessions picker: same routing pattern.
	if m.sessionsOpen {
		return m.handleSessionsKey(msg)
	}

	// Slash-command palette: same routing pattern.
	if m.paletteOpen {
		return m.handlePaletteKey(msg)
	}

	// /rewind overlay: same routing pattern.
	if m.rewindOpen {
		return m.handleRewindKey(msg)
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

	// Interactive egress prompt: same intercept discipline as perm.
	if m.egress != nil {
		decision, decided := resolveEgress(msg.String())
		if !decided {
			return m, nil
		}
		req := m.egress.req
		// Mirror the perm flow's "remembered" line: when the user
		// scopes the grant to the task ([t]), surface a confirmation
		// so they know the broker will let further calls to the same
		// target through without prompting.
		var cmd tea.Cmd
		if decision == permissions.EgressAllowTask {
			cmd = tea.Println(statusStyle.Render("→ allowing " + req.Target + " for this task"))
		}
		m.egress = nil
		go func() { req.Respond <- decision }()
		return m, cmd
	}

	// Session-inspector overlay handling: Ctrl-Space toggles, Esc
	// dismisses while open. (Ctrl-Space rather than Ctrl-A so the
	// readline-style move-to-start-of-line binding stays free.)
	// Bubble Tea reports the same chord as either "ctrl+space" or
	// "ctrl+@" depending on the terminal's keyboard protocol — handle
	// both. While the overlay is open we swallow other keys so
	// accidental typing doesn't enter the input buffer behind it.
	switch msg.String() {
	case "ctrl+space", "ctrl+@":
		if m.overlayOpen {
			m.overlayOpen = false
			return m, nil
		}
		if m.overlay != nil {
			m.overlayOpen = true
			return m, nil
		}
	case "ctrl+r":
		if m.sessions != nil {
			m.sessions.reset()
			m.sessions.load()
			m.sessionsOpen = true
			return m, nil
		}
	case "esc":
		if m.overlayOpen {
			m.overlayOpen = false
			return m, nil
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
			handled, exitNormal := handleVimNormalKey(&m.input, key, []rune(msg.Text))
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
		// Empty input: quit (with confirmation — see below). Non-empty:
		// clear the line (matches readline).
		if m.input.buf == "" {
			if !prevQuitAt.IsZero() && time.Since(prevQuitAt) < quitConfirmWindow {
				m.quitting = true
				return m, tea.Quit
			}
			m.lastQuitAt = time.Now()
			return m, tea.Println(statusStyle.Render("(press Ctrl-D again to quit)"))
		}
		m.input.reset()
		return m, nil

	case "ctrl+c":
		// First Ctrl-C while busy: cancel the in-flight turn. Second
		// Ctrl-C within the chord window: force-quit, even if the
		// cancel itself hung (provider not honouring ctx, daemon RPC
		// wedged).
		busy := m.busy || m.conv.Live() != nil
		if busy && m.cancelTurn != nil {
			if !prevCancelAt.IsZero() && time.Since(prevCancelAt) < escDoubleWindow {
				m.quitting = true
				return m, tea.Quit
			}
			m.cancelTurn()
			m.lastCancelAt = time.Now()
			return m, tea.Println(statusStyle.Render("(cancelling turn — press Ctrl-C again to force quit)"))
		}
		// Ctrl-C while idle: confirm before quitting so a reflexive tap
		// doesn't silently discard the session. A second Ctrl-C within
		// quitConfirmWindow commits.
		if !prevQuitAt.IsZero() && time.Since(prevQuitAt) < quitConfirmWindow {
			m.quitting = true
			return m, tea.Quit
		}
		m.lastQuitAt = time.Now()
		return m, tea.Println(statusStyle.Render("(press Ctrl-C again to quit)"))

	case "esc":
		// Double-Esc means "undo": on an empty input line while a
		// turn is in flight it cancels the turn; on a non-empty line
		// it wipes the buffer. Single Esc is a no-op in either case
		// (vim mode handles Esc earlier as "enter normal" and never
		// falls through to this switch). A stray single Esc does
		// nothing; a deliberate two-tap is the commit.
		inProgress := m.busy || m.conv.Live() != nil
		if m.input.buf == "" {
			if !inProgress || m.cancelTurn == nil {
				return m, nil
			}
			if !prevEscAt.IsZero() && time.Since(prevEscAt) < escDoubleWindow {
				m.cancelTurn()
				return m, nil
			}
			m.lastEscAt = time.Now()
			return m, nil
		}
		if !prevEscAt.IsZero() && time.Since(prevEscAt) < escDoubleWindow {
			m.input.reset()
			return m, nil
		}
		m.lastEscAt = time.Now()
		return m, nil

	case "shift+enter", "alt+enter", "ctrl+j":
		// Insert a literal newline instead of submitting. shift+enter is
		// the asked-for binding but only reaches us when the terminal
		// supports the Kitty keyboard protocol (bubbletea negotiates it);
		// alt+enter and ctrl+j are reliable fallbacks for terminals that
		// fold shift+enter into a bare enter. The input soft-wraps and
		// scrolls up to maxInputLines rows so multi-line text stays
		// usable.
		m.input.insertString("\n")
		return m, nil

	case "enter":
		text := m.input.trimSpace()
		if text == "" {
			return m, nil
		}
		m.input.reset()

		// Slash commands run in-process; their output goes to scrollback
		// without touching the agent's input channel.
		if strings.HasPrefix(text, "/") && m.slashReg != nil && m.slashCtx != nil {
			cmd := dispatchSlash(m.slashReg, m.slashCtx, text)
			// A command may have switched the active provider (e.g.
			// /model). dispatchSlash runs the handler synchronously, so
			// Provider() already reflects the switch — refresh the
			// status-line model name so the chrome doesn't keep showing
			// the stale startup model.
			if p := m.slashCtx.agt.Provider(); p != nil && p.Model != "" {
				m.modelName = p.Model
			}
			// /rewind signals the model to open its overlay (a command
			// can't mutate the model directly; it sets a flag we read here,
			// mirroring sc.quit).
			if m.slashCtx.openRewind {
				m.slashCtx.openRewind = false
				if m.rewind != nil {
					m.rewind.reset()
					m.rewind.load()
					m.rewindOpen = true
				}
			}
			// /new signals a fresh-session relaunch. Quit so run.go can
			// re-exec after teardown (same exit path as a session switch;
			// alt-screen is left declaratively via m.quitting).
			if m.slashCtx.pendingNew {
				m.slashCtx.pendingNew = false
				m.pendingNew = true
				m.quitting = true
				return m, tea.Quit
			}
			return m, cmd
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

		// Mark the agent busy and ensure the spinner tick is running so
		// the "waiting…" indicator's elapsed counter advances during
		// the gap before the first delta arrives.
		m.busy = true
		m.busySince = time.Now()
		printCmd := tea.Println(renderBlock(ub, m.width, true))
		if !m.spinning {
			m.spinning = true
			return m, tea.Batch(printCmd, spinTick())
		}
		return m, printCmd

	case "backspace":
		m.input.backspace()
		return m, nil

	case "left":
		m.input.left()
		return m, nil
	case "right":
		m.input.right()
		return m, nil
	case "up":
		// Move the cursor up one visual row through the soft-wrapped /
		// multi-line buffer. (Overlays — picker, sessions — intercept
		// up/down earlier, so this only fires for the live input.)
		m.input.up()
		return m, nil
	case "down":
		m.input.down()
		return m, nil
	case "home", "ctrl+a":
		// Readline-style move-to-start-of-line. Ctrl-A is reachable
		// here now that the session-inspector overlay binding moved to
		// Ctrl-Space.
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

	// Append printable text. Bubble Tea v2 delivers the typed runes as
	// msg.Text (a string); control keys arrive as named strings handled
	// above and have an empty Text field.
	if msg.Text != "" {
		// /-trigger: open the slash-command palette when `/` is typed on
		// a completely empty input line (slash commands only run at line
		// start — see the enter handler). The `/` is not inserted; Esc
		// cancels cleanly and Enter inserts `/<name> ` for the pick,
		// mirroring the @ picker's "insert text" mental model.
		if msg.Text == "/" && m.input.buf == "" && m.palette != nil {
			m.palette.reset()
			m.paletteOpen = true
			return m, nil
		}
		// @-trigger: open the file picker if @ would start a new
		// token. Mid-token @s (emails, URLs) fall through to insertion.
		if msg.Text[0] == '@' && m.input.atIsTokenStart() && m.picker != nil {
			m.picker.ensureWalked()
			m.picker.reset()
			m.pickerOpen = true
			return m, nil
		}
		m.input.insertString(msg.Text)
	}
	return m, nil
}

// handleSessionsKey routes keys to the Ctrl-R recent-sessions
// overlay. Up/Down navigate, Enter signals a session switch (run.go
// picks up m.pendingSwitch and syscall.Execs the new session), Esc
// cancels.
func (m *model) handleSessionsKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "ctrl+r":
		m.sessionsOpen = false
		return m, nil
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
			return m, nil
		}
		m.pendingSwitch = m.sessions.sessions[m.sessions.sel].ID
		m.sessionsOpen = false
		m.quitting = true
		// Alt-screen is declarative in v2: m.quitting=true makes View()
		// return an empty non-AltScreen view, so bubbletea exits the
		// alt-screen buffer on its own before tea.Quit takes effect.
		return m, tea.Quit
	}
	return m, nil
}

// handlePickerKey routes keys to the @ file picker overlay. Filter
// typing edits picker.filter, ↑/↓ move the selection, Enter inserts the
// picked path as an `@<path>` mention (with a trailing space), Esc
// cancels.
//
// Lossless trigger: the `@` that opened the picker was deliberately NOT
// inserted into the buffer, so a bare cancel (or an Enter with no match)
// would silently drop the keystroke — and anything the user typed to
// filter. Both exit paths therefore restore `@<filter>` to the input so
// the typed text survives as literal content; the user can edit or
// resubmit it instead of losing it.
func (m *model) handlePickerKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.input.insertString("@" + m.picker.filter)
		m.pickerOpen = false
		return m, nil

	case "enter":
		matches := m.picker.matches()
		if len(matches) == 0 {
			m.input.insertString("@" + m.picker.filter)
			m.pickerOpen = false
			return m, nil
		}
		path := matches[m.picker.sel]
		// Insert as an `@<path>` mention (marker preserved) rather than a
		// bare path, mirroring the slash palette's `/<name>` and making
		// file references visually distinct in the input line.
		m.input.insertString("@" + path + " ")
		m.pickerOpen = false
		return m, nil

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

	if msg.Text != "" {
		m.picker.filter += msg.Text
		m.picker.sel = 0
	}
	return m, nil
}

// handlePaletteKey routes keys to the `/` slash-command palette. Filter
// typing narrows the list, ↑/↓ move the selection, Enter (or Tab, or a
// trailing space) inserts the picked `/<name> ` at the input cursor so
// the user can add args or press Enter again to run, Esc cancels. The
// implied leading `/` is removed on backspace-past-empty, closing the
// palette so a stray `/` never gets stuck.
func (m *model) handlePaletteKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	accept := func() (tea.Model, tea.Cmd) {
		matches := m.palette.matches()
		if len(matches) == 0 || m.palette.sel >= len(matches) {
			m.paletteOpen = false
			return m, nil
		}
		m.input.insertString("/" + matches[m.palette.sel].name + " ")
		m.paletteOpen = false
		return m, nil
	}

	switch msg.String() {
	case "esc":
		m.paletteOpen = false
		return m, nil

	case "enter", "tab":
		return accept()

	case "up", "ctrl+p":
		if m.palette.sel > 0 {
			m.palette.sel--
		}
		return m, nil
	case "down", "ctrl+n":
		matches := m.palette.matches()
		if m.palette.sel < len(matches)-1 {
			m.palette.sel++
		}
		return m, nil

	case "backspace":
		// Backspace past the empty filter removes the implied `/` and
		// dismisses the palette (the symmetric undo of the trigger).
		if n := len(m.palette.filter); n > 0 {
			r := []rune(m.palette.filter)
			m.palette.filter = string(r[:len(r)-1])
			m.palette.sel = 0
			return m, nil
		}
		m.paletteOpen = false
		return m, nil
	}

	// A trailing space is the natural "done naming the command" gesture
	// (no command name contains a space), so accept the selection.
	if msg.Text == " " {
		return accept()
	}
	if msg.Text != "" {
		m.palette.filter += msg.Text
		m.palette.sel = 0
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

	// Turn-terminal events clear the "waiting on agent" flag. Tool-call
	// boundaries don't clear it — between a tool ending and the next
	// LLM call we're still waiting on the agent and the user should
	// still see the "waiting…" indicator.
	switch ev.Type {
	case bus.EventAgentIdle, bus.EventCancelled, bus.EventError:
		m.busy = false
		m.busySince = time.Time{}
		// Also clear any in-flight permission/egress prompts: the
		// agent goroutine that would have read req.Respond is gone, so
		// the View()'s pinned "▸ awaiting…" hint and handleKey's
		// y/n/a/t intercepts would otherwise stay live for a dead
		// resolver. Reply Deny on a background goroutine so a still-
		// living waiter (rare but possible on EventError before the
		// agent fully unwinds) gets a definite answer rather than a
		// closed Respond channel.
		if m.perm != nil && m.perm.req != nil {
			req := m.perm.req
			m.perm = nil
			go func() {
				defer func() { _ = recover() }()
				req.Respond <- permissions.Deny
			}()
		}
		if m.egress != nil && m.egress.req != nil {
			req := m.egress.req
			m.egress = nil
			go func() {
				defer func() { _ = recover() }()
				req.Respond <- permissions.EgressDeny
			}()
		}
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
			// "remember"/"turn" grants reach the worker's enforcing
			// checker via sess (seam path) and mirror onto the host
			// display checker; in true attach mode both are nil and
			// resolvePerm degrades the a/t branches to a "once" allow
			// with notice.
			m.perm = &permPending{
				req:     req,
				checker: m.permCheckerCwd.checker,
				cwd:     m.permCheckerCwd.cwd,
				sess:    m.permCheckerCwd.sess,
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

	// Interactive egress prompt (host InteractiveBroker blocked on a
	// denied target). Same one-at-a-time discipline as permissions.
	if ev.Type == bus.EventEgressRequest {
		req, ok := ev.Payload.(*permissions.EgressPrompt)
		if !ok || req == nil {
			return m, nil
		}
		if m.egress == nil && m.perm == nil {
			dl := req.Deadline
			if dl.IsZero() {
				dl = time.Now().Add(egressPromptDeadline)
			}
			m.egress = &egressPending{req: req, deadline: dl}
			return m, tea.Batch(startEgressPrompt(req), egressTick())
		}
		go func() { req.Respond <- permissions.EgressDeny }()
		return m, nil
	}

	// Cross-turn inline notices for subagent lifecycle. Top-level
	// agent runs (no parent / depth 0) skip the notice — that "agent"
	// is the user's session, surfacing it as a notice would be noise.
	if notice := subagentNotice(ev); notice != "" {
		return m, tea.Println(noticeStyle.Render(notice))
	}

	prevType := liveBlockKind(m.conv.Live())
	graduate := m.conv.HandleEvent(ev)
	nowType := liveBlockKind(m.conv.Live())

	// Transition tracking for the bottom contextual indicator: stamp
	// liveStarted when entering a new live state (or switching kinds),
	// clear it when going back to idle.
	if nowType != prevType {
		if m.conv.Live() != nil {
			m.liveStarted = time.Now()
		} else {
			m.liveStarted = time.Time{}
		}
	}

	// Kick off the per-tool elapsed badge tick when a tool becomes
	// live (the badge inside the tool block, distinct from the bottom
	// indicator's elapsed counter).
	var startTick tea.Cmd
	if t, ok := m.conv.Live().(*blocks.Tool); ok && t.Running() && ev.Type == bus.EventToolCallStart {
		startTick = elapsedTick(t.ID)
	}

	// Start the spinner tick if anything is now live (or we're still
	// waiting on the agent) and no tick is in flight. The tick handler
	// self-terminates when both conditions clear.
	var startSpin tea.Cmd
	if (m.conv.Live() != nil || m.busy) && !m.spinning {
		m.spinning = true
		startSpin = spinTick()
	}

	cmds := []tea.Cmd{}
	if len(graduate) > 0 {
		var lines []string
		for _, b := range graduate {
			if s := renderBlock(b, m.width, true); s != "" {
				lines = append(lines, s)
			}
		}
		if len(lines) > 0 {
			cmds = append(cmds, tea.Println(strings.Join(lines, "\n")))
		}
	}
	if startTick != nil {
		cmds = append(cmds, startTick)
	}
	if startSpin != nil {
		cmds = append(cmds, startSpin)
	}
	switch len(cmds) {
	case 0:
		return m, nil
	case 1:
		return m, cmds[0]
	default:
		return m, tea.Batch(cmds...)
	}
}

// liveBlockKind returns a stable string identifier for the runtime type
// of a live block, or "" for nil. Used to detect transitions between
// live states (reasoning → assistant, tool → idle, etc.) so the bottom
// indicator can reset its elapsed counter at the right moment.
func liveBlockKind(b blocks.Block) string {
	switch b.(type) {
	case nil:
		return ""
	case *blocks.Reasoning:
		return "reasoning"
	case *blocks.Assistant:
		return "assistant"
	case *blocks.Tool:
		return "tool"
	default:
		return "other"
	}
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

// contextIndicator returns a compact live context-window usage badge
// ("ctx 42%") for the status line, already styled, or "" when the data
// isn't available — true attach mode with no agent control, or a
// provider whose context window is unconfigured. Reads cached
// atomics/telemetry (EstimateTokens / ContextWindow), so it's cheap to
// call on every frame. Once usage crosses 80% of the window — where
// auto-compaction starts looming — the badge switches to the brighter
// notice colour so the user gets a heads-up before a compaction lands.
func (m *model) contextIndicator() string {
	if m.slashCtx == nil || m.slashCtx.agt == nil {
		return ""
	}
	window := m.slashCtx.agt.ContextWindow()
	if window <= 0 {
		return ""
	}
	used := m.slashCtx.agt.EstimateTokens()
	style := statusStyle
	if used*100/window >= 80 {
		style = noticeStyle
	}
	return style.Render("ctx " + percentOf(used, window))
}

// costIndicator returns a compact live session-usage badge for the
// status line, already styled, or "" when there's nothing to show.
// Two modes, chosen by whether the active provider carries pricing
// (InputPrice/OutputPrice are dollars per 1M tokens, both zero for a
// local / free model):
//   - priced provider → "$0.0123", cumulative spend this session,
//     derived from the cumulative input/output token counters and the
//     provider's per-million rates (same numbers /cost reports).
//   - free / local provider → "Σ 15k", cumulative tokens this session
//     (input+output) so a heavy local run is still visible even though
//     a dollar figure would be meaningless.
//
// Distinct from contextIndicator: that shows the CURRENT prompt-prefix
// size (what gets re-sent each turn); this shows the session-cumulative
// total (what's been billed so far). Reads cached atomics/telemetry, so
// it's cheap per frame. Returns "" before any tokens are billed (a fresh
// session shows no badge) and in true attach mode with no agent control.
func (m *model) costIndicator() string {
	if m.slashCtx == nil || m.slashCtx.agt == nil {
		return ""
	}
	in := m.slashCtx.agt.CumulativeInputTokens()
	out := m.slashCtx.agt.CumulativeOutputTokens()
	if in <= 0 && out <= 0 {
		return ""
	}
	if prov := m.slashCtx.agt.Provider(); prov != nil && (prov.InputPrice > 0 || prov.OutputPrice > 0) {
		cost := float64(in)/1e6*prov.InputPrice + float64(out)/1e6*prov.OutputPrice
		return statusStyle.Render(fmtCost(cost))
	}
	return statusStyle.Render("Σ " + formatWindow(int(in+out)))
}

// View renders the live region: the in-flight block (if any), a
// single-line status, and the input prompt. Past blocks are NOT
// rendered here — they live in terminal scrollback after tea.Println.
//
// In bubbletea v2, alt-screen mode is declarative: View() returns a
// tea.View struct with AltScreen=true when an overlay is open. The
// runtime handles entering / exiting the alt-screen buffer based on
// frame-to-frame changes to that field; we no longer fire imperative
// EnterAltScreen / ExitAltScreen commands.
func (m *model) View() tea.View {
	if m.quitting {
		return tea.NewView("")
	}
	if m.overlayOpen {
		v := tea.NewView(renderOverlay(m.overlay, m.width, m.height))
		v.AltScreen = true
		return v
	}
	if m.pickerOpen {
		v := tea.NewView(renderPicker(m.picker, m.width, m.height))
		v.AltScreen = true
		return v
	}
	if m.sessionsOpen {
		v := tea.NewView(renderSessionsOverlay(m.sessions, m.width, m.height))
		v.AltScreen = true
		return v
	}
	if m.paletteOpen {
		v := tea.NewView(renderSlashPalette(m.palette, m.width, m.height))
		v.AltScreen = true
		return v
	}
	if m.rewindOpen {
		v := tea.NewView(renderRewindOverlay(m.rewind, m.width, m.height))
		v.AltScreen = true
		return v
	}
	var sb strings.Builder

	if live := m.conv.Live(); live != nil {
		if rendered := renderBlock(live, m.width, false); rendered != "" {
			sb.WriteString(rendered)
			if !strings.HasSuffix(rendered, "\n") {
				sb.WriteByte('\n')
			}
		}
	}

	// Blank line between the live region and the status indicator so
	// streaming output and the status line don't visually fuse.
	sb.WriteByte('\n')

	// Contextual indicator above the input: spinner + label + elapsed
	// while anything is live (thinking, responding, running a tool);
	// "waiting…" when the agent is working but no block is live yet
	// (gap between submit and first delta, or between tool-call rounds);
	// model name when fully idle. statusLine remains as a fallback for
	// any transitional event that set it without a corresponding live
	// block.
	// liveIndicator and waitingIndicator return pre-styled strings (the
	// comet spinner is rendered cell-by-cell in its own colours), so
	// from this point on we don't re-wrap status in statusStyle —
	// fallback paths apply statusStyle inline instead.
	status := liveIndicator(m.conv.Live(), m.liveStarted)
	if status == "" && m.busy {
		status = waitingIndicator(m.busySince)
	}
	if status == "" && m.statusLine != "" {
		status = statusStyle.Render(m.statusLine)
	}
	if status == "" {
		status = statusStyle.Render(m.modelName)
	}
	// Live context-window usage. Steady chrome (unlike the situational
	// hints below): shown whenever the data exists so the user can see
	// the window filling up — and the colour shift past 80% warns before
	// auto-compaction kicks in — instead of having to run /context.
	if ind := m.contextIndicator(); ind != "" {
		status += statusStyle.Render("  · ") + ind
	}
	// Live session cost / cumulative usage. Steady chrome alongside the
	// context badge: ctx shows the current prompt size, this shows the
	// session-cumulative spend (or token total on a free local model) so
	// the user doesn't have to run /cost to watch it climb.
	if ind := m.costIndicator(); ind != "" {
		status += statusStyle.Render("  · ") + ind
	}
	// While a permission or egress prompt is pending, APPEND a pinned
	// reminder so the user always sees that an answer is owed — the
	// full Println'd prompt can scroll off-screen when reasoning streams
	// above it, but this hint sits on the status line which stays
	// anchored above the input box. Appending (not replacing) keeps the
	// live tool spinner / elapsed / model-name base visible so the user
	// retains the existing situational cues while answering.
	if m.perm != nil && m.perm.req != nil {
		status += statusStyle.Render("  · ") + permPendingHint(m.perm.req)
		if !m.perm.req.Deadline.IsZero() {
			remaining := time.Until(m.perm.req.Deadline)
			if remaining > 0 {
				status += statusStyle.Render("  · auto-deny in " + fmtCountdown(remaining))
			}
		}
	} else if m.egress != nil && m.egress.req != nil {
		status += statusStyle.Render("  · ") + egressPendingHint(m.egress.req)
		if !m.egress.deadline.IsZero() {
			if remaining := time.Until(m.egress.deadline); remaining > 0 {
				status += statusStyle.Render("  · auto-deny in " + fmtCountdown(remaining))
			}
		}
	}
	// Surface the interrupt affordance whenever a turn is in flight and
	// the input line is empty — that's exactly when Esc means
	// "interrupt" (on a non-empty line it clears the buffer instead) and
	// turn-cancel is available. A pending permission/egress prompt owns
	// Esc (= deny), so suppress the hint there to avoid contradicting it.
	// Two-stage: the bare "esc to interrupt" invites the first tap; once
	// armed, "press esc again to stop" guides the follow-through. Both
	// clear naturally — the spin tick re-renders every spinFrameMs while
	// busy/live so the chord-window expiry drops the armed string on the
	// next frame, and any non-Esc key zeroes lastEscAt at the top of
	// handleKey, reverting to the bare hint immediately. (Ctrl-C also
	// interrupts on the first press; Esc is the documented one here to
	// keep the line short.)
	if (m.busy || m.conv.Live() != nil) && m.input.buf == "" && m.cancelTurn != nil &&
		m.perm == nil && m.egress == nil {
		if !m.lastEscAt.IsZero() && time.Since(m.lastEscAt) < escDoubleWindow {
			status += statusStyle.Render("  · press esc again to stop")
		} else {
			status += statusStyle.Render("  · esc to interrupt")
		}
	}
	sb.WriteString(status)
	// Blank line between the status indicator and the input prompt so
	// the typing area has a bit of breathing room above it.
	sb.WriteString("\n\n")

	sb.WriteString(m.input.render(m.width))
	return tea.NewView(sb.String())
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
