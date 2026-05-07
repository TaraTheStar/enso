// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package tools

import (
	"os"
	"os/exec"
)

// setProcessGroup is a no-op on Windows. The orphan-pipeline-children
// problem doesn't apply the same way (Windows job objects would be the
// equivalent fix; not in scope — daemon is !windows tagged anyway).
func setProcessGroup(_ *exec.Cmd) {}

// killProcessGroup falls back to killing just p on Windows.
func killProcessGroup(p *os.Process) error {
	if p == nil {
		return nil
	}
	return p.Kill()
}
