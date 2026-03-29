//go:build linux

package ctxio

import (
	"io"
)

var backends = []backend{
	{name: "epoll", new: epollNew},
	{name: "select", new: selectNew, skipWriteCancel: true},
	{name: "deadline", new: deadlineNew},
}

func epollNew(rw io.ReadWriter, name string) (ContextIO, error) {
	fd, err := extractFd(rw)
	if err != nil {
		return nil, err
	}

	return newEpollIO(rw, fd, name)
}
