//go:build !darwin && !windows && !linux && !solaris && !freebsd && !netbsd && !openbsd

package ctxio

var backends = []backend{
	{name: "deadline", new: deadlineNew},
}
