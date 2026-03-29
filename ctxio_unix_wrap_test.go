//go:build !windows

package ctxio

import "net"

// deadlinerOnlyConn wraps a net.Conn but hides its SyscallConn method so that
// fdFromConn fails and WrapConn falls back to deadline-based I/O.
type deadlinerOnlyConn struct {
	net.Conn
}

// Verify that deadlinerOnlyConn satisfies Deadliner but not syscallConner.
var (
	_ Deadliner = deadlinerOnlyConn{}
	_ net.Conn  = deadlinerOnlyConn{}
)
