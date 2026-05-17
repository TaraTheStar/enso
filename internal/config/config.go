// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/TaraTheStar/enso/internal/paths"
)

// Config is the top-level configuration structure.
type Config struct {
	// DefaultProvider names which entry in Providers is selected at
	// session start. Empty = alphabetical-first. The corresponding
	// `default_provider = "..."` TOML key must appear before any
	// [providers.X] section in the file (TOML scoping).
	DefaultProvider string `toml:"default_provider"`

	Providers    map[string]ProviderConfig `toml:"providers"`
	MCP          map[string]MCPConfig      `toml:"mcp"`
	Permissions  PermConfig                `toml:"permissions"`
	UI           UIConfig                  `toml:"ui"`
	Git          GitConfig                 `toml:"git"`
	LSP          map[string]LSPConfig      `toml:"lsp"`
	Bash         BashConfig                `toml:"bash"`
	Backend      BackendConfig             `toml:"backend"`
	Hooks        HooksConfig               `toml:"hooks"`
	WebFetch     WebFetchConfig            `toml:"web_fetch"`
	Search       SearchConfig              `toml:"search"`
	Daemon       DaemonConfig              `toml:"daemon"`
	Context      ContextPruneConfig        `toml:"context_prune"`
	Instructions InstructionsConfig        `toml:"instructions"`
	Pools        map[string]PoolConfig     `toml:"pools"`
}

// PoolConfig is a [pools.<name>] block: a shared-hardware / rate
// constraint applied across every provider assigned to it. A pool is a
// set of providers sharing a constraint (e.g. one llama-swap behind one
// GPU); each provider belongs to exactly one pool and limits apply
// pool-wide. See ResolvePools for assignment.
type PoolConfig struct {
	// Concurrency is the max in-flight requests across ALL members.
	// < 1 resolves to 1.
	Concurrency int `toml:"concurrency"`
	// QueueTimeout is how long a request waits for a slot before
	// erroring (Go duration string, e.g. "300s"). Empty/invalid →
	// DefaultQueueTimeout.
	QueueTimeout string `toml:"queue_timeout"`
	// SwapCost ("low"/"high"/...) is a hint surfaced to the model in
	// the auto "## Available models" section (step 5). Parsed now;
	// not yet rendered.
	SwapCost string `toml:"swap_cost"`

	// RPM, TPM, DailyBudget are reserved for cloud rate-limit aware
	// scheduling. Parsed so setting them isn't a config error, but NOT
	// enforced in v1 — a one-time warning fires if set (see
	// warnPoolReservedOnce). Reserving the keys now avoids a config
	// migration when enforcement lands.
	RPM         int     `toml:"rpm"`
	TPM         int     `toml:"tpm"`
	DailyBudget float64 `toml:"daily_budget"`
}

// DefaultQueueTimeout is the per-pool wait budget when [pools.X]
// queue_timeout is unset. 300s comfortably covers a local GPU model
// swap plus a cold cloud call without hanging the agent forever on a
// permanently stuck backend.
const DefaultQueueTimeout = 300 * time.Second

// ResolvedPool is the post-defaulting shape llm.BuildProviders consumes
// to construct one shared *llm.Pool per pool.
type ResolvedPool struct {
	Name         string
	Concurrency  int
	QueueTimeout time.Duration
	// SwapCost is the [pools.X] swap_cost hint, "" for auto pools or
	// when unset. Surfaced to the model in the "## Available models"
	// section so it learns swap-cost intuition for routing.
	SwapCost string
}

// PoolResolution is the output of ResolvePools: which pool each
// provider belongs to, plus the resolved settings per pool.
type PoolResolution struct {
	// Assignment maps provider name → pool name.
	Assignment map[string]string
	// Pools maps pool name → resolved settings.
	Pools map[string]ResolvedPool
}

var (
	warnedPoolMu  sync.Mutex
	warnedPoolKey = map[string]struct{}{}
)

// warnPoolReservedOnce logs (once per pool+key, process-wide) that a
// reserved cloud-limit key is parsed but not enforced. Mirrors
// warnEnvOnce so the 3 entry points + sub-agent rebuilds don't spam.
func warnPoolReservedOnce(pool, key string) {
	id := pool + "/" + key
	warnedPoolMu.Lock()
	_, seen := warnedPoolKey[id]
	if !seen {
		warnedPoolKey[id] = struct{}{}
	}
	warnedPoolMu.Unlock()
	if seen {
		return
	}
	slog.Warn("config: [pools] reserved key parsed but not enforced in v1",
		"pool", pool, "key", key)
}

