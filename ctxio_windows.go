//go:build windows

package ctxio

import (
	"fmt"
	"net"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

var fileShareValidFlags uint32 = 0x00000007

// WrapFile returns a ContextIO wrapping the given *os.File with context-aware
// and explicitly cancelable I/O. The implementation is chosen based on the type
// of Windows handle:
//   - Console handles: WaitForMultipleObjects on CONIN$ with overlapped reads
//   - Files and named pipes: Overlapped I/O with CancelIoEx
//   - Anonymous pipes: CancelSynchronousIo
func WrapFile(file *os.File) (ContextIO, error) {
	handle := windows.Handle(file.Fd())

	if mode := uint32(0); windows.GetConsoleMode(handle, &mode) == nil {
		return newWinConsoleIO(file, handle)
	}

	fileType, err := windows.GetFileType(handle)
	if err != nil {
		return nil, fmt.Errorf("GetFileType: %w", err)
	}

	switch fileType {
	case windows.FILE_TYPE_DISK:
		return newWinOverlappedIO(file, handle, true)
	case windows.FILE_TYPE_PIPE:
		return newWinOverlappedIO(file, handle, false)
	default:
		return WrapDeadliner(file, file.Name())
	}
}

// WrapConn returns a ContextIO wrapping the given net.Conn with context-aware
// and explicitly cancelable I/O using deadline-based cancelation.
func WrapConn(conn net.Conn) (ContextIO, error) {
	return newWinSocketIO(conn)
}

// WrapHandle returns a ContextIO wrapping the given Windows handle with
// context-aware and explicitly cancelable I/O. It auto-detects the handle type
// (console, file, pipe) and chooses the appropriate strategy.
func WrapHandle(handle windows.Handle) (ContextIO, error) {
	file := os.NewFile(uintptr(handle), fmt.Sprintf("handle:%d", handle))
	if file == nil {
		return nil, fmt.Errorf("invalid handle %d", handle)
	}

	return WrapFile(file)
}

// waitForMultipleObjects waits for either the I/O handle or the cancel event
// to become signaled. Used by the console implementation where the console
// handle itself is waitable.
func waitForMultipleObjects(ioHandle, cancelEvent windows.Handle) error {
	event, err := windows.WaitForMultipleObjects(
		[]windows.Handle{ioHandle, cancelEvent}, false, windows.INFINITE)

	switch {
	case event == windows.WAIT_OBJECT_0:
		return nil
	case event == windows.WAIT_OBJECT_0+1:
		return ErrCanceled
	case windows.WAIT_ABANDONED <= event && event < windows.WAIT_ABANDONED+2:
		return fmt.Errorf("wait abandoned")
	case event == uint32(windows.WAIT_TIMEOUT):
		return fmt.Errorf("wait timeout")
	case event == windows.WAIT_FAILED:
		return fmt.Errorf("wait failed: %w", err)
	default:
		return fmt.Errorf("unexpected wait result %d: %w", event, err)
	}
}

var (
	modkernel32                 = windows.NewLazySystemDLL("kernel32.dll")
	modntdll                    = windows.NewLazySystemDLL("ntdll.dll")
	procFlushConsoleInputBuffer = modkernel32.NewProc("FlushConsoleInputBuffer")
	procReOpenFile              = modkernel32.NewProc("ReOpenFile")
	procNtQueryInformationFile  = modntdll.NewProc("NtQueryInformationFile")
	procCancelSynchronousIo     = modkernel32.NewProc("CancelSynchronousIo")
)

// cancelSynchronousIo cancels a synchronous I/O operation that is pending on
// the given thread. Returns an error (including ERROR_NOT_FOUND if no I/O is
// currently pending on that thread).
func cancelSynchronousIo(thread windows.Handle) error {
	r, _, err := procCancelSynchronousIo.Call(uintptr(thread))
	if r == 0 {
		return err
	}

	return nil
}

func flushConsoleInputBuffer(consoleInput windows.Handle) error {
	r, _, err := procFlushConsoleInputBuffer.Call(uintptr(consoleInput))
	if r == 0 {
		return err
	}

	return nil
}

func reOpenFile(handle windows.Handle, desiredAccess, shareMode, flags uint32) (windows.Handle, error) {
	r, _, err := procReOpenFile.Call(
		uintptr(handle),
		uintptr(desiredAccess),
		uintptr(shareMode),
		uintptr(flags),
	)
	if windows.Handle(r) == windows.InvalidHandle {
		return windows.InvalidHandle, fmt.Errorf("ReOpenFile: %w", err)
	}

	return windows.Handle(r), nil
}

func isOverlapped(handle windows.Handle) (bool, error) {
	const fileModeInformation = 16

	var mode uint32

	// IO_STATUS_BLOCK is two pointer-sized fields.
	var iosb [2]uintptr

	r, _, err := procNtQueryInformationFile.Call(
		uintptr(handle),
		uintptr(unsafe.Pointer(&iosb[0])),
		uintptr(unsafe.Pointer(&mode)),
		unsafe.Sizeof(mode),
		fileModeInformation,
	)
	if r != 0 {
		return false, fmt.Errorf("NtQueryInformationFile: %w", err)
	}

	const (
		fileSynchronousIOAlert    = 0x00000010
		fileSynchronousIONonAlert = 0x00000020
	)

	return mode&(fileSynchronousIOAlert|fileSynchronousIONonAlert) == 0, nil
}
