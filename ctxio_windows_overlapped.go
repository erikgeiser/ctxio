//go:build windows

package ctxio

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync/atomic"

	"golang.org/x/sys/windows"
)

func newWinOverlappedIO(file *os.File, handle windows.Handle, isFile bool) (ContextIO, error) {
	overlapped, err := isOverlapped(handle)
	if err != nil {
		return nil, fmt.Errorf("check overlapped mode: %w", err)
	}

	var (
		overlappedHandle windows.Handle
		ownHandle        bool
	)

	if overlapped {
		// Handle is already in overlapped mode — use it directly.
		overlappedHandle = handle
	} else {
		// Reopen the handle with FILE_FLAG_OVERLAPPED so we can use
		// WaitForMultipleObjects on the overlapped event. This fails for
		// anonymous pipes, which is how we detect them.
		overlappedHandle, err = reOpenFile(
			handle,
			windows.GENERIC_READ|windows.GENERIC_WRITE,
			fileShareValidFlags,
			windows.FILE_FLAG_OVERLAPPED,
		)
		if err != nil {
			// ReOpenFile fails for anonymous pipes, which do not support
			// FILE_FLAG_OVERLAPPED. Fall back to the synchronous I/O backend
			// that uses CancelSynchronousIo for cancellation.
			return newWinAnonymousPipeIO(file, handle)
		}

		ownHandle = true
	}

	readCancel, err := windows.CreateEvent(nil, 0, 0, nil)
	if err != nil {
		_ = windows.CloseHandle(overlappedHandle)

		return nil, fmt.Errorf("create read cancel event: %w", err)
	}

	writeCancel, err := windows.CreateEvent(nil, 0, 0, nil)
	if err != nil {
		_ = windows.CloseHandle(overlappedHandle)
		_ = windows.CloseHandle(readCancel)

		return nil, fmt.Errorf("create write cancel event: %w", err)
	}

	return &winOverlappedIO{
		file:        file,
		handle:      overlappedHandle,
		readCancel:  readCancel,
		writeCancel: writeCancel,
		isFile:      isFile,
		ownHandle:   ownHandle,
	}, nil
}

type winOverlappedIO struct {
	file        *os.File
	handle      windows.Handle // reopened with FILE_FLAG_OVERLAPPED
	readCancel  windows.Handle
	writeCancel windows.Handle
	ownHandle   bool // false when handle is the original (not reopened)

	readCanceled  atomic.Bool
	writeCanceled atomic.Bool

	isFile      bool
	readOffset  int64
	writeOffset int64
}

func (r *winOverlappedIO) Name() string { return r.file.Name() }

func (r *winOverlappedIO) ReadContext(ctx context.Context, data []byte) (int, error) {
	if ctx.Err() != nil {
		return 0, ErrCanceled
	}

	if r.readCanceled.Load() {
		r.resetReader()
	}

	done := cancelWhenContextIsDone(ctx, r.CancelReads)
	defer done()

	return r.read(data)
}

func (r *winOverlappedIO) Read(data []byte) (int, error) {
	if r.readCanceled.Load() {
		return 0, ErrCanceled
	}

	return r.read(data)
}

func (r *winOverlappedIO) read(data []byte) (int, error) {
	n, err := r.overlappedIO(data, r.readOffset, true, r.readCancel)
	if err != nil {
		return n, err
	}

	if r.isFile {
		r.readOffset += int64(n)
	}

	return n, nil
}

func (r *winOverlappedIO) WriteContext(ctx context.Context, data []byte) (int, error) {
	if ctx.Err() != nil {
		return 0, ErrCanceled
	}

	if r.writeCanceled.Load() {
		r.resetWriter()
	}

	done := cancelWhenContextIsDone(ctx, r.CancelWrites)
	defer done()

	return r.write(data)
}

func (r *winOverlappedIO) Write(data []byte) (int, error) {
	if r.writeCanceled.Load() {
		return 0, ErrCanceled
	}

	return r.write(data)
}

func (r *winOverlappedIO) write(data []byte) (int, error) {
	n, err := r.overlappedIO(data, r.writeOffset, false, r.writeCancel)
	if err != nil {
		return n, err
	}

	if r.isFile {
		r.writeOffset += int64(n)
	}

	return n, nil
}

