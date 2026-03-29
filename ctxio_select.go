//go:build linux || darwin || freebsd || netbsd || openbsd || solaris

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

func newSelectContextIO(rw io.ReadWriter, fd uintptr, name string) (ContextIO, error) {
	if fd >= unix.FD_SETSIZE {
		return nil, fmt.Errorf("file descriptor %d exceeds maximum of %d", fd, unix.FD_SETSIZE)
	}

	r := &selectIO{rw: rw, fd: fd, name: name}

	var err error

	r.readCancelReader, r.readCancelWriter, err = os.Pipe()
	if err != nil {
		return nil, err
	}

	r.writeCancelReader, r.writeCancelWriter, err = os.Pipe()
	if err != nil {
		_ = r.readCancelReader.Close()
		_ = r.readCancelWriter.Close()

		return nil, err
	}

	if r.readCancelReader.Fd() >= unix.FD_SETSIZE {
		return nil, fmt.Errorf("cancelation pipe file selector %d exceeds maximum of %d",
			r.readCancelReader.Fd(), unix.FD_SETSIZE)
	}

	return r, nil
}

type selectIO struct {
	rw                io.ReadWriter
	fd                uintptr
	name              string
	readCancelReader  *os.File
	readCancelWriter  *os.File
	writeCancelReader *os.File
	writeCancelWriter *os.File

	readCanceled  atomic.Bool
	writeCanceled atomic.Bool
}

func (r *selectIO) Name() string {
	return r.name
}

func (r *selectIO) ReadContext(ctx context.Context, data []byte) (int, error) {
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

func (r *selectIO) Read(data []byte) (int, error) {
	if r.readCanceled.Load() {
		return 0, ErrCanceled
	}

	return r.read(data)
}

func (r *selectIO) read(data []byte) (int, error) {
	for {
		err := waitForRead(int(r.fd), r.readCancelReader)
		if err != nil {
			if errors.Is(err, unix.EINTR) {
				continue // try again if the syscall was interrupted
			}

			return 0, err
		}

		return r.rw.Read(data)
	}
}

func (r *selectIO) WriteContext(ctx context.Context, data []byte) (int, error) {
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

func (r *selectIO) Write(data []byte) (int, error) {
	if r.writeCanceled.Load() {
		return 0, ErrCanceled
	}

	return r.write(data)
}

func (r *selectIO) write(data []byte) (int, error) {
	for {
		err := waitForWrite(int(r.fd), r.writeCancelReader)
		if err != nil {
			if errors.Is(err, unix.EINTR) {
				continue // try again if the syscall was interrupted
			}

			return 0, err
		}

		return r.rw.Write(data)
	}
}

func (r *selectIO) CancelReads() {
	if r.readCanceled.CompareAndSwap(false, true) {
		_, _ = r.readCancelWriter.Write(make([]byte, 1))
	}
}

func (r *selectIO) CancelWrites() {
	if r.writeCanceled.CompareAndSwap(false, true) {
		_, _ = r.writeCancelWriter.Write(make([]byte, 1))
	}
}

func (r *selectIO) Cancel() {
	r.CancelReads()
	r.CancelWrites()
}

func (r *selectIO) resetReader() {
	if r.readCanceled.Swap(false) {
		_, _ = r.readCancelReader.Read(make([]byte, 1))
	}
}

func (r *selectIO) resetWriter() {
	if r.writeCanceled.Swap(false) {
		_, _ = r.writeCancelReader.Read(make([]byte, 1))
	}
}

func (r *selectIO) ResetReader() { r.resetReader() }
func (r *selectIO) ResetWriter() { r.resetWriter() }

func (r *selectIO) Reset() {
	r.ResetReader()
	r.ResetWriter()
}

func (r *selectIO) Close() error {
	r.Cancel()

	var errs []error

	err := r.readCancelWriter.Close()
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

func waitForRead(readerFd int, abort *os.File) error {
	abortFd := int(abort.Fd())

	maxFd := max(abortFd, readerFd)

	readFdSet := &unix.FdSet{}
	readFdSet.Set(readerFd)
	readFdSet.Set(abortFd)

	_, err := unix.Select(maxFd+1, readFdSet, nil, nil, nil)
	if err != nil {
		return fmt.Errorf("select: %w", err)
	}

	if readFdSet.IsSet(abortFd) {
		return ErrCanceled
	}

	if readFdSet.IsSet(readerFd) {
		return nil
	}

	return fmt.Errorf("select returned without setting a file descriptor")
}

func waitForWrite(writerFd int, abort *os.File) error {
	abortFd := int(abort.Fd())

	maxFd := max(abortFd, writerFd)

	readFdSet := &unix.FdSet{}
	readFdSet.Set(abortFd)

	writeFdSet := &unix.FdSet{}
	writeFdSet.Set(writerFd)

	_, err := unix.Select(maxFd+1, readFdSet, writeFdSet, nil, nil)
	if err != nil {
		return fmt.Errorf("select: %w", err)
	}

	if readFdSet.IsSet(abortFd) {
		return ErrCanceled
	}

	if writeFdSet.IsSet(writerFd) {
		return nil
	}

	return fmt.Errorf("select returned without setting a file descriptor")
}

func selectNew(rw io.ReadWriter, name string) (ContextIO, error) {
	fd, err := extractFd(rw)
	if err != nil {
		return nil, err
	}

	return newSelectContextIO(rw, fd, name)
}

// extractFd returns the file descriptor for an *os.File or net.Conn.
func extractFd(rw io.ReadWriter) (uintptr, error) {
	switch v := rw.(type) {
	case *os.File:
		return v.Fd(), nil
	case net.Conn:
		return fdFromConn(v)
	default:
		return 0, ErrNotSupported
	}
}

func fdFromConn(conn net.Conn) (uintptr, error) {
	c, ok := conn.(syscallConner)
	if !ok {
		return 0, fmt.Errorf("cannot determine file descriptor of %T", conn)
	}

	rawConn, err := c.SyscallConn()
	if err != nil {
		return 0, err
	}

	var fd uintptr

	err = rawConn.Control(func(passedFd uintptr) { fd = passedFd })
	if err != nil {
		return 0, fmt.Errorf("getting file descriptor via rawConn.Control: %w", err)
	}

	return fd, nil
}
