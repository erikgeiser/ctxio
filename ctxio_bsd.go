//go:build darwin || freebsd || netbsd || openbsd

package ctxio

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync/atomic"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// WrapFile returns a ContextIO interface wrapping the given *os.File with
// context-aware and explicitly cancelable I/O. The macOS and *BSD
// implementation is based on the kqueue mechanism with a fallback to the select
// cancel mechanism for TTYs (where kqueue misfires by reporting readiness before
// data is buffered in the line discipline), and a further fallback to
// deadline-based cancelation for files that support it but cannot be used with
// kqueue or select.
func WrapFile(file *os.File) (ContextIO, error) {
	fd := file.Fd()

	// kqueue misfires on TTYs: it reports the fd as ready even when no data is
	// buffered in the line discipline, causing Read to block.
	if term.IsTerminal(int(fd)) {
		cio, err := newSelectContextIO(file, fd, file.Name())
		if err == nil {
			return cio, nil
		}

		cio, deadlineErr := WrapDeadliner(file, file.Name())
		if deadlineErr == nil {
			return cio, nil
		}

		return nil, fmt.Errorf("could not find suitable cancelation strategy: %w",
			joinErrors(err, deadlineErr))
	}

	cio, err := newKqueueIO(file, fd, file.Name())
	if err == nil {
		return cio, nil
	}

	cio, selectErr := newSelectContextIO(file, fd, file.Name())
	if selectErr == nil {
		return cio, nil
	}

	cio, deadlineErr := WrapDeadliner(file, file.Name())
	if deadlineErr == nil {
		return cio, nil
	}

	return nil, fmt.Errorf("could not find suitable cancelation strategy: %w",
		joinErrors(err, selectErr, deadlineErr))
}

// WrapConn returns a ContextIO interface wrapping the given net.Conn with
// context-aware and explicitly cancelable I/O based on macOS's and BSD's kqueue
// mechanism. If the net.Conn does not support kqueue, it falls back to a
// deadline based approach.
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

	cio, kqErr := newKqueueIO(conn, fd, name)
	if kqErr != nil {
		cio, selectErr := newSelectContextIO(conn, fd, name)
		if selectErr == nil {
			return cio, nil
		}

		cio, deadlineErr := WrapDeadliner(conn, name)
		if deadlineErr == nil {
			return cio, nil
		}

		return nil, fmt.Errorf("could not find suitable cancelation strategy: %w",
			joinErrors(err, selectErr, deadlineErr))
	}

	return cio, nil
}

// WrapConn returns a ContextIO interface wrapping the given file descriptor
// with context-aware and explicitly cancelable I/O based on macOS's and BSD's
// kqueue mechanism. If the file does not support kqueue, it falls back to a
// deadline based approach.
func WrapFd(fd uintptr, name string) (ContextIO, error) {
	file := os.NewFile(fd, name)
	if file == nil {
		return nil, fmt.Errorf("invalid file descriptor %d", fd)
	}

	if term.IsTerminal(int(fd)) {
		cio, err := newSelectContextIO(file, fd, file.Name())
		if err == nil {
			return cio, nil
		}

		cio, deadlineErr := WrapDeadliner(file, file.Name())
		if deadlineErr == nil {
			return cio, nil
		}

		return nil, fmt.Errorf("could not find suitable cancelation strategy: %w",
			joinErrors(err, deadlineErr))
	}

	cio, err := newKqueueIO(file, fd, file.Name())
	if err == nil {
		return cio, nil
	}

	cio, selectErr := newSelectContextIO(file, fd, file.Name())
	if selectErr == nil {
		return cio, nil
	}

	cio, deadlineErr := WrapDeadliner(file, file.Name())
	if deadlineErr == nil {
		return cio, nil
	}

	return nil, fmt.Errorf("could not find suitable cancelation strategy: %w",
		joinErrors(err, selectErr, deadlineErr))
}

