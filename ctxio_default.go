//go:build !darwin && !windows && !linux && !solaris && !freebsd && !netbsd && !openbsd

package ctxio

import (
	"net"
	"os"
)

// WrapFile is an alias for WrapDeadliner on this platform.
func WrapFile(file *os.File) (ContextIO, error) {
	return WrapDeadliner(file, file.Name())
}

// WrapConn is an alias for WrapDeadliner on this platform.
func WrapConn(conn net.Conn) (ContextIO, error) {
	return WrapDeadliner(conn, connName(conn))
}
