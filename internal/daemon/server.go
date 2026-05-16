// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !windows

package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/TaraTheStar/enso/internal/agent"
	"github.com/TaraTheStar/enso/internal/bus"
	"github.com/TaraTheStar/enso/internal/config"
	"github.com/TaraTheStar/enso/internal/hooks"
	"github.com/TaraTheStar/enso/internal/llm"
	"github.com/TaraTheStar/enso/internal/mcp"
	"github.com/TaraTheStar/enso/internal/paths"
	"github.com/TaraTheStar/enso/internal/permissions"
	"github.com/TaraTheStar/enso/internal/session"
	"github.com/TaraTheStar/enso/internal/tools"
)

// SocketPath returns the absolute path to the daemon's unix socket. The
// parent dir ($XDG_RUNTIME_DIR/enso) is created on first call so callers
// can bind/listen without separate mkdir plumbing.
func SocketPath() (string, error) {
	dir, err := paths.RuntimeDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	_ = os.Chmod(dir, 0o700)
	return filepath.Join(dir, SocketName), nil
}

// PIDPath returns the absolute path to the daemon's pid lock.
func PIDPath() (string, error) {
	dir, err := paths.RuntimeDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	_ = os.Chmod(dir, 0o700)
	return filepath.Join(dir, PIDFileName), nil
}

// Server runs the daemon: accepts connections, manages session goroutines,
// fans bus events to subscribers.
type Server struct {
	cfg *config.Config
	// provider is the default provider — used for new-session metadata
	// (model/name recorded on the session row).
	provider *llm.Provider
	// providers is the full configured set, built once at startup and
	// handed to every session's agent.New. Because the daemon runs all
	// hosted agent loops in-process, sharing this one map (and thus the
	// one *llm.Pool per pool) is what makes pools coordinate across
	// every daemon session / detached / attached client — no IPC: the
	// shared pointer IS the coordination. Detached/attached clients are
	// pure RPC front-ends that never run an agent, so they need nothing
	// extra. (Separate daemon-less processes still don't coordinate —
	// documented gap; run the daemon if you need cross-process pool
	// limits.) May be nil in tests that construct Server directly;
	// sessionProviders falls back to {provider}.
	providers  map[string]*llm.Provider
	registry   *tools.Registry
	store      *session.Store
	mcpMgr     *mcp.Manager
	listener   net.Listener
	pidPath    string
	socketPath string

	mu           sync.Mutex
	sessions     map[string]*sessionState
	globalAgents *atomic.Int64
}

type sessionState struct {
	id        string
	cwd       string
	yolo      bool
	bus       *bus.Bus
	agent     *agent.Agent
	writer    *session.Writer
	cancel    context.CancelFunc
	createdAt time.Time
	inputCh   chan string
	server    *Server // back-ref so per-session goroutines can read live config

	mu   sync.Mutex
	seq  int64
	ring []Event // capped recent-event ring
	subs []chan Event

	// pendingPerms maps an in-flight permission-request id to the
	// agent's Respond chan. Filled when we forward a request to clients,
	// drained when the client sends back a PermissionResponse (or by
	// timeout / no-subscriber fallback).
	permsMu      sync.Mutex
	pendingPerms map[string]chan permissions.Decision
}

const ringCapacity = 256

// permissionTimeout returns the auto-deny budget for one permission
// request, derived from `[daemon].permission_timeout` (seconds; 0 →
// DefaultPermissionTimeout). Read fresh per-prompt so a config edit +
// daemon SIGHUP-style reload (future) takes effect immediately.
func (s *Server) permissionTimeout() time.Duration {
	secs := config.DefaultPermissionTimeout
	if s.cfg != nil && s.cfg.Daemon.PermissionTimeout > 0 {
		secs = s.cfg.Daemon.PermissionTimeout
	}
	return time.Duration(secs) * time.Second
}