func newKqueueIO(rw io.ReadWriter, fd uintptr, name string) (ContextIO, error) {
	if int(fd) == -1 {
		return nil, fmt.Errorf("unsuitable file: %w", os.ErrClosed)
	}

	kQueue, err := unix.Kqueue()
	if err != nil {
		return nil, fmt.Errorf("create kqueue: %w", err)
	}

	r := &kqueueIO{
		rw:     rw,
		fd:     fd,
		name:   name,
		kQueue: kQueue,
	}

	r.readCancelReader, r.readCancelWriter, err = os.Pipe()
	if err != nil {
		_ = unix.Close(kQueue)

		return nil, err
	}

	r.writeCancelReader, r.writeCancelWriter, err = os.Pipe()
	if err != nil {
		_ = unix.Close(kQueue)
		_ = r.readCancelReader.Close()
		_ = r.readCancelWriter.Close()

		return nil, err
	}

	unix.SetKevent(&r.readEvent, int(fd), unix.EVFILT_READ, unix.EV_ADD)
	unix.SetKevent(&r.writeEvent, int(fd), unix.EVFILT_WRITE, unix.EV_ADD)
	unix.SetKevent(&r.readCancelEvent, int(r.readCancelReader.Fd()), unix.EVFILT_READ, unix.EV_ADD)
	unix.SetKevent(&r.writeCancelEvent, int(r.writeCancelReader.Fd()), unix.EVFILT_READ, unix.EV_ADD)

	return r, nil
}

type kqueueIO struct {
	rw                io.ReadWriter
	fd                uintptr
	name              string
	readCancelReader  *os.File
	readCancelWriter  *os.File
	writeCancelReader *os.File
	writeCancelWriter *os.File
	kQueue            int
	writeEvent        unix.Kevent_t
	readEvent         unix.Kevent_t
	readCancelEvent   unix.Kevent_t
	writeCancelEvent  unix.Kevent_t

	readCanceled  atomic.Bool
	writeCanceled atomic.Bool
}

func (r *kqueueIO) Name() string {
	return r.name
}

