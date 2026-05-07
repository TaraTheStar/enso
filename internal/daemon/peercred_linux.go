// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build linux

package daemon

import (
	"fmt"
	"net"
	"os"
	"syscall"
)

// checkPeer rejects connections whose peer UID doesn't match the
// daemon's own UID. The 0700 directory and 0600 socket already block
// other users at the filesystem layer; this is a belt-and-suspenders
// check against bind-mount / namespace edge cases. Returns nil for
// non-unix transports (defensive — Accept on a unix listener should
// only ever return *net.UnixConn).
func checkPeer(c net.Conn) error {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return nil
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return fmt.Errorf("syscall conn: %w", err)
	}
	var ucred *syscall.Ucred
	var sockoptErr error
	cerr := raw.Control(func(fd uintptr) {
		ucred, sockoptErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	})
	if cerr != nil {
		return fmt.Errorf("control: %w", cerr)
	}
	if sockoptErr != nil {
		return fmt.Errorf("SO_PEERCRED: %w", sockoptErr)
	}
	if ucred == nil {
		return fmt.Errorf("SO_PEERCRED returned nil")
	}
	if int(ucred.Uid) != os.Getuid() {
		return fmt.Errorf("peer uid %d does not match daemon uid %d", ucred.Uid, os.Getuid())
	}
	return nil
}
