// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/pelletier/go-toml/v2"
)

// Config is the top-level configuration structure.
type Config struct {
	// DefaultProvider names which entry in Providers is selected at
	// session start. Empty = alphabetical-first. The corresponding
	// `default_provider = "..."` TOML key must appear before any
	// [providers.X] section in the file (TOML scoping).
	DefaultProvider string `toml:"default_provider"`

	Providers   map[string]ProviderConfig `toml:"providers"`
	MCP         map[string]MCPConfig      `toml:"mcp"`
	Permissions PermConfig                `toml:"permissions"`
	UI          UIConfig                  `toml:"ui"`
	Git         GitConfig                 `toml:"git"`
	LSP         map[string]LSPConfig      `toml:"lsp"`
	Bash        BashConfig                `toml:"bash"`
	Hooks       HooksConfig               `toml:"hooks"`
	WebFetch    WebFetchConfig            `toml:"web_fetch"`
	Search      SearchConfig              `toml:"search"`
	Daemon      DaemonConfig              `toml:"daemon"`
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
	Endpoint      string        `toml:"endpoint"`
	Model         string        `toml:"model"`
	ContextWindow int           `toml:"context_window"`
	Concurrency   int           `toml:"concurrency"`
	APIKey        string        `toml:"api_key"`
	Sampler       SamplerConfig `toml:"sampler"`

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
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "enso", "config.toml"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("get home dir: %w", err)
	}
	return filepath.Join(home, ".config", "enso", "config.toml"), nil
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