// ResolvePools performs the hybrid pool assignment:
//
//   - A provider with `pool = "X"` joins pool X (explicit override).
//   - Otherwise it auto-groups with every provider sharing its
//     endpoint, under a derived name `auto-<host>-<port>` (one
//     llama-swap = one pool, zero config). An unparseable endpoint
//     falls back to a per-provider pool so it can't accidentally share.
//
// Hybrid because it handles both the common case (one llama-swap = one
// pool, no config) and the edge case (a LiteLLM-style gateway fans one
// endpoint out to several real backends — the user overrides per
// provider with `pool =`). Caller-invoked (not done inside Load),
// mirroring Context.Resolve(): the config struct stays a plain decode
// target and the derived shape is computed on demand.
//
// A pool with exactly one member inherits that provider's
// `concurrency` (preserves pre-pools behaviour for distinct-endpoint
// setups); a multi-member auto pool defaults to concurrency 1
// (serialise shared hardware). A matching [pools.X] block then
// overrides each setting it actually specifies — a block that tunes
// only queue_timeout leaves the inherited concurrency intact. Reserved
// rpm/tpm/daily_budget keys warn once. Deterministic regardless of map
// iteration order.
func (c *Config) ResolvePools() PoolResolution {
	res := PoolResolution{
		Assignment: map[string]string{},
		Pools:      map[string]ResolvedPool{},
	}

	names := make([]string, 0, len(c.Providers))
	for n := range c.Providers {
		names = append(names, n)
	}
	sort.Strings(names)

	members := map[string][]string{} // pool name → provider names
	for _, n := range names {
		pc := c.Providers[n]
		poolName := pc.Pool
		if poolName == "" {
			poolName = autoPoolName(pc.Endpoint, n)
		}
		res.Assignment[n] = poolName
		members[poolName] = append(members[poolName], n)
	}

	poolNames := make([]string, 0, len(members))
	for p := range members {
		poolNames = append(poolNames, p)
	}
	sort.Strings(poolNames)

	for _, p := range poolNames {
		rp := ResolvedPool{Name: p, Concurrency: 1, QueueTimeout: DefaultQueueTimeout}
		// Baseline: a pool with exactly one member inherits that
		// provider's own `concurrency` (preserves pre-pools behaviour
		// for distinct-endpoint setups). This is computed BEFORE the
		// [pools.X] block so a block that tunes only e.g. queue_timeout
		// doesn't silently clamp a lone provider's concurrency to 1 —
		// the block overrides concurrency only when it sets it (>= 1).
		if len(members[p]) == 1 {
			if mc := c.Providers[members[p][0]].Concurrency; mc >= 1 {
				rp.Concurrency = mc
			}
		}
		if pcfg, ok := c.Pools[p]; ok {
			if pcfg.Concurrency >= 1 {
				rp.Concurrency = pcfg.Concurrency
			}
			if d, err := time.ParseDuration(pcfg.QueueTimeout); pcfg.QueueTimeout != "" && err == nil && d > 0 {
				rp.QueueTimeout = d
			}
			rp.SwapCost = pcfg.SwapCost
			if pcfg.RPM != 0 {
				warnPoolReservedOnce(p, "rpm")
			}
			if pcfg.TPM != 0 {
				warnPoolReservedOnce(p, "tpm")
			}
			if pcfg.DailyBudget != 0 {
				warnPoolReservedOnce(p, "daily_budget")
			}
		}
		res.Pools[p] = rp
	}
	return res
}

// autoPoolName derives `auto-<host>-<port>` from a provider endpoint so
// providers behind the same llama-swap/Ollama share a pool with zero
// config. Scheme-less endpoints like "localhost:8080" parse with an
// empty Host (the host lands in Scheme), so we retry them as a network
// reference ("//host:port") — otherwise two providers on the same
// scheme-less endpoint would each get their own pool. Falls back to a
// provider-unique name when the endpoint still can't be parsed
// (defensive: never silently co-pool unrelated endpoints).
func autoPoolName(endpoint, provider string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "auto-" + provider
	}
	if u.Host == "" {
		if u2, err2 := url.Parse("//" + endpoint); err2 == nil && u2.Host != "" {
			u = u2
		}
	}
	if u.Host == "" {
		return "auto-" + provider
	}
	host := u.Hostname()
	port := u.Port()
	if port == "" {
		return "auto-" + host
	}
	return "auto-" + host + "-" + port
}

// InstructionsConfig tunes how the system prompt is assembled.
type InstructionsConfig struct {
	// IncludeProviders controls the auto-rendered "## Available models"
	// section. nil/unset = on whenever ≥2 providers are configured;
	// an explicit `include_providers = false` suppresses it everywhere
	// (including sub-agents). Pointer so "unset" is distinguishable
	// from a deliberate opt-out.
	IncludeProviders *bool `toml:"include_providers"`
}

// ProvidersIncluded resolves the include_providers tri-state: unset
// means on (the auto-section still self-suppresses below 2 providers);
// only an explicit `false` turns it off.
func (ic InstructionsConfig) ProvidersIncluded() bool {
	return ic.IncludeProviders == nil || *ic.IncludeProviders
}

