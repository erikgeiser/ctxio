//go:build windows

package ctxio

import (
	"net"
)

func newWinSocketIO(conn net.Conn) (ContextIO, error) {
	return WrapDeadliner(conn, connName(conn))
}