// Run starts the daemon synchronously and blocks until ctx is cancelled.
// Acquires a pid lock; refuses to start if another daemon is already running.
// `explicitConfig` is the optional `-c` override path; "" means rely on the
// usual layered search.
func Run(ctx context.Context, explicitConfig string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("get cwd: %w", err)
	}
	cfg, err := config.Load(cwd, explicitConfig)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	pidPath, err := PIDPath()
	if err != nil {
		return err
	}
	if err := acquirePID(pidPath); err != nil {
		return err
	}
	defer os.Remove(pidPath)

	socketPath, err := SocketPath()
	if err != nil {
		return err
	}
	_ = os.Remove(socketPath) // stale socket from a crashed daemon
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o700); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	// Set umask so the socket is created mode 0600 even before the
	// explicit chmod below — defence-in-depth in case a same-host
	// attacker raced an Accept between Listen and Chmod.
	prevMask := syscall.Umask(0o077)
	listener, err := net.Listen("unix", socketPath)
	syscall.Umask(prevMask)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	if err := os.Chmod(socketPath, 0o600); err != nil {
		_ = listener.Close()
		return fmt.Errorf("chmod socket: %w", err)
	}
	defer listener.Close()
	defer os.Remove(socketPath)

	// Providers (and their pools) are built once here and shared by
	// every session the daemon hosts — that shared *llm.Pool is what
	// coordinates concurrency across all sessions (see Server.providers).
	// Sessions still DEFAULT to `defaultName`; letting a client pick a
	// different default per session over the wire would need a protocol
	// extension and is out of scope, but sub-agents can already route
	// across the full set via spawn_agent's `model:` arg.
	providers, err := llm.BuildProviders(cfg.Providers, cfg.ResolvePools())
	if err != nil {
		return err
	}
	for _, p := range providers {
		p.IncludeProviders = cfg.Instructions.ProvidersIncluded()
	}
	defaultName, err := pickDefaultProviderName(providers, cfg.DefaultProvider)
	if err != nil {
		return err
	}
	provider := providers[defaultName]

	store, err := session.Open()
	if err != nil {
		return fmt.Errorf("open session store: %w", err)
	}
	defer store.Close()

	registry := tools.BuildDefault()
	agent.RegisterSpawn(registry)
	tools.RegisterSearch(registry, cfg.Search)

	mcpMgr := mcp.NewManager()
	if len(cfg.MCP) > 0 {
		mcpMgr.Start(ctx, cfg.MCP)
		mcpMgr.RegisterAll(registry)
	}
	defer mcpMgr.Close()

	// LSP servers are not registered for daemon sessions: each
	// `enso run --detach` can target a different cwd, but the registry
	// is shared across sessions. Per-session LSP would need a multi-
	// manager indirection that's not in v1 scope. Use `enso run` (in-
	// process) when you need the lsp_* tools.

	s := &Server{
		cfg:          cfg,
		provider:     provider,
		providers:    providers,
		registry:     registry,
		store:        store,
		mcpMgr:       mcpMgr,
		listener:     listener,
		pidPath:      pidPath,
		socketPath:   socketPath,
		sessions:     map[string]*sessionState{},
		globalAgents: &atomic.Int64{},
	}

	slog.Info("daemon listening", "socket", socketPath)

	// Accept loop.
	go s.acceptLoop(ctx)

	<-ctx.Done()
	slog.Info("daemon shutting down")

	s.mu.Lock()
	for _, sess := range s.sessions {
		sess.cancel()
	}
	s.mu.Unlock()
	return nil
}

// pickDefaultProviderName mirrors agent.pickDefaultProvider for daemon
// startup. Empty `requested` falls back to the alphabetically-first
// configured key.
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

// acquirePID writes the current PID to pidPath via O_EXCL. If the file
// already exists, checks whether the recorded PID is alive; if not, replaces
// the stale lock.
func acquirePID(pidPath string) error {
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o700); err != nil {
		return fmt.Errorf("mkdir pid: %w", err)
	}
	if data, err := os.ReadFile(pidPath); err == nil {
		if pid, err := strconv.Atoi(string(data)); err == nil && pid > 0 {
			if err := syscall.Kill(pid, 0); err == nil {
				return fmt.Errorf("daemon already running (pid %d, lock %s)", pid, pidPath)
			}
		}
		_ = os.Remove(pidPath)
	}
	f, err := os.OpenFile(pidPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("acquire pid lock: %w", err)
	}
	defer f.Close()
	if _, err := f.WriteString(strconv.Itoa(os.Getpid())); err != nil {
		return fmt.Errorf("write pid: %w", err)
	}
	return nil
}

func (s *Server) acceptLoop(ctx context.Context) {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Warn("daemon accept", "err", err)
			continue
		}
		go s.handleConn(ctx, conn)
	}
}

func (s *Server) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	if err := checkPeer(conn); err != nil {
		slog.Warn("daemon: rejecting connection", "err", err)
		return
	}
	for {
		msg, err := ReadMessage(conn)
		if err != nil {
			return
		}
		if err := s.dispatch(ctx, conn, msg); err != nil {
			_ = WriteMessage(conn, KindError, ErrorResp{Message: err.Error()})
		}
	}
}