// ContextPruneConfig controls how aggressively old tool-result
// messages are stubbed and how compaction treats designated content.
// Tuned at workload level — the defaults are conservative for typical
// agentic-coding sessions but not all workloads.
//
// Disable by setting `enabled = false`; the agent then falls back to
// pre-pruning behaviour (full retention, compaction at 60% only).
type ContextPruneConfig struct {
	// Enabled gates the entire prune subsystem. Default true. Set
	// false to revert to pre-pruning behaviour (verbatim tool results
	// retained until compaction fires).
	Enabled *bool `toml:"enabled"`

	// StaleAfter is the global default user-turn threshold beyond
	// which a tool result is replaced by a short stub. Per-tool
	// overrides in ToolRetention take precedence. 0 → 5.
	StaleAfter int `toml:"stale_after"`

	// ToolRetention overrides StaleAfter per tool name. Sensible
	// defaults are applied when a tool isn't listed:
	//   read: 8, bash: 3, grep: 2, glob: 2, edit: 1, write: 1
	// Anything else falls through to StaleAfter.
	ToolRetention map[string]int `toml:"tool_retention"`

	// PinnedPaths is matched as a suffix against absolute paths in
	// `read` results — "PLAN.md" matches "/abs/.../PLAN.md" and
	// "/work/PLAN.md" (sandbox path). Reads of pinned paths are not
	// stubbed and survive compaction verbatim.
	PinnedPaths []string `toml:"pinned_paths"`

	// OutputCaps controls per-tool LLMOutput line caps. Zero =
	// in-tree default (2000 for back-compat).
	OutputCaps OutputCapsConfig `toml:"output_caps"`

	// SmartTruncate toggles relevance-based truncation (B2). When
	// true, outputs exceeding the cap try to keep lines matching the
	// most recent user message; falls back to head/tail otherwise.
	// Default false.
	SmartTruncate bool `toml:"smart_truncate"`
}

// OutputCapsConfig holds per-tool LLMOutput line caps.
type OutputCapsConfig struct {
	Default int            `toml:"default"`
	PerTool map[string]int `toml:"-"`
	// Per-named-tool fields are folded into PerTool by Resolve().
	// We keep an explicit field per tool here so the TOML surface
	// reads naturally:
	//   [context_prune.output_caps]
	//   default = 500
	//   bash    = 500
	//   read    = 1000
	Bash     int `toml:"bash"`
	Read     int `toml:"read"`
	Grep     int `toml:"grep"`
	Glob     int `toml:"glob"`
	WebFetch int `toml:"web_fetch"`
}

// Resolve normalises the Config into deterministic defaults the
// agent can hand to AgentContext. Centralising this here keeps the
// agent free of TOML-shaped concerns.
//
// Lookup precedence at use-time (implemented in Agent.staleAfterFor):
//
//  1. user-explicit per-tool override (ToolRetention entry)
//  2. user-explicit global StaleAfter (when > 0)
//  3. in-code per-tool default (read=8, bash=3, grep/glob=2, edit/write=1)
//  4. fallback (5)
//
// Resolve() does NOT pre-mix the in-code per-tool defaults into
// ToolRetention — that would shadow a user-set StaleAfter. The
// in-code defaults live in agent.inCodeDefaultRetention and are
// consulted only when neither the per-tool nor the global override
// is set.
func (c ContextPruneConfig) Resolve() ResolvedPruneConfig {
	out := ResolvedPruneConfig{
		Enabled:           true,
		StaleAfter:        c.StaleAfter,
		PinnedPaths:       c.PinnedPaths,
		SmartTruncate:     c.SmartTruncate,
		ToolRetention:     map[string]int{},
		OutputCapDefault:  2000,
		OutputCapsPerTool: map[string]int{},
	}
	if c.Enabled != nil {
		out.Enabled = *c.Enabled
	}
	for k, v := range c.ToolRetention {
		if v > 0 {
			out.ToolRetention[k] = v
		}
	}
	if c.OutputCaps.Default > 0 {
		out.OutputCapDefault = c.OutputCaps.Default
	}
	if c.OutputCaps.Bash > 0 {
		out.OutputCapsPerTool["bash"] = c.OutputCaps.Bash
	}
	if c.OutputCaps.Read > 0 {
		out.OutputCapsPerTool["read"] = c.OutputCaps.Read
	}
	if c.OutputCaps.Grep > 0 {
		out.OutputCapsPerTool["grep"] = c.OutputCaps.Grep
	}
	if c.OutputCaps.Glob > 0 {
		out.OutputCapsPerTool["glob"] = c.OutputCaps.Glob
	}
	if c.OutputCaps.WebFetch > 0 {
		out.OutputCapsPerTool["web_fetch"] = c.OutputCaps.WebFetch
	}
	for k, v := range c.OutputCaps.PerTool {
		if v > 0 {
			out.OutputCapsPerTool[k] = v
		}
	}
	return out
}

// ResolvedPruneConfig is the post-defaulting shape the agent uses.
type ResolvedPruneConfig struct {
	Enabled           bool
	StaleAfter        int
	ToolRetention     map[string]int
	PinnedPaths       []string
	SmartTruncate     bool
	OutputCapDefault  int
	OutputCapsPerTool map[string]int
}

// DaemonConfig holds settings that only apply when running enso under
// the long-lived daemon (`enso daemon` + `enso attach`). Standalone
// runs ignore this section entirely.
type DaemonConfig struct {
	// PermissionTimeout caps how long the daemon waits for a client
	// decision on a permission request before auto-denying. Seconds;
	// 0 → 60. Setting this above ~5 minutes is reasonable if you walk
	// away from terminals; very small values will surprise users by
	// auto-denying mid-thought.
	PermissionTimeout int `toml:"permission_timeout"`
}

// DefaultPermissionTimeout is the auto-deny budget when the user
// hasn't customised [daemon].permission_timeout. Exposed as a constant
// so the daemon and the TUI countdown can both reference one source.
const DefaultPermissionTimeout = 60

