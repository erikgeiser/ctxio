//go:build darwin || freebsd || netbsd || openbsd || solaris

package ctxio

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// NewSelectCancelReader returns a reader and a cancel function. If the input
// reader is an *os.File, the cancel function can be used to interrupt a
// blocking call read call. In this case, the cancel function returns true if
// the call was cancelled successfully. If the input reader is not a *os.File or
// the file descriptor is 1024 or larger, the cancel function does nothing and
// always returns false. The generic unix implementation is based on the posix
// select syscall.
func NewSelectCancelReader(reader io.Reader) (CancelableReadWriteCloser, error) {
	file, ok := reader.(*os.File)
	if !ok || file.Fd() >= unix.FD_SETSIZE {
		return nil, fmt.Errorf("file descriptor %d exceeds maximum of %d", file.Fd(), unix.FD_SETSIZE)
	}
	r := &selectCancelReader{file: file}

	var err error

	r.cancelSignalReader, r.cancelSignalWriter, err = os.Pipe()
	if err != nil {
		return nil, err
	}

	if r.cancelSignalReader.Fd() >= unix.FD_SETSIZE {
		return nil, fmt.Errorf("cancelation pipe file selector %d exceeds maximum of %d",
			r.cancelSignalReader.Fd(), unix.FD_SETSIZE)
	}

	return r, nil
}

type selectCancelReader struct {
	file               *os.File
	cancelSignalReader *os.File
	cancelSignalWriter *os.File
	cancelSignalBuffer [1]byte
}

func (r *selectCancelReader) File() File {
	return r.file
}

func (r *selectCancelReader) ReadContext(ctx context.Context, data []byte) (int, error) {
	done := cancelWhenContextIsDone(ctx, r)
	defer done()

	return r.Read(data)
}

func (r *selectCancelReader) Read(data []byte) (int, error) {
	for {
		err := waitForRead(r.file, r.cancelSignalReader)
		if err != nil {
			if errors.Is(err, unix.EINTR) {
				continue // try again if the syscall was interrupted
			}

			if errors.Is(err, ErrCanceled) {
				// remove signal from pipe
				_, readErr := r.cancelSignalReader.Read(r.cancelSignalBuffer[:])
				if readErr != nil {
					return 0, fmt.Errorf("reading cancel signal: %w", readErr)
				}
			}

			return 0, err
		}

		return r.file.Read(data)
	}
}

func (r *selectCancelReader) WriteContext(ctx context.Context, data []byte) (int, error) {
	done := cancelWhenContextIsDone(ctx, r)
	defer done()

	return r.Write(data)
}

func (r *selectCancelReader) Write(data []byte) (int, error) {
	for {
		err := waitForWrite(r.file, r.cancelSignalReader)
		if err != nil {
			if errors.Is(err, unix.EINTR) {
				continue // try again if the syscall was interrupted
			}

			if errors.Is(err, ErrCanceled) {
				// remove signal from pipe
				_, readErr := r.cancelSignalReader.Read(r.cancelSignalBuffer[:])
				if readErr != nil {
					return 0, fmt.Errorf("reading cancel signal: %w", readErr)
				}
			}

			return 0, err
		}

		return r.file.Write(data)
	}
}

func (r *selectCancelReader) Cancel() bool {
	_, err := r.cancelSignalWriter.Write(r.cancelSignalBuffer[:])
	return err == nil
}

func (r *selectCancelReader) Close() error {
	var errMsgs []string

	// close pipe
	err := r.cancelSignalWriter.Close()
	if err != nil {
		errMsgs = append(errMsgs, fmt.Sprintf("closing cancel signal writer: %v", err))
	}

	err = r.cancelSignalReader.Close()
	if err != nil {
		errMsgs = append(errMsgs, fmt.Sprintf("closing cancel signal reader: %v", err))
	}

	if len(errMsgs) > 0 {
		return fmt.Errorf(strings.Join(errMsgs, ", "))
	}

	return nil
}

func waitForRead(reader *os.File, abort *os.File) error {
	readerFd := int(reader.Fd())
	abortFd := int(abort.Fd())

	maxFd := readerFd
	if abortFd > maxFd {
		maxFd = abortFd
	}

	readFdSet := &unix.FdSet{}
	readFdSet.Set(int(reader.Fd()))
	readFdSet.Set(int(abort.Fd()))

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

func waitForWrite(writer *os.File, abort *os.File) error {
	writerFd := int(writer.Fd())
	abortFd := int(abort.Fd())

	maxFd := writerFd
	if abortFd > maxFd {
		maxFd = abortFd
	}

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
