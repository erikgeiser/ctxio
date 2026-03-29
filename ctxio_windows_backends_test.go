//go:build windows

package ctxio

// The overlapped and console backends require a Windows handle and cannot be
// constructed from a plain io.ReadWriter, so they are tested separately in
// ctxio_windows_test.go. The shared test suite runs against deadline-based I/O,
// which is what WrapConn uses on Windows.
var backends = []backend{
	{name: "deadline", new: deadlineNew},
}
