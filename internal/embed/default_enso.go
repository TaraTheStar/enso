// SPDX-License-Identifier: AGPL-3.0-or-later

package embed

import _ "embed"

// DefaultSystemPrompt is the embedded default system prompt.
//
//go:embed default_enso.md
var DefaultSystemPrompt string
