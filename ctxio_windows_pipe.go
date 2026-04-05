//go:build windows

package ctxio

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"

	"golang.org/x/sys/windows"
)

//nolint:unparam
func newWinAnonymousPipeIO(file *os.File, handle windows.Handle) (*winAnonymousPipeIO, error) {
	return &winAnonymousPipeIO{
		file:   file,
		handle: handle,
	}, nil
}

// winAnonymousPipeIO implements ContextIO for Windows anonymous pipe handles,
// which cannot be reopened with FILE_FLAG_OVERLAPPED and therefore cannot use
// the overlapped I/O mechanism. Instead, each blocking ReadFile/WriteFile runs
// on a goroutine locked to its own OS thread, and cancellation is performed via
// CancelSynchronousIo on that thread's handle.
type winAnonymousPipeIO struct {
	file   *os.File
	handle windows.Handle

	readCanceled  atomic.Bool
	writeCanceled atomic.Bool

	readMu           sync.Mutex
	readThreadHandle windows.Handle // real thread handle while a read is in progress, else 0
	readOpDone       chan struct{}  // closed when the current read op goroutine finishes

	writeMu           sync.Mutex
	writeThreadHandle windows.Handle
	writeOpDone       chan struct{}
}

func (r *winAnonymousPipeIO) Name() string { return r.file.Name() }

func (r *winAnonymousPipeIO) ReadContext(ctx context.Context, data []byte) (int, error) {
	if ctx.Err() != nil {
		return 0, ErrCanceled
	}

	if r.readCanceled.Load() {
		r.ResetReader()
	}

	done := cancelWhenContextIsDone(ctx, r.CancelReads)
	defer done()

	return r.read(data)
}

func (r *winAnonymousPipeIO) Read(data []byte) (int, error) {
	if r.readCanceled.Load() {
		return 0, ErrCanceled
	}

	return r.read(data)
}

func (r *winAnonymousPipeIO) read(data []byte) (int, error) {
	return r.doSyncIO(
		&r.readMu,
		&r.readOpDone,
		&r.readThreadHandle,
		func(buf []byte, n *uint32) error { return windows.ReadFile(r.handle, buf, n, nil) },
		data,
	)
}

// doSyncIO runs a blocking Windows I/O call (fn) on its own OS thread so that
// CancelSynchronousIo can interrupt it from another goroutine. mu guards both
// opDoneField (the channel that signals goroutine exit) and threadHandleField
// (the real thread handle used for cancellation).
func (r *winAnonymousPipeIO) doSyncIO(
	mu *sync.Mutex,
	opDoneField *chan struct{},
	threadHandleField *windows.Handle,
	fn func(buf []byte, n *uint32) error,
	data []byte,
) (int, error) {
	type result struct {
		n   int
		err error
	}

	resultCh := make(chan result, 1)
	opDone := make(chan struct{})

	mu.Lock()
	*opDoneField = opDone
	mu.Unlock()

	go func() {
		runtime.LockOSThread()

		defer func() {
			mu.Lock()
			*opDoneField = nil
			mu.Unlock()
			runtime.UnlockOSThread()
			close(opDone)
		}()

		var realHandle windows.Handle

		err := windows.DuplicateHandle(
			windows.CurrentProcess(),
			windows.CurrentThread(),
			windows.CurrentProcess(),
			&realHandle,
			0,
			false,
			windows.DUPLICATE_SAME_ACCESS,
		)
		if err != nil {
			// No thread handle — do the I/O without cancellation support.
			var n uint32

			ioErr := fn(data, &n)
			resultCh <- result{int(n), ioErr}

			return
		}
		defer windows.CloseHandle(realHandle)

		mu.Lock()
		*threadHandleField = realHandle
		mu.Unlock()

		var n uint32

		ioErr := fn(data, &n)

		mu.Lock()
		*threadHandleField = 0
		mu.Unlock()

		if errors.Is(ioErr, windows.ERROR_OPERATION_ABORTED) {
			resultCh <- result{0, ErrCanceled}
		} else {
			resultCh <- result{int(n), ioErr}
		}
	}()

	res := <-resultCh

	return res.n, res.err
}

func (r *winAnonymousPipeIO) WriteContext(ctx context.Context, data []byte) (int, error) {
	if ctx.Err() != nil {
		return 0, ErrCanceled
	}

	if r.writeCanceled.Load() {
		r.ResetWriter()
	}

	done := cancelWhenContextIsDone(ctx, r.CancelWrites)
	defer done()

	return r.write(data)
}

func (r *winAnonymousPipeIO) Write(data []byte) (int, error) {
	if r.writeCanceled.Load() {
		return 0, ErrCanceled
	}

	return r.write(data)
}

func (r *winAnonymousPipeIO) write(data []byte) (int, error) {
	return r.doSyncIO(
		&r.writeMu,
		&r.writeOpDone,
		&r.writeThreadHandle,
		func(buf []byte, n *uint32) error { return windows.WriteFile(r.handle, buf, n, nil) },
		data,
	)
}

func (r *winAnonymousPipeIO) CancelReads() {
	r.readCanceled.Store(true)

	r.readMu.Lock()
	h := r.readThreadHandle
	done := r.readOpDone
	r.readMu.Unlock()

	if h == 0 || done == nil {
		return
	}

	// Retry CancelSynchronousIo until it succeeds or the I/O completes on its
	// own. There is a narrow window between when the goroutine registers the
	// thread handle and when ReadFile actually starts; if CancelSynchronousIo
	// returns ERROR_NOT_FOUND it means no I/O is pending yet, so we yield and
	// try again.
	for {
		select {
		case <-done:
			return
		default:
		}

		_ = cancelSynchronousIo(h)

		runtime.Gosched()
	}
}

func (r *winAnonymousPipeIO) CancelWrites() {
	r.writeCanceled.Store(true)

	r.writeMu.Lock()
	h := r.writeThreadHandle
	done := r.writeOpDone
	r.writeMu.Unlock()

	if h == 0 || done == nil {
		return
	}

	for {
		select {
		case <-done:
			return
		default:
		}

		_ = cancelSynchronousIo(h)

		runtime.Gosched()
	}
}

func (r *winAnonymousPipeIO) Cancel() {
	r.CancelReads()
	r.CancelWrites()
}

func (r *winAnonymousPipeIO) ResetReader() {
	r.readCanceled.Store(false)
}

func (r *winAnonymousPipeIO) ResetWriter() {
	r.writeCanceled.Store(false)
}

func (r *winAnonymousPipeIO) Reset() {
	r.ResetReader()
	r.ResetWriter()
}

func (r *winAnonymousPipeIO) Close() error {
	r.Cancel()

	var errMsgs []string

	err := r.file.Close()
	if err != nil {
		errMsgs = append(errMsgs, fmt.Sprintf("closing underlying file: %v", err))
	}

	if len(errMsgs) > 0 {
		return fmt.Errorf("%s", strings.Join(errMsgs, ", "))
	}

	return nil
}
