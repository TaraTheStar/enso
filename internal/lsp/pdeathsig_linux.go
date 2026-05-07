// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build linux

package lsp

import (
	"os/exec"
	"syscall"
)

// setPdeathsig asks the kernel to SIGKILL the LSP child the moment its
// parent (enso) dies — including SIGKILL of enso itself, which gives no
// shutdown hook a chance to fire. Without this, gopls / rust-analyzer /
// etc. get reparented to PID 1 and linger across crashes, holding file
// watchers and indexing state.
//
// SIGKILL not SIGTERM: parent is already dead, no graceful work to do,
// and SIGTERM is ignorable while SIGKILL is not.
func setPdeathsig(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL
}
