// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !windows

package tools

import (
	"os"
	"os/exec"
	"syscall"
)

// setProcessGroup tells the OS to put the spawned process in its own
// process group. Without this, exec.CommandContext's auto-cancel would
// only SIGKILL the parent (`sh`) and leave any pipeline children
// (long_running | other_thing) orphaned.
func setProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killProcessGroup sends SIGKILL to every process in the group whose
// leader is p. Falls back to killing just p if Getpgid fails (which
// shouldn't happen if setProcessGroup ran beforehand).
func killProcessGroup(p *os.Process) error {
	if p == nil {
		return nil
	}
	pgid, err := syscall.Getpgid(p.Pid)
	if err == nil {
		return syscall.Kill(-pgid, syscall.SIGKILL)
	}
	return p.Kill()
}