func (s *Server) dispatch(ctx context.Context, conn net.Conn, msg Message) error {
	switch msg.Kind {
	case KindListSessions:
		return s.onListSessions(conn)
	case KindCreateSession:
		var req CreateSessionReq
		if err := json.Unmarshal(msg.Body, &req); err != nil {
			return fmt.Errorf("create_session body: %w", err)
		}
		return s.onCreateSession(ctx, conn, req)
	case KindSubmit:
		var req SubmitReq
		if err := json.Unmarshal(msg.Body, &req); err != nil {
			return fmt.Errorf("submit body: %w", err)
		}
		return s.onSubmit(req)
	case KindSubscribe:
		var req SubscribeReq
		if err := json.Unmarshal(msg.Body, &req); err != nil {
			return fmt.Errorf("subscribe body: %w", err)
		}
		return s.onSubscribe(conn, req)
	case KindCancel:
		var req CancelReq
		if err := json.Unmarshal(msg.Body, &req); err != nil {
			return fmt.Errorf("cancel body: %w", err)
		}
		return s.onCancel(req)
	case KindPermissionResponse:
		var req PermissionResponseReq
		if err := json.Unmarshal(msg.Body, &req); err != nil {
			return fmt.Errorf("permission_response body: %w", err)
		}
		return s.onPermissionResponse(req)
	default:
		return fmt.Errorf("unknown kind: %s", msg.Kind)
	}
}

func (s *Server) onListSessions(conn net.Conn) error {
	s.mu.Lock()
	out := make([]SessionInfo, 0, len(s.sessions))
	for _, sess := range s.sessions {
		out = append(out, SessionInfo{
			ID:        sess.id,
			Cwd:       sess.cwd,
			CreatedAt: sess.createdAt,
			Yolo:      sess.yolo,
		})
	}
	s.mu.Unlock()
	return WriteMessage(conn, KindSessionList, SessionList{Sessions: out})
}

// sessionProviders returns the provider map handed to every session's
// agent. It's the one map built at startup, so the *llm.Pool pointers
// are shared across all daemon sessions — that pointer identity is the
// entire cross-session pool-coordination mechanism (no IPC: the daemon
// runs every hosted agent loop in-process). Falls back to a single-entry
// map when only `provider` is set (Server constructed directly in tests).
func (s *Server) sessionProviders() map[string]*llm.Provider {
	if len(s.providers) > 0 {
		return s.providers
	}
	return map[string]*llm.Provider{s.provider.Name: s.provider}
}

func (s *Server) onCreateSession(ctx context.Context, conn net.Conn, req CreateSessionReq) error {
	if req.Cwd == "" {
		req.Cwd = "."
	}

	checker := permissions.NewChecker(s.cfg.Permissions.Allow, s.cfg.Permissions.Ask, s.cfg.Permissions.Deny, s.cfg.Permissions.Mode)
	if req.Yolo {
		checker.SetYolo(true)
	}

	busInst := bus.New()
	writer, err := session.NewSession(s.store, s.provider.Model, s.provider.Name, req.Cwd)
	if err != nil {
		return fmt.Errorf("session: %w", err)
	}
	id := writer.SessionID()

	var restrictedRoots []string
	if !s.cfg.Permissions.DisableFileConfinement {
		restrictedRoots = append([]string{req.Cwd}, s.cfg.Permissions.AdditionalDirectories...)
	}

	agt, err := agent.New(agent.Config{
		Providers:          s.sessionProviders(),
		DefaultProvider:    s.provider.Name,
		Bus:                busInst,
		Registry:           s.registry,
		Perms:              checker,
		Writer:             writer,
		Cwd:                req.Cwd,
		SessionID:          id,
		MaxTurns:           req.MaxTurns,
		GlobalAgents:       s.globalAgents,
		GitAttribution:     s.cfg.Git.Attribution,
		GitAttributionName: s.cfg.Git.AttributionName,
		Hooks:              hooks.New(s.cfg.Hooks.OnFileEdit, s.cfg.Hooks.OnSessionEnd),
		WebFetchAllowHosts: s.cfg.WebFetch.AllowHosts,
		RestrictedRoots:    restrictedRoots,
		PruneCfg:           s.cfg.Context.Resolve(),
	})
	if err != nil {
		return fmt.Errorf("agent: %w", err)
	}

	sessCtx, cancel := context.WithCancel(ctx)
	state := &sessionState{
		id:           id,
		cwd:          req.Cwd,
		yolo:         req.Yolo,
		bus:          busInst,
		agent:        agt,
		writer:       writer,
		cancel:       cancel,
		createdAt:    time.Now(),
		inputCh:      make(chan string, 16),
		pendingPerms: make(map[string]chan permissions.Decision),
		server:       s,
	}

	// Permission proxy: when the agent asks for a decision we generate a
	// request id, stash the Respond chan, fan a wire-format Event of type
	// "PermissionRequest" out to every attached client. The client picks
	// and sends a KindPermissionResponse back which routes via
	// resolvePermission. If no clients are attached we deny immediately
	// — there's nobody to ask. A 60s hard timeout prevents the agent
	// goroutine from hanging if a client disappears mid-prompt.
	permCh := busInst.Subscribe(8)
	go func() {
		for evt := range permCh {
			if evt.Type != bus.EventPermissionRequest {
				continue
			}
			pr, ok := evt.Payload.(*permissions.PromptRequest)
			if !ok {
				continue
			}
			state.proxyPermission(pr, req.Yolo)
		}
	}()

	// Bus → ring + subscriber fan-out.
	relay := busInst.Subscribe(64)
	go func() {
		for evt := range relay {
			state.recordAndFan(evt)
		}
	}()

	go func() {
		_ = agt.Run(sessCtx, state.inputCh)
	}()

	s.mu.Lock()
	s.sessions[id] = state
	s.mu.Unlock()

	state.inputCh <- req.Prompt

	if err := WriteMessage(conn, KindSession, SessionInfo{
		ID:        id,
		Cwd:       req.Cwd,
		CreatedAt: state.createdAt,
		Yolo:      req.Yolo,
	}); err != nil {
		return err
	}
	return nil
}