// WebFetchConfig controls the web_fetch tool's SSRF guard. By default the
// tool refuses any URL that resolves to a loopback / private / link-local
// address — that blocks the agent from probing instance-metadata
// (169.254.169.254), the user's running dev servers, or LAN hosts. Add
// entries to AllowHosts to opt specific local services back in (e.g. a
// llama.cpp server on `localhost:8080` or `127.0.0.1:11434`).
//
// Each entry is matched against the URL's `host` or `host:port` exactly
// (case-insensitive on host). An entry without a port matches any port
// for that host; with a port, the port must match. The DNS-rebind defence
// stays on regardless: the resolved IP is pinned for the actual TCP dial.
type WebFetchConfig struct {
	AllowHosts []string `toml:"allow_hosts"`
}

// SearchConfig configures the web_search tool. Provider selects which
// backend implementation is used; today the choices are "searxng" (point
// at a self-hosted instance for higher-quality, multi-engine results),
// "duckduckgo" / "ddg" (no signup; scrapes html.duckduckgo.com), or
// "none" (suppress the tool entirely). When unset, web_search defaults
// to SearXNG if SearXNG.Endpoint is configured, otherwise DuckDuckGo.
//
// Adding a new backend = a new file in internal/tools implementing
// SearchBackend, plus a case in tools.pickSearchBackend.
type SearchConfig struct {
	Provider string        `toml:"provider"`
	SearXNG  SearXNGConfig `toml:"searxng"`
}

// SearXNGConfig configures the SearXNG backend.
type SearXNGConfig struct {
	Endpoint           string   `toml:"endpoint"`             // e.g. "http://localhost:8888"
	Categories         []string `toml:"categories"`           // optional, e.g. ["general","it"]
	Engines            []string `toml:"engines"`              // optional, e.g. ["google","duckduckgo"]
	MaxResults         int      `toml:"max_results"`          // capped count; 0 → 10
	APIKey             string   `toml:"api_key"`              // forwarded as Authorization: Bearer; ENSO_-prefixed env-var refs expanded
	Timeout            int      `toml:"timeout"`              // seconds; 0 → 15
	CACert             string   `toml:"ca_cert"`              // path to PEM bundle to trust (self-hosted CA); appended to system roots
	InsecureSkipVerify bool     `toml:"insecure_skip_verify"` // disables TLS verification entirely — only for ad-hoc self-signed setups
}

// HooksConfig holds the two supported lifecycle commands. Empty
// strings disable the corresponding hook. Templates use Go's
// text/template syntax against vars documented in internal/hooks.
//
//	[hooks]
//	on_file_edit   = "gofmt -w {{.Path}}"
//	on_session_end = "notify-send 'enso done'"
type HooksConfig struct {
	OnFileEdit   string `toml:"on_file_edit"`
	OnSessionEnd string `toml:"on_session_end"`
}

// ProviderConfig holds settings for a single LLM endpoint.
type ProviderConfig struct {
	// Endpoint + APIKey are worker:"deny" — inference is host-proxied,
	// so the worker never dials a model and must never receive either
	// (the credential-scrub invariant, enforced by ScrubbedForWorker).
	Endpoint string `toml:"endpoint" worker:"deny"`
	Model    string `toml:"model"`
	// Description is a short capability hint ("deep reasoning, hard
	// SWE") surfaced in the auto-rendered "## Available models" prompt
	// section so the model can route across endpoints. Optional.
	Description   string `toml:"description"`
	ContextWindow int    `toml:"context_window"`
	Concurrency   int    `toml:"concurrency"`
	// Pool overrides which [pools.X] this provider belongs to. Empty =
	// auto-grouped with every other provider sharing its Endpoint
	// (one llama-swap = one pool, zero config). See ResolvePools.
	Pool    string        `toml:"pool"`
	APIKey  string        `toml:"api_key" worker:"deny"`
	Sampler SamplerConfig `toml:"sampler"`

	// InputPricePerMillion and OutputPricePerMillion are dollars per
	// 1M tokens for the cumulative-spend line in the sidebar. Both
	// zero (or omitted) means "free / local model" and the cost
	// segment hides — that's the right default for llama.cpp,
	// ollama, and other self-hosted endpoints. A typical paid setup
	// looks like:
	//
	//   [providers.openai]
	//   model = "gpt-4o"
	//   input_price_per_million  = 2.50
	//   output_price_per_million = 10.00
	InputPricePerMillion  float64 `toml:"input_price_per_million"`
	OutputPricePerMillion float64 `toml:"output_price_per_million"`
}

// SamplerConfig holds generation parameters.
type SamplerConfig struct {
	Temperature     float64 `toml:"temperature"`
	TopK            int     `toml:"top_k"`
	TopP            float64 `toml:"top_p"`
	MinP            float64 `toml:"min_p"`
	PresencePenalty float64 `toml:"presence_penalty"`
}

// MCPConfig holds settings for an MCP server. `command` selects stdio
// transport; `url` selects HTTP (Streamable-HTTP, falling back to SSE).
// `headers` is HTTP-only and applies the same key/value pairs to every
// request — typical use is `Authorization = "Bearer $TOKEN"`. Both
// `args` values and `headers` values get `$VAR` expansion against the
// process env at startup; `${VAR}` works too.
type MCPConfig struct {
	Command string            `toml:"command"`
	Args    []string          `toml:"args"`
	URL     string            `toml:"url"`
	Headers map[string]string `toml:"headers"`
}