// overlappedIO performs a single overlapped ReadFile or WriteFile, waiting on
// both the I/O completion event and the cancel event.
func (r *winOverlappedIO) overlappedIO(data []byte, offset int64, read bool, cancelEvent windows.Handle) (int, error) {
	hevent, err := windows.CreateEvent(nil, 0, 0, nil)
	if err != nil {
		return 0, fmt.Errorf("create event: %w", err)
	}
	defer windows.CloseHandle(hevent)

	overlapped := windows.Overlapped{HEvent: hevent}
	if r.isFile {
		overlapped.Offset = uint32(offset)
		overlapped.OffsetHigh = uint32(offset >> 32)
	}

	var n uint32

	if read {
		err = windows.ReadFile(r.handle, data, &n, &overlapped)
	} else {
		err = windows.WriteFile(r.handle, data, &n, &overlapped)
	}

	if err == nil {
		// Completed synchronously.
		return int(n), nil
	}

	if !errors.Is(err, windows.ERROR_IO_PENDING) {
		return int(n), err
	}

	// I/O is pending — wait for completion or cancelation.
	event, waitErr := windows.WaitForMultipleObjects(
		[]windows.Handle{hevent, cancelEvent}, false, windows.INFINITE)

	switch event {
	case windows.WAIT_OBJECT_0:
		// I/O completed.
		err = windows.GetOverlappedResult(r.handle, &overlapped, &n, false)
		if errors.Is(err, windows.ERROR_OPERATION_ABORTED) {
			return 0, ErrCanceled
		} else if err != nil {
			return int(n), err
		}

		return int(n), nil

	case windows.WAIT_OBJECT_0 + 1:
		// Cancel event signaled — abort pending I/O.
		_ = windows.CancelIoEx(r.handle, &overlapped)

		// Drain the completion to avoid a resource leak.
		_ = windows.GetOverlappedResult(r.handle, &overlapped, &n, true)

		return 0, ErrCanceled

	default:
		return 0, fmt.Errorf("WaitForMultipleObjects: %w", waitErr)
	}
}

func (r *winOverlappedIO) CancelReads() {
	r.readCanceled.Store(true)
	// Signal the cancel event to wake WaitForMultipleObjects, and also
	// cancel kernel-level I/O in case the signal arrives before the wait
	// has started. Both are needed to avoid a missed-wakeup race.
	_ = windows.SetEvent(r.readCancel)
	_ = windows.CancelIoEx(r.handle, nil)
}

func (r *winOverlappedIO) CancelWrites() {
	r.writeCanceled.Store(true)
	// See CancelReads for why both mechanisms are needed.
	_ = windows.SetEvent(r.writeCancel)
	_ = windows.CancelIoEx(r.handle, nil)
}

func (r *winOverlappedIO) Cancel() {
	r.CancelReads()
	r.CancelWrites()
}

func (r *winOverlappedIO) resetReader() {
	r.readCanceled.Store(false)
	_ = windows.ResetEvent(r.readCancel)
}

func (r *winOverlappedIO) resetWriter() {
	r.writeCanceled.Store(false)
	_ = windows.ResetEvent(r.writeCancel)
}

func (r *winOverlappedIO) ResetReader() { r.resetReader() }
func (r *winOverlappedIO) ResetWriter() { r.resetWriter() }

func (r *winOverlappedIO) Reset() {
	r.ResetReader()
	r.ResetWriter()
}

func (r *winOverlappedIO) Close() error {
	r.Cancel()

	var errs []error

	err := windows.CloseHandle(r.readCancel)
	if err != nil {
		errs = append(errs, fmt.Errorf("closing read cancel event: %w", err))
	}

	err = windows.CloseHandle(r.writeCancel)
	if err != nil {
		errs = append(errs, fmt.Errorf("closing write cancel event: %w", err))
	}

	if r.ownHandle {
		err := windows.CloseHandle(r.handle)
		if err != nil {
			errs = append(errs, fmt.Errorf("closing overlapped handle: %w", err))
		}
	}

	// close the underlying file
	err = r.file.Close()
	if err != nil {
		errs = append(errs, fmt.Errorf("closing underlying file: %w", err))
	}

	if len(errs) > 0 {
		return joinErrors(errs...)
	}

	return nil
}
