// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"log/slog"
	"os"
	"strings"
	"sync"
)

// envPrefix gates which environment variables config values may reference
// via $VAR / ${VAR}. The threat is `git clone <hostile-repo> && cd && enso`:
// the trust prompt blocks committed config from auto-loading at all, but a
// user who's already trusted a project still shouldn't have their AWS /
// GitHub / Anthropic tokens harvested via `api_key = "$AWS_SECRET_ACCESS_KEY"`
// or `[mcp.headers] Authorization = "Bearer $GITHUB_TOKEN"`. Limiting
// expansion to ENSO_-prefixed names means a user who wants a token
// available to enso must opt in by re-exporting:
//
//	export ENSO_OPENAI_KEY=$OPENAI_API_KEY
//
// Anything outside that prefix expands to the empty string and a single
// slog.Warn is logged per unique offending name.
const envPrefix = "ENSO_"

var (
	warnedEnvMu sync.Mutex
	warnedEnv   = map[string]struct{}{}
)

// ExpandEnsoEnv resolves $VAR / ${VAR} references in s, but only for
// variables whose name starts with envPrefix. Other references resolve
// to "" — the empty value will fail visibly downstream (e.g. an
// `Authorization: Bearer ` header → 401, or an empty API key → 401),
// making the misconfiguration loud rather than silently exfiltrating
// an unrelated secret.
func ExpandEnsoEnv(s string) string {
	return os.Expand(s, func(name string) string {
		if strings.HasPrefix(name, envPrefix) {
			return os.Getenv(name)
		}
		warnEnvOnce(name)
		return ""
	})
}

func warnEnvOnce(name string) {
	warnedEnvMu.Lock()
	_, seen := warnedEnv[name]
	if !seen {
		warnedEnv[name] = struct{}{}
	}
	warnedEnvMu.Unlock()
	if seen {
		return
	}
	slog.Warn("config: env var reference ignored — only ENSO_-prefixed names are expanded",
		"var", name, "prefix", envPrefix)
}