func (s *Server) onSubmit(req SubmitReq) error {
	state := s.lookup(req.SessionID)
	if state == nil {
		return fmt.Errorf("unknown session %q", req.SessionID)
	}
	state.inputCh <- req.Message
	return nil
}

func (s *Server) onCancel(req CancelReq) error {
	state := s.lookup(req.SessionID)
	if state == nil {
		return fmt.Errorf("unknown session %q", req.SessionID)
	}
	state.agent.Cancel()
	return nil
}

func (s *Server) onSubscribe(conn net.Conn, req SubscribeReq) error {
	state := s.lookup(req.SessionID)
	if state == nil {
		return fmt.Errorf("unknown session %q", req.SessionID)
	}
	ch, replay := state.subscribe(req.FromSeq)
	for _, e := range replay {
		if err := WriteMessage(conn, KindEvent, e); err != nil {
			state.unsubscribe(ch)
			return nil
		}
	}
	for evt := range ch {
		if err := WriteMessage(conn, KindEvent, evt); err != nil {
			state.unsubscribe(ch)
			return nil
		}
	}
	return nil
}

func (s *Server) lookup(id string) *sessionState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[id]
}

func (s *Server) onPermissionResponse(req PermissionResponseReq) error {
	state := s.lookup(req.SessionID)
	if state == nil {
		return fmt.Errorf("unknown session %q", req.SessionID)
	}
	// Fail-closed: anything other than the explicit "allow" string
	// becomes Deny. Earlier wire format used an int that defaulted to
	// Allow on missing/garbled input.
	d := permissions.Deny
	if req.Decision == PermissionAllow {
		d = permissions.Allow
	}
	state.resolvePermission(req.RequestID, d)
	return nil
}

// sessionState methods

// proxyPermission allocates an id for the permission request, stashes the
// agent's Respond chan, fans a wire-format Event to attached subscribers,
// and resolves with Deny on timeout / no-subscriber. yolo callers
// shouldn't reach here in the first place (their checker auto-allows
// before publishing); the parameter is plumbed for symmetry.
func (st *sessionState) proxyPermission(pr *permissions.PromptRequest, yolo bool) {
	if pr == nil || pr.Respond == nil {
		return
	}
	if yolo {
		pr.Respond <- permissions.Allow
		return
	}

	st.mu.Lock()
	hasSubs := len(st.subs) > 0
	st.mu.Unlock()
	if !hasSubs {
		// Nobody's listening — there's no UI to ask. Deny so the agent
		// gets a clear signal rather than hanging.
		pr.Respond <- permissions.Deny
		return
	}

	id := uuid.NewString()
	st.permsMu.Lock()
	st.pendingPerms[id] = pr.Respond
	st.permsMu.Unlock()

	timeout := st.server.permissionTimeout()
	payload, err := json.Marshal(PermissionRequestPayload{
		RequestID: id,
		Tool:      pr.ToolName,
		Args:      pr.Args,
		Diff:      pr.Diff,
		AgentID:   pr.AgentID,
		AgentRole: pr.AgentRole,
		Deadline:  time.Now().Add(timeout),
	})
	if err != nil {
		st.resolvePermission(id, permissions.Deny)
		return
	}

	st.mu.Lock()
	st.seq++
	wireEvt := Event{Seq: st.seq, Type: "PermissionRequest", Payload: payload}
	subs := make([]chan Event, len(st.subs))
	copy(subs, st.subs)
	st.mu.Unlock()

	// Don't add to the ring — replay must not re-trigger an old prompt.
	for _, ch := range subs {
		select {
		case ch <- wireEvt:
		default:
		}
	}

	// Hard timeout so the agent goroutine never hangs on a disconnected
	// or unresponsive client. Computed once above so the wire-deadline
	// the client renders matches what we enforce.
	go func() {
		time.Sleep(timeout)
		st.resolvePermission(id, permissions.Deny)
	}()
}

