//go:build linux

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
)

// WrapFile returns a ContextIO interface wrapping the given *os.File with
// context-aware and explicitly cancelable I/O. The linux implementation is
// based on the epoll mechanism, falling back to deadline-based cancelation for
// files that support it but cannot be used with epoll (e.g. some device files).
func WrapFile(file *os.File) (ContextIO, error) {
	cio, err := newEpollIO(file, file.Fd(), file.Name())
	if err != nil {
		cio, selectErr := newSelectContextIO(file, file.Fd(), file.Name())
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

	return cio, err
}

// WrapConn returns a ContextIO interface wrapping the given net.Conn with
// context-aware and explicitly cancelable I/O based on the epoll mechanism,
// falling back to deadline-based cancelation if epoll is not supported for the
// net.Conn.
func WrapConn(conn net.Conn) (ContextIO, error) {
	name := connName(conn)

	fd, err := fdFromConn(conn)
	if err != nil {
		// Fall back to deadline-based cancelation for connections where the
		// file descriptor cannot be extracted.
		cio, deadlineErr := WrapDeadliner(conn, name)
		if deadlineErr == nil {
			return cio, nil
		}

		return nil, fmt.Errorf("could not find suitable cancelation strategy: %w",
			joinErrors(err, deadlineErr))
	}

	cio, epollErr := newEpollIO(conn, fd, name)
	if epollErr != nil {
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

// WrapFd returns a ContextIO interface wrapping the given file descriptor with
// context-aware and explicitly cancelable I/O based on the epoll mechanism,
// falling back to deadline-based cancelation if epoll is not supported for the
// file descriptor.
func WrapFd(fd uintptr, name string) (ContextIO, error) {
	file := os.NewFile(fd, name)
	if file == nil {
		return nil, fmt.Errorf("invalid file descriptor %d", fd)
	}

	return newEpollIO(file, fd, file.Name())
}

func newEpollIO(rw io.ReadWriter, fd uintptr, name string) (ContextIO, error) {
	if int(fd) == -1 {
		return nil, fmt.Errorf("unsuitable file: %w", os.ErrClosed)
	}

	readEpoll, err := unix.EpollCreate1(0)
	if err != nil {
		return nil, fmt.Errorf("create read epoll: %w", err)
	}

	writeEpoll, err := unix.EpollCreate1(0)
	if err != nil {
		_ = unix.Close(readEpoll)

		return nil, fmt.Errorf("create write epoll: %w", err)
	}

	readCancelReader, readCancelWriter, err := os.Pipe()
	if err != nil {
		_ = unix.Close(readEpoll)
		_ = unix.Close(writeEpoll)

		return nil, err
	}

	writeCancelReader, writeCancelWriter, err := os.Pipe()
	if err != nil {
		_ = unix.Close(readEpoll)
		_ = unix.Close(writeEpoll)
		_ = readCancelReader.Close()
		_ = readCancelWriter.Close()

		return nil, err
	}

	r := &epollIO{
		rw:                rw,
		fd:                fd,
		name:              name,
		readEpoll:         readEpoll,
		writeEpoll:        writeEpoll,
		readCancelReader:  readCancelReader,
		readCancelWriter:  readCancelWriter,
		writeCancelReader: writeCancelReader,
		writeCancelWriter: writeCancelWriter,
	}

	err = unix.EpollCtl(readEpoll, unix.EPOLL_CTL_ADD, int(fd), &unix.EpollEvent{
		Events: unix.EPOLLIN,
		Fd:     int32(fd),
	})
	if err != nil {
		r.closeAll()

		return nil, fmt.Errorf("add reader with descriptor %d to epoll interest list: %w",
			int(fd), err)
	}

	err = unix.EpollCtl(writeEpoll, unix.EPOLL_CTL_ADD, int(fd), &unix.EpollEvent{
		Events: unix.EPOLLOUT,
		Fd:     int32(fd),
	})
	if err != nil {
		r.closeAll()

		return nil, fmt.Errorf("add writer with descriptor %d to epoll interest list: %w",
			int(fd), err)
	}

	err = unix.EpollCtl(readEpoll, unix.EPOLL_CTL_ADD, int(r.readCancelReader.Fd()), &unix.EpollEvent{
		Events: unix.EPOLLIN,
		Fd:     int32(r.readCancelReader.Fd()),
	})
	if err != nil {
		r.closeAll()

		return nil, fmt.Errorf("add read cancel signal with descriptor %d to epoll interest list: %w",
			int(r.readCancelReader.Fd()), err)
	}

	err = unix.EpollCtl(writeEpoll, unix.EPOLL_CTL_ADD, int(r.writeCancelReader.Fd()), &unix.EpollEvent{
		Events: unix.EPOLLIN,
		Fd:     int32(r.writeCancelReader.Fd()),
	})
	if err != nil {
		r.closeAll()

		return nil, fmt.Errorf("add write cancel signal with descriptor %d to epoll interest list: %w",
			int(r.writeCancelReader.Fd()), err)
	}

	return r, nil
}

type epollIO struct {
	rw                io.ReadWriter
	fd                uintptr
	name              string
	readCancelReader  *os.File
	readCancelWriter  *os.File
	writeCancelReader *os.File
	writeCancelWriter *os.File
	readEpoll         int
	writeEpoll        int

	readCanceled  atomic.Bool
	writeCanceled atomic.Bool
}

func (r *epollIO) Name() string {
	return r.name
}

func (r *epollIO) ReadContext(ctx context.Context, data []byte) (int, error) {
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

func (r *epollIO) Read(data []byte) (int, error) {
	if r.readCanceled.Load() {
		return 0, ErrCanceled
	}

	return r.read(data)
}

func (r *epollIO) read(data []byte) (int, error) {
	err := r.waitRead()
	if err != nil {
		return 0, err
	}

	return r.rw.Read(data)
}

func (r *epollIO) WriteContext(ctx context.Context, data []byte) (int, error) {
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

func (r *epollIO) Write(data []byte) (int, error) {
	if r.writeCanceled.Load() {
		return 0, ErrCanceled
	}

	return r.write(data)
}

// writeChunkSize limits individual rw.Write calls so that the write loop
// returns to waitWrite frequently enough for cancellation to be responsive.
const writeChunkSize = 64 * 1024

func (r *epollIO) write(data []byte) (int, error) {
	bytesWritten := 0

	for bytesWritten < len(data) {
		err := r.waitWrite()
		if err != nil {
			return bytesWritten, err
		}

		end := min(bytesWritten+writeChunkSize, len(data))

		n, err := r.rw.Write(data[bytesWritten:end])
		bytesWritten += n

		if err != nil {
			return bytesWritten, err
		}
	}

	return bytesWritten, nil
}

func (r *epollIO) CancelReads() {
	if r.readCanceled.CompareAndSwap(false, true) {
		_, _ = r.readCancelWriter.Write(make([]byte, 1))
	}
}

func (r *epollIO) CancelWrites() {
	if r.writeCanceled.CompareAndSwap(false, true) {
		_, _ = r.writeCancelWriter.Write(make([]byte, 1))
	}
}

func (r *epollIO) Cancel() {
	r.CancelReads()
	r.CancelWrites()
}

func (r *epollIO) ResetReader() {
	if r.readCanceled.Swap(false) {
		_, _ = r.readCancelReader.Read(make([]byte, 1))
	}
}

func (r *epollIO) ResetWriter() {
	if r.writeCanceled.Swap(false) {
		_, _ = r.writeCancelReader.Read(make([]byte, 1))
	}
}

func (r *epollIO) Reset() {
	r.ResetReader()
	r.ResetWriter()
}

func (r *epollIO) closeAll() {
	_ = unix.Close(r.readEpoll)
	_ = unix.Close(r.writeEpoll)
	_ = r.readCancelReader.Close()
	_ = r.readCancelWriter.Close()
	_ = r.writeCancelReader.Close()
	_ = r.writeCancelWriter.Close()
}

func (r *epollIO) Close() error {
	r.Cancel()

	var errs []error

	err := unix.Close(r.readEpoll)
	if err != nil {
		errs = append(errs, fmt.Errorf("closing read epoll: %w", err))
	}

	err = unix.Close(r.writeEpoll)
	if err != nil {
		errs = append(errs, fmt.Errorf("closing write epoll: %w", err))
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

	// close underlying object
	if c, ok := r.rw.(io.Closer); ok {
		err := c.Close()
		if err != nil {
			errs = append(errs, fmt.Errorf("closing underlying object: %w", err))
		}
	}

	if len(errs) > 0 {
		return joinErrors(errs...)
	}

	return nil
}

func (r *epollIO) waitRead() error {
	events := make([]unix.EpollEvent, 1)

	for {
		_, err := unix.EpollWait(r.readEpoll, events, -1)
		if errors.Is(err, unix.EINTR) {
			continue
		}

		if err != nil {
			return fmt.Errorf("epoll wait: %w", err)
		}

		break
	}

	switch events[0].Fd {
	case int32(r.fd):
		return nil
	case int32(r.readCancelReader.Fd()):
		return ErrCanceled
	}

	return fmt.Errorf("epoll_wait returned event with file descriptor %d "+
		"instead of primary file descriptor %d or read cancelation pipe file descriptor %d",
		events[0].Fd, r.fd, r.readCancelReader.Fd())
}

func (r *epollIO) waitWrite() error {
	events := make([]unix.EpollEvent, 1)

	for {
		_, err := unix.EpollWait(r.writeEpoll, events, -1)
		if errors.Is(err, unix.EINTR) {
			continue
		}

		if err != nil {
			return fmt.Errorf("epoll wait: %w", err)
		}

		break
	}

	switch events[0].Fd {
	case int32(r.fd):
		return nil
	case int32(r.writeCancelReader.Fd()):
		return ErrCanceled
	}

	return fmt.Errorf("epoll_wait returned event with file descriptor %d "+
		"instead of primary file descriptor %d or write cancelation pipe file descriptor %d",
		events[0].Fd, r.fd, r.writeCancelReader.Fd())
}