// PermConfig holds permission settings. Three rule lists evaluate in
// precedence order deny → ask → allow; each pattern looks like
// `tool(arg-pattern)` (e.g. `bash(git *)`, `edit(./src/**)`,
// `web_fetch(domain:example.com)`). Ask rules force a prompt even when
// the call would otherwise be auto-allowed; useful for blast-radius
// commands like `bash(git push *)`.
type PermConfig struct {
	Mode                  string   `toml:"mode"`
	Allow                 []string `toml:"allow"`
	Ask                   []string `toml:"ask"`
	Deny                  []string `toml:"deny"`
	AdditionalDirectories []string `toml:"additional_directories"`

	// DisableFileConfinement, when true, lets file-touching tools
	// (read/write/edit/grep/glob/lsp_*) accept any absolute path. By
	// default the agent is confined to `cwd + AdditionalDirectories`
	// regardless of bash sandbox setting. Set this only if you want the
	// model to roam outside its workspace.
	DisableFileConfinement bool `toml:"disable_file_confinement"`
}

// UIConfig holds TUI settings.
type UIConfig struct {
	Theme      string `toml:"theme"`
	EditorMode string `toml:"editor_mode"` // "default" | "vim"
	// StatusLine, when non-empty, replaces the default right-side status
	// segment. text/template syntax. Variables: .Provider .Model
	// .Session .Mode .Activity .Tokens .Window .TokensFmt.
	// Default if empty: "[{{.Provider}}] {{.Model}} · {{.Session}} · {{.TokensFmt}}".
	StatusLine string `toml:"status_line"`
}

// GitConfig controls how the agent attributes itself when making git commits
// on the user's behalf. Attribution is empty / "none" by default — opt in
// per project by setting `[git] attribution = "co-authored-by"`.
type GitConfig struct {
	// Attribution: "co-authored-by" | "assisted-by" | "none" (or "").
	Attribution string `toml:"attribution"`
	// AttributionName is the name to use in the trailer; defaults to "enso".
	AttributionName string `toml:"attribution_name"`
}

// BashConfig controls how the bash tool runs. The default (sandbox =
// "off") executes commands directly on the host; setting sandbox to
// "auto"/"podman"/"docker" runs them inside a per-project container so
// the agent's shell can't escape the project directory.
type BashConfig struct {
	// Sandbox: "off" (default), "auto" (podman, fallback docker),
	// "podman", or "docker".
	Sandbox string             `toml:"sandbox"`
	Sb      BashSandboxOptions `toml:"sandbox_options"`
}

// BashSandboxOptions are the per-project container settings.
type BashSandboxOptions struct {
	// Image to run; defaults to "alpine:latest". Pick whatever
	// language toolchain your project needs (e.g. "golang:1.22").
	Image string `toml:"image"`
	// Init runs once after container creation. Re-runs only when this
	// list (or image / mounts / env) change — tracked via a label on
	// the container. Each line is a `sh -c` invocation.
	Init []string `toml:"init"`
	// Network passes through to the runtime's --network flag. Empty
	// uses the runtime's default. Use "none" for fully offline; a
	// named network for podman pods; "host" to share the host net.
	Network string `toml:"network"`
	// ExtraMounts are additional `-v src:dst[:opts]` entries. The
	// project cwd is always mounted at workdir_mount.
	ExtraMounts []string `toml:"extra_mounts"`
	// Env injects KEY=value pairs into the container.
	Env []string `toml:"env"`
	// Name overrides the auto-generated container name. Empty leaves
	// it as `enso-<basename>-<6-hex>`.
	Name string `toml:"name"`
	// WorkdirMount is the path inside the container where the
	// project cwd is mounted. Defaults to `/work`.
	WorkdirMount string `toml:"workdir_mount"`
	// UID is the `--user` value (e.g. "1000:1000"). Empty defaults to
	// the runtime's normal behaviour: rootless podman remaps the
	// host user automatically; docker runs as root inside the
	// container, which can leave root-owned files in the bind mount.
	UID string `toml:"uid"`

	// Workspace = "overlay" makes the project mount a throwaway podman
	// overlay: the agent sees the real tree but every write lands in
	// an ephemeral upper layer discarded at task end (automatic
	// one-command rollback). Empty/"direct" = writes hit
	// the project in place (today's behaviour).
	Workspace string `toml:"workspace"`

	// Egress is the per-task outbound allowlist ("host:port", or bare
	// "host" for :80+:443). A network-sealed worker reaches ONLY these,
	// via the host allowlist proxy, and only after the tier-3 broker
	// grants the matching capability. Empty = fully sealed.
	Egress []string `toml:"egress"`

	// Credentials maps a logical secret name the worker may broker
	// (CapCredential) to its value; ENSO_-prefixed env refs are
	// expanded like provider api_key. The worker env is otherwise
	// empty — this is the only way a secret reaches a sealed box.
	Credentials map[string]string `toml:"credentials"`

	// Hardening selects a hardened OCI runtime for the container.
	// "gvisor" (alias "runsc") runs it under gVisor — syscalls are
	// intercepted by a userspace kernel, shrinking the host kernel
	// attack surface, at a syscall-heavy performance cost. Linux only;
	// requires runsc installed and configured as a podman runtime. If
	// set but unavailable enso REFUSES to run rather than silently
	// dropping to unhardened isolation. Empty = the default runtime.
	Hardening string `toml:"hardening"`
}

