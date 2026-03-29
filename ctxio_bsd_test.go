//go:build darwin || freebsd || netbsd || openbsd

package ctxio

import (
	"io"
)

var backends = []backend{
	{name: "kqueue", new: kqueueNew},
	{name: "select", new: selectNew, skipWriteCancel: true},
	{name: "deadline", new: deadlineNew},
}

func kqueueNew(rw io.ReadWriter, name string) (ContextIO, error) {
	fd, err := extractFd(rw)
	if err != nil {
		return nil, err
	}

	return newKqueueIO(rw, fd, name)
}
