// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !linux && !windows

package daemon

import "net"

// checkPeer is a no-op on non-Linux unix systems. The 0700 directory and
// 0600 socket are the primary defence; Linux gets SO_PEERCRED on top.
// macOS would use LOCAL_PEERCRED via getsockopt — not implemented since
// macOS isn't a documented daemon target.
func checkPeer(_ net.Conn) error { return nil }
