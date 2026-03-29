//go:build solaris

package ctxio

var backends = []backend{
	{name: "select", new: selectNew, skipWriteCancel: true},
	{name: "deadline", new: deadlineNew},
}