// OCIRuntime maps the user-facing Hardening knob to the `--runtime`
// value podman expects. "" stays empty (default runtime); the gvisor
// alias resolves to runsc; any other non-empty value is passed through
// verbatim so an unknown/misconfigured choice fails the availability
// check loudly instead of silently running unhardened.
func (o BashSandboxOptions) OCIRuntime() string {
	switch strings.ToLower(strings.TrimSpace(o.Hardening)) {
	case "":
		return ""
	case "gvisor", "runsc":
		return "runsc"
	default:
		return strings.TrimSpace(o.Hardening)
	}
}

// BackendKind selects where the agent core runs. There is exactly one
// execution path: the core always runs as a Worker behind a Backend.
type BackendKind string

const (
	// BackendLocal runs the Worker as a host child process: no
	// container, no overlay, full host env — today's behavior. The
	// default.
	BackendLocal BackendKind = "local"
	// BackendPodman runs the Worker as PID 1 of a rootless podman
	// container: overlay workspace, network-sealed, host-proxied
	// inference.
	BackendPodman BackendKind = "podman"
)

// BackendConfig selects the execution backend. `type` is optional and
// layered like every other config key, so a project can override the
// user/global default. Empty `type` is not an error: ResolveBackend
// derives the kind from the existing [bash] sandbox setting so old
// config files keep working unchanged.
type BackendConfig struct {
	// Type: "local" (default) or "podman". Empty = derive from
	// [bash] sandbox (off → local; auto/podman/docker → podman).
	Type string `toml:"type"`
}

// ResolveBackend returns the selected BackendKind. An explicit
// [backend] type wins; otherwise the kind is derived from the existing
// [bash] sandbox knob so this is purely additive and behavior-
// preserving — no existing config file changes meaning.
func (c *Config) ResolveBackend() BackendKind {
	switch strings.ToLower(strings.TrimSpace(c.Backend.Type)) {
	case string(BackendLocal):
		return BackendLocal
	case string(BackendPodman):
		return BackendPodman
	case "":
		// Derive from the legacy bash.sandbox selector.
		switch strings.ToLower(strings.TrimSpace(c.Bash.Sandbox)) {
		case "", "off":
			return BackendLocal
		default: // "auto", "podman", "docker"
			return BackendPodman
		}
	default:
		// Unknown explicit value: fail safe to the no-isolation
		// default rather than silently picking a container.
		return BackendLocal
	}
}

// ScrubbedForWorker returns a deep copy with every `worker:"deny"`
// field zeroed (see scrub.go for what that tag means and why it is
// scoped to the host-proxied provider credentials, not all secrets).
// It is the enforcement point for the credential-scrub invariant: the
// host serializes THIS — never the raw config — into
// TaskSpec.ResolvedConfig, so a provider endpoint/key cannot cross the
// Backend seam. The worker rebuilds providers from the non-secret
// catalog (TaskSpec.Providers) plus the host-proxied inference client.
//
// The deep copy is a JSON round-trip: Config is already required to be
// JSON-(de)serializable for TaskSpec.ResolvedConfig, so this cannot
// introduce a shape the worker can't read, and it guarantees the
// reflection scrub never mutates the caller's live config (maps and
// slices included). A marshal failure falls back to the explicit
// provider scrub so the invariant holds even then.
func (c *Config) ScrubbedForWorker() *Config {
	raw, err := json.Marshal(c)
	if err == nil {
		cp := &Config{}
		if json.Unmarshal(raw, cp) == nil {
			scrubSecrets(reflect.ValueOf(cp))
			return cp
		}
	}
	// Defensive fallback: never return an unscrubbed config.
	cp := *c
	if c.Providers != nil {
		cp.Providers = make(map[string]ProviderConfig, len(c.Providers))
		for k, p := range c.Providers {
			p.Endpoint = ""
			p.APIKey = ""
			cp.Providers[k] = p
		}
	}
	return &cp
}

// LSPConfig declares one language-server entry. The TOML key
// (`[lsp.<name>]`) is the human-readable label; the actual mapping is
// controlled by `extensions`. All fields except Command are optional;
// reasonable LSP servers run with defaults.
type LSPConfig struct {
	// Command is the executable to spawn (e.g. "gopls", "rust-analyzer",
	// "typescript-language-server"). Required.
	Command string `toml:"command"`
	// Args are extra CLI args after Command. Many servers need a flag
	// like "--stdio" to enable JSON-RPC mode.
	Args []string `toml:"args"`
	// Extensions decides routing — files whose path ends with one of
	// these (case-insensitive) get sent to this server. Include the
	// leading dot, e.g. `[".go"]`.
	Extensions []string `toml:"extensions"`
	// RootMarkers is a list of filenames the manager walks up from the
	// target file to find the project root. First match wins. If none
	// match, the cwd is used. Examples: ["go.mod", ".git"].
	RootMarkers []string `toml:"root_markers"`
	// InitOptions is passed verbatim as `initializationOptions` in the
	// LSP `initialize` request. Use a TOML inline-table or sub-section
	// to populate. Server-specific.
	InitOptions map[string]any `toml:"init_options"`
	// Env is extra environment variables for the server process, in
	// `KEY=value` format. Inherits the parent enso process env.
	Env []string `toml:"env"`
	// LanguageID is the LSP `languageId` to send on `didOpen`. Defaults
	// to the config block name (`<name>` in `[lsp.<name>]`).
	LanguageID string `toml:"language_id"`
}