// resolvePermission delivers `d` on the chan registered for `id` (if any)
// and removes the entry. Calls after the first are no-ops, so the
// timeout-deny and a real client response can race safely.
func (st *sessionState) resolvePermission(id string, d permissions.Decision) {
	st.permsMu.Lock()
	ch, ok := st.pendingPerms[id]
	if ok {
		delete(st.pendingPerms, id)
	}
	st.permsMu.Unlock()
	if !ok {
		return
	}
	select {
	case ch <- d:
	default:
	}
}

func (st *sessionState) recordAndFan(evt bus.Event) {
	wireEvt, ok := toWireEvent(evt)
	if !ok {
		return
	}
	st.mu.Lock()
	st.seq++
	wireEvt.Seq = st.seq
	st.ring = append(st.ring, wireEvt)
	if len(st.ring) > ringCapacity {
		st.ring = st.ring[len(st.ring)-ringCapacity:]
	}
	subs := make([]chan Event, len(st.subs))
	copy(subs, st.subs)
	st.mu.Unlock()

	for _, ch := range subs {
		select {
		case ch <- wireEvt:
		default:
		}
	}
}

func (st *sessionState) subscribe(fromSeq int64) (<-chan Event, []Event) {
	st.mu.Lock()
	defer st.mu.Unlock()
	var replay []Event
	for _, e := range st.ring {
		if e.Seq > fromSeq {
			replay = append(replay, e)
		}
	}
	ch := make(chan Event, 64)
	st.subs = append(st.subs, ch)
	return ch, replay
}

func (st *sessionState) unsubscribe(ch <-chan Event) {
	st.mu.Lock()
	defer st.mu.Unlock()
	for i, c := range st.subs {
		if c == ch {
			st.subs = append(st.subs[:i], st.subs[i+1:]...)
			break
		}
	}
}

// toWireEvent maps a bus event to the wire form. Returns ok=false for
// internal/unserializable events (e.g. PermissionRequest with a chan).
func toWireEvent(evt bus.Event) (Event, bool) {
	var typ string
	switch evt.Type {
	case bus.EventUserMessage:
		typ = "UserMessage"
	case bus.EventAssistantDelta:
		typ = "AssistantDelta"
	case bus.EventAssistantDone:
		typ = "AssistantDone"
	case bus.EventError:
		typ = "Error"
	case bus.EventCancelled:
		typ = "Cancelled"
	case bus.EventToolCallStart:
		typ = "ToolCallStart"
	case bus.EventToolCallProgress:
		typ = "ToolCallProgress"
	case bus.EventToolCallEnd:
		typ = "ToolCallEnd"
	case bus.EventAgentStart:
		typ = "AgentStart"
	case bus.EventAgentEnd:
		typ = "AgentEnd"
	case bus.EventCompacted:
		typ = "Compacted"
	case bus.EventAgentIdle:
		typ = "AgentIdle"
	case bus.EventInputDiscarded:
		typ = "InputDiscarded"
	default:
		return Event{}, false
	}
	payload, err := json.Marshal(simplifyPayload(evt.Payload))
	if err != nil {
		payload = []byte(`null`)
	}
	return Event{Type: typ, Payload: payload}, true
}

// simplifyPayload coerces non-JSON-serializable payloads (errors, channels)
// to safe representations.
func simplifyPayload(p any) any {
	switch v := p.(type) {
	case error:
		return v.Error()
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, val := range v {
			out[k] = simplifyPayload(val)
		}
		return out
	case nil:
		return nil
	default:
		// Strings, ints, floats, bools all marshal fine.
		return v
	}
}

// nullID is just a guard so tests can detect uninitialised state.
var nullID = uuid.NewString()