func (r *kqueueIO) ReadContext(ctx context.Context, data []byte) (int, error) {
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

func (r *kqueueIO) Read(data []byte) (int, error) {
	if r.readCanceled.Load() {
		return 0, ErrCanceled
	}

	return r.read(data)
}

func (r *kqueueIO) read(data []byte) (int, error) {
	bytesAvailable, err := r.waitRead()
	if err != nil {
		return 0, err
	}

	// Cap bytesAvailable to the caller's buffer size and fall back to
	// len(data) when kevent did not return a sensible amount.
	if bytesAvailable < 1 || bytesAvailable > int64(len(data)) {
		bytesAvailable = int64(len(data))
	}

	return r.rw.Read(data[:bytesAvailable])
}

func (r *kqueueIO) WriteContext(ctx context.Context, data []byte) (int, error) {
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

func (r *kqueueIO) Write(data []byte) (int, error) {
	if r.writeCanceled.Load() {
		return 0, ErrCanceled
	}

	return r.write(data)
}

func (r *kqueueIO) write(data []byte) (int, error) {
	bytesWritten := 0

	for bytesWritten < len(data) {
		bufferSize, err := r.waitWrite()
		if err != nil {
			return 0, err
		}

		// kqueue does not return buffer size of regular files and instead
		// returns the only safe size to write which is 1. However, writing one
		// byte at a time is incredibly slow, so we go for the risky full write
		// instead.
		if _, ok := r.rw.(*os.File); ok && bytesWritten == 0 && bufferSize < 2 {
			bufferSize = int64(len(data))
		}

		maxOffset := min(bytesWritten+int(bufferSize), len(data))

		n, err := r.rw.Write(data[bytesWritten:maxOffset])
		if err != nil {
			return bytesWritten + n, err
		}

		bytesWritten += n
	}

	return bytesWritten, nil
}

func (r *kqueueIO) CancelReads() {
	if r.readCanceled.CompareAndSwap(false, true) {
		_, _ = r.readCancelWriter.Write(make([]byte, 1))
	}
}

func (r *kqueueIO) CancelWrites() {
	if r.writeCanceled.CompareAndSwap(false, true) {
		_, _ = r.writeCancelWriter.Write(make([]byte, 1))
	}
}

func (r *kqueueIO) Cancel() {
	r.CancelReads()
	r.CancelWrites()
}

func (r *kqueueIO) ResetReader() {
	if r.readCanceled.Swap(false) {
		_, _ = r.readCancelReader.Read(make([]byte, 1))
	}
}

func (r *kqueueIO) ResetWriter() {
	if r.writeCanceled.Swap(false) {
		_, _ = r.writeCancelReader.Read(make([]byte, 1))
	}
}

func (r *kqueueIO) Reset() {
	r.ResetReader()
	r.ResetWriter()
}

func (r *kqueueIO) Close() error {
	r.Cancel()

	var errs []error

	err := unix.Close(r.kQueue)
	if err != nil {
		errs = append(errs, fmt.Errorf("closing kqueue: %w", err))
	}

	err = r.readCancelWriter.Close()
	if err != nil {
		errs = append(errs, fmt.Errorf("closing read cancel writer: %w", err))
	}

	err = r.readCancelReader.Close()
	if err != nil {
		errs = append(errs, fmt.Errorf("closing read cancel reader: %w", err))
	}

	err = r.writeCancelWriter.Close()
	if err != nil {
		errs = append(errs, fmt.Errorf("closing write cancel writer: %w", err))
	}

	err = r.writeCancelReader.Close()
	if err != nil {
		errs = append(errs, fmt.Errorf("closing write cancel reader: %w", err))
	}

	if c, ok := r.rw.(io.Closer); ok {
		err = c.Close()
		if err != nil {
			errs = append(errs, fmt.Errorf("closing underlying object: %w", err))
		}
	}

	if len(errs) > 0 {
		return joinErrors(errs...)
	}

	return nil
}

func (r *kqueueIO) waitRead() (int64, error) {
	activeEvent := make([]unix.Kevent_t, 1)

	for {
		_, err := unix.Kevent(r.kQueue, []unix.Kevent_t{r.readEvent, r.readCancelEvent}, activeEvent, nil)
		if errors.Is(err, unix.EINTR) {
			continue // try again if the syscall was interrupted
		}

		if err != nil {
			return 0, fmt.Errorf("kevent: %w", err)
		}

		break
	}

	switch activeEvent[0].Ident {
	case uint64(r.fd):
		return activeEvent[0].Data, nil
	case uint64(r.readCancelReader.Fd()):
		return 0, ErrCanceled
	}

	return 0, fmt.Errorf(
		"kevent identifier %d does not match primary file descriptor %d "+
			"or read cancelation pipe file descriptor %d",
		activeEvent[0].Ident, r.fd, r.readCancelReader.Fd())
}

func (r *kqueueIO) waitWrite() (int64, error) {
	activeEvent := make([]unix.Kevent_t, 1)

	for {
		_, err := unix.Kevent(r.kQueue, []unix.Kevent_t{r.writeEvent, r.writeCancelEvent}, activeEvent, nil)
		if errors.Is(err, unix.EINTR) {
			continue // try again if the syscall was interrupted
		}

		if err != nil {
			return 0, fmt.Errorf("kevent: %w", err)
		}

		break
	}

	switch activeEvent[0].Ident {
	case uint64(r.fd):
		return activeEvent[0].Data, nil
	case uint64(r.writeCancelReader.Fd()):
		return 0, ErrCanceled
	}

	return 0, fmt.Errorf(
		"kevent identifier %d does not match primary file descriptor %d "+
			"or write cancelation pipe file descriptor %d",
		activeEvent[0].Ident, r.fd, r.writeCancelReader.Fd())
}