// SearchPaths returns the layered config files in priority order (lowest →
// highest). `explicit`, when non-empty, becomes the final, highest-priority
// layer. Files that don't exist are skipped silently. The order is:
//
//  1. /etc/enso/config.toml                       (system)
//  2. $XDG_CONFIG_HOME/enso/config.toml           (user; defaults to ~/.config/enso/config.toml)
//  3. <cwd>/.enso/config.toml                     (project, committed)
//  4. <cwd>/.enso/config.local.toml               (project, gitignored — "Allow + Remember" writes here)
//  5. <explicit>                                  (-c flag, if set)
func SearchPaths(cwd, explicit string) []string {
	paths := []string{"/etc/enso/config.toml"}
	if user, err := UserConfigPath(); err == nil {
		paths = append(paths, user)
	}
	if cwd != "" {
		paths = append(paths, filepath.Join(cwd, ".enso", "config.toml"))
		paths = append(paths, filepath.Join(cwd, ".enso", "config.local.toml"))
	}
	if explicit != "" {
		paths = append(paths, explicit)
	}
	return paths
}

// ProjectLocalPath returns <cwd>/.enso/config.local.toml. This is where
// "Allow + Remember" rules accumulate so they don't pollute the shared
// project config.
func ProjectLocalPath(cwd string) string {
	if cwd == "" {
		return ""
	}
	return filepath.Join(cwd, ".enso", "config.local.toml")
}

// AppendAllow loads the TOML file at `path`, appends `pattern` to
// `permissions.allow` (deduping), and writes it back. Other top-level
// sections are preserved. The file (and parent directory) are created if
// they don't exist. No-op if the pattern is already present.
func AppendAllow(path, pattern string) error {
	if path == "" {
		return fmt.Errorf("AppendAllow: empty path")
	}
	root := map[string]any{}
	if data, err := os.ReadFile(path); err == nil {
		if err := toml.Unmarshal(data, &root); err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read %s: %w", path, err)
	}

	perms, _ := root["permissions"].(map[string]any)
	if perms == nil {
		perms = map[string]any{}
		root["permissions"] = perms
	}

	allow, _ := perms["allow"].([]any)
	for _, e := range allow {
		if s, _ := e.(string); s == pattern {
			return nil // already present
		}
	}
	allow = append(allow, pattern)
	perms["allow"] = allow

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	out, err := toml.Marshal(root)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	return os.WriteFile(path, out, 0o600)
}

