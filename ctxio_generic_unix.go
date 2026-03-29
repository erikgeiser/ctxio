//go:build solaris

package ctxio

import (
	"fmt"
	"net"
	"os"
)

// WrapFile returns a ContextIO based on the POSIX select syscall, falling back
// to deadline-based cancelation for files that support it but have a file
// descriptor >= FD_SETSIZE.
func WrapFile(file *os.File) (ContextIO, error) {
	cio, err := newSelectContextIO(file, file.Fd(), file.Name())
	if err != nil {
		cio, deadlineErr := WrapDeadliner(file, file.Name())
		if deadlineErr == nil {
			return cio, nil
		}

		return nil, fmt.Errorf("could not find suitable cancelation strategy: %w",
			joinErrors(err, deadlineErr))
	}

	return cio, err
}

// WrapConn returns a ContextIO wrapping the given net.Conn. Uses deadline-based
// cancelation if the file descriptor cannot be extracted, otherwise uses
// select.
func WrapConn(conn net.Conn) (ContextIO, error) {
	name := connName(conn)

	fd, err := fdFromConn(conn)
	if err != nil {
		cio, deadlineErr := WrapDeadliner(conn, name)
		if deadlineErr == nil {
			return cio, nil
		}

		return nil, fmt.Errorf("could not find suitable cancelation strategy: %w",
			joinErrors(err, deadlineErr))
	}

	cio, selectErr := newSelectContextIO(conn, fd, name)
	if selectErr != nil {
		cio, deadlineErr := WrapDeadliner(conn, name)
		if deadlineErr == nil {
			return cio, nil
		}

		return nil, fmt.Errorf("could not find suitable cancelation strategy: %w",
			joinErrors(err, deadlineErr))
	}

	return cio, nil
}

// WrapFd returns a ContextIO wrapping the given file descriptor.
func WrapFd(fd uintptr, name string) (ContextIO, error) {
	file := os.NewFile(fd, name)
	if file == nil {
		return nil, fmt.Errorf("invalid file descriptor %d", fd)
	}

	cio, err := newSelectContextIO(file, fd, file.Name())
	if err != nil {
		cio, deadlineErr := WrapDeadliner(file, name)
		if deadlineErr == nil {
			return cio, nil
		}

		return nil, fmt.Errorf("could not find suitable cancelation strategy: %w",
			joinErrors(err, deadlineErr))
	}

	return cio, err
}
