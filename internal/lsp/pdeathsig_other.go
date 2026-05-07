// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !linux

package lsp

import "os/exec"

// setPdeathsig is a no-op outside Linux. macOS / BSD lack the
// equivalent kernel hook (PR_SET_PDEATHSIG); the LSP child surviving a
// parent crash is annoying but not security-impacting.
func setPdeathsig(_ *exec.Cmd) {}