// LoadRules reads only the permissions allow/ask/deny lists from a
// single TOML file. Used by the /permissions overlay, which scopes
// itself to <cwd>/.enso/config.local.toml so deletions are obviously
// project-local. A non-existent file returns three empty slices and
// no error — fresh projects have nothing to list yet.
func LoadRules(path string) (allow, ask, deny []string, err error) {
	if path == "" {
		return nil, nil, nil, fmt.Errorf("LoadRules: empty path")
	}
	data, readErr := os.ReadFile(path)
	if errors.Is(readErr, os.ErrNotExist) {
		return nil, nil, nil, nil
	}
	if readErr != nil {
		return nil, nil, nil, fmt.Errorf("read %s: %w", path, readErr)
	}
	var root map[string]any
	if err := toml.Unmarshal(data, &root); err != nil {
		return nil, nil, nil, fmt.Errorf("parse %s: %w", path, err)
	}
	perms, _ := root["permissions"].(map[string]any)
	if perms == nil {
		return nil, nil, nil, nil
	}
	collect := func(key string) []string {
		raw, _ := perms[key].([]any)
		out := make([]string, 0, len(raw))
		for _, e := range raw {
			if s, _ := e.(string); s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return collect("allow"), collect("ask"), collect("deny"), nil
}

// RemoveRule loads the TOML file at `path`, drops the first occurrence
// of `pattern` from `permissions.<kind>` (where kind is "allow", "ask",
// or "deny"), and writes the file back. Other top-level sections are
// preserved. Returns (true, nil) if the rule was found and removed,
// (false, nil) if the file or the rule wasn't there. Used by the
// /permissions overlay.
func RemoveRule(path, kind, pattern string) (bool, error) {
	if path == "" {
		return false, fmt.Errorf("RemoveRule: empty path")
	}
	switch kind {
	case "allow", "ask", "deny":
	default:
		return false, fmt.Errorf("RemoveRule: invalid kind %q", kind)
	}
	data, readErr := os.ReadFile(path)
	if errors.Is(readErr, os.ErrNotExist) {
		return false, nil
	}
	if readErr != nil {
		return false, fmt.Errorf("read %s: %w", path, readErr)
	}
	root := map[string]any{}
	if err := toml.Unmarshal(data, &root); err != nil {
		return false, fmt.Errorf("parse %s: %w", path, err)
	}
	perms, _ := root["permissions"].(map[string]any)
	if perms == nil {
		return false, nil
	}
	list, _ := perms[kind].([]any)
	idx := -1
	for i, e := range list {
		if s, _ := e.(string); s == pattern {
			idx = i
			break
		}
	}
	if idx < 0 {
		return false, nil
	}
	list = append(list[:idx], list[idx+1:]...)
	if len(list) == 0 {
		delete(perms, kind)
	} else {
		perms[kind] = list
	}
	out, err := toml.Marshal(root)
	if err != nil {
		return false, fmt.Errorf("marshal: %w", err)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		return false, fmt.Errorf("write %s: %w", path, err)
	}
	return true, nil
}

// UserConfigPath returns $XDG_CONFIG_HOME/enso/config.toml, falling back to
// ~/.config/enso/config.toml.
func UserConfigPath() (string, error) {
	dir, err := paths.ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.toml"), nil
}

// DefaultTOML returns the embedded default config as a string. Used by
// `enso config init --print` to show or pipe the template.
func DefaultTOML() string { return defaultTOML }

// Load reads and merges every config file in SearchPaths(cwd, explicit). If
// no file exists and `explicit` was empty, a default config is written to the
// user config path and then loaded. If `explicit` was supplied but the file
// doesn't exist, that's an error.
func Load(cwd, explicit string) (*Config, error) {
	cfg, _, err := LoadWithFirstRun(cwd, explicit)
	return cfg, err
}

// LoadWithFirstRun is Load plus a `freshlyWritten` flag set true when this
// invocation just created the user config (i.e. no config existed anywhere
// on the search path). The CLI uses this to gate a one-shot welcome message
// so the user knows where their config is and how to point it at a real
// provider.
func LoadWithFirstRun(cwd, explicit string) (*Config, bool, error) {
	paths := SearchPaths(cwd, explicit)

	merged := map[string]any{}
	foundAny := false
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				if p == explicit {
					return nil, false, fmt.Errorf("config %s: %w", p, err)
				}
				continue
			}
			return nil, false, fmt.Errorf("read %s: %w", p, err)
		}
		foundAny = true
		var layer map[string]any
		if err := toml.Unmarshal(data, &layer); err != nil {
			return nil, false, fmt.Errorf("parse %s: %w", p, err)
		}
		mergeMaps(merged, layer)
	}

	freshlyWritten := false
	if !foundAny {
		if explicit != "" {
			// Should be unreachable given the per-file check above, but be
			// defensive about future refactors.
			return nil, false, fmt.Errorf("config %s not found", explicit)
		}
		path, err := writeDefault()
		if err != nil {
			return nil, false, err
		}
		freshlyWritten = true
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, false, fmt.Errorf("read default %s: %w", path, err)
		}
		if err := toml.Unmarshal(data, &merged); err != nil {
			return nil, false, fmt.Errorf("parse default %s: %w", path, err)
		}
	}

	// Re-marshal the merged map and decode into the typed struct so the same
	// validation & defaulting paths apply regardless of how many layers
	// contributed.
	tomlBytes, err := toml.Marshal(merged)
	if err != nil {
		return nil, false, fmt.Errorf("re-marshal merged config: %w", err)
	}
	var cfg Config
	if err := toml.Unmarshal(tomlBytes, &cfg); err != nil {
		return nil, false, fmt.Errorf("decode merged config: %w", err)
	}
	// Expand $ENSO_* references in secret-bearing fields so committable
	// configs can say `api_key = "$ENSO_OPENAI_KEY"` instead of pasting
	// the literal token. Non-ENSO_ references collapse to "" (logged
	// once) — see ExpandEnsoEnv. MCP headers/args are expanded at
	// dial time in internal/mcp.
	for name, p := range cfg.Providers {
		p.APIKey = ExpandEnsoEnv(p.APIKey)
		cfg.Providers[name] = p
	}
	cfg.Search.SearXNG.APIKey = ExpandEnsoEnv(cfg.Search.SearXNG.APIKey)
	for k, v := range cfg.Bash.Sb.Credentials {
		cfg.Bash.Sb.Credentials[k] = ExpandEnsoEnv(v)
	}
	return &cfg, freshlyWritten, nil
}

// writeDefault creates the user config dir + file populated from defaultTOML.
// Returns the path written.
func writeDefault() (string, error) {
	path, err := UserConfigPath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("create config dir: %w", err)
	}
	// User config can hold api_key — clamp parent dir on every write
	// for installs that predate the 0700 tightening.
	_ = os.Chmod(filepath.Dir(path), 0o700)
	if err := os.WriteFile(path, []byte(defaultTOML), 0o600); err != nil {
		return "", fmt.Errorf("write default config: %w", err)
	}
	return path, nil
}

// mergeMaps does a recursive merge of src into dst. Nested maps are merged
// key-by-key; everything else is replaced. Used for layering TOML files.
func mergeMaps(dst, src map[string]any) {
	for k, v := range src {
		if existing, ok := dst[k]; ok {
			if dstMap, dok := existing.(map[string]any); dok {
				if srcMap, sok := v.(map[string]any); sok {
					mergeMaps(dstMap, srcMap)
					continue
				}
			}
		}
		dst[k] = v
	}
}
