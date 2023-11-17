//go:build linux

package ctxio

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// NewCancelReader returns a reader and a cancel function. If the input reader
// is an *os.File, the cancel function can be used to interrupt a blocking call
// read call. In this case, the cancel function returns true if the call was
// cancelled successfully. If the input reader is not a *os.File, the cancel
// function does nothing and always returns false. The linux implementation is
// based on the epoll mechanism.
func NewCancelReader(file File) (CancelableReadWriteCloser, error) {
	if int(file.Fd()) == -1 {
		return nil, fmt.Errorf("unsuitable file: %w", os.ErrClosed)
	}

	return NewIOUringCancelReader(file)

	readEpoll, err := unix.EpollCreate1(0)
	if err != nil {
		return nil, fmt.Errorf("create read epoll: %w", err)
	}

	writeEpoll, err := unix.EpollCreate1(0)
	if err != nil {
		return nil, fmt.Errorf("create write epoll: %w", err)
	}

	cancelSignalReader, cancelSignalWriter, err := os.Pipe()
	if err != nil {
		_ = unix.Close(readEpoll)

		return nil, err
	}

	r := &epollCancelReader{
		file:               file,
		readEpoll:          readEpoll,
		writeEpoll:         writeEpoll,
		cancelSignalReader: cancelSignalReader,
		cancelSignalWriter: cancelSignalWriter,
	}

	err = unix.EpollCtl(readEpoll, unix.EPOLL_CTL_ADD, int(file.Fd()), &unix.EpollEvent{
		Events: unix.EPOLLIN,
		Fd:     int32(file.Fd()),
	})
	if err != nil {
		_ = unix.Close(readEpoll)
		_ = cancelSignalReader.Close()
		_ = cancelSignalWriter.Close()

		return nil, fmt.Errorf("add reader with descriptor %d to epoll interest list: %w",
			int(file.Fd()), err)
	}

	err = unix.EpollCtl(writeEpoll, unix.EPOLL_CTL_ADD, int(file.Fd()), &unix.EpollEvent{
		Events: unix.EPOLLET,
		Fd:     int32(file.Fd()),
	})
	if err != nil {
		_ = unix.Close(readEpoll)
		_ = cancelSignalReader.Close()
		_ = cancelSignalWriter.Close()

		return nil, fmt.Errorf("add writer with descriptor %d to epoll interest list: %w",
			int(file.Fd()), err)
	}

	err = unix.EpollCtl(readEpoll, unix.EPOLL_CTL_ADD, int(r.cancelSignalReader.Fd()), &unix.EpollEvent{
		Events: unix.EPOLLIN,
		Fd:     int32(r.cancelSignalReader.Fd()),
	})
	if err != nil {
		_ = unix.Close(readEpoll)
		_ = cancelSignalReader.Close()
		_ = cancelSignalWriter.Close()

		return nil, fmt.Errorf("add cancel signal with descriptor %d to epoll interest list: %w",
			int(r.cancelSignalReader.Fd()), err)
	}

	err = unix.EpollCtl(writeEpoll, unix.EPOLL_CTL_ADD, int(r.cancelSignalReader.Fd()), &unix.EpollEvent{
		Events: unix.EPOLLIN,
		Fd:     int32(r.cancelSignalReader.Fd()),
	})
	if err != nil {
		_ = unix.Close(readEpoll)
		_ = cancelSignalReader.Close()
		_ = cancelSignalWriter.Close()

		return nil, fmt.Errorf("add cancel signal with descriptor %d to epoll interest list: %w",
			int(r.cancelSignalReader.Fd()), err)
	}

	return r, nil
}

type epollCancelReader struct {
	file               File
	cancelSignalReader *os.File
	cancelSignalWriter *os.File
	cancelSignalBuffer [1]byte
	readEpoll          int
	writeEpoll         int
}

func (r *epollCancelReader) File() File {
	return r.file
}

func (r *epollCancelReader) ReadContext(ctx context.Context, data []byte) (int, error) {
	done := cancelWhenContextIsDone(ctx, r)
	defer done()

	return r.Read(data)
}

func (r *epollCancelReader) Read(data []byte) (int, error) {
	err := wait(r.readEpoll, r.file, r.cancelSignalReader)
	if err != nil {
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

func (r *epollCancelReader) WriteContext(ctx context.Context, data []byte) (int, error) {
	done := cancelWhenContextIsDone(ctx, r)
	defer done()

	return r.Write(data)
}

func (r *epollCancelReader) Write(data []byte) (int, error) {
	err := wait(r.writeEpoll, r.file, r.cancelSignalReader)
	if err != nil {
		if errors.Is(err, ErrCanceled) {
			// remove signal from pipe
			var b [1]byte
			_, readErr := r.cancelSignalReader.Read(b[:])
			if readErr != nil {
				return 0, fmt.Errorf("reading cancel signal: %w", readErr)
			}
		}

		return 0, err
	}

	return r.file.Write(data)
}

func (r *epollCancelReader) Cancel() bool {
	// send cancel signal
	_, err := r.cancelSignalWriter.Write(r.cancelSignalBuffer[:])
	return err == nil
}

func (r *epollCancelReader) Close() error {
	var errMsgs []string

	// close kqueue
	err := unix.Close(r.readEpoll)
	if err != nil {
		errMsgs = append(errMsgs, fmt.Sprintf("closing epoll: %v", err))
	}

	// close pipe
	err = r.cancelSignalWriter.Close()
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

func wait(epoll int, f File, cancelSignalReader *os.File) error {
	events := make([]unix.EpollEvent, 1)

	for {
		_, err := unix.EpollWait(epoll, events, -1)
		if errors.Is(err, unix.EINTR) {
			continue // try again if the syscall was interrupted
		}

		if err != nil {
			return fmt.Errorf("epoll wait: %w", err)
		}

		break
	}

	switch events[0].Fd {
	case int32(f.Fd()):
		return nil
	case int32(cancelSignalReader.Fd()):
		return ErrCanceled
	}

	return fmt.Errorf("epoll_wait returned event with file descriptor %d "+
		"instead of primary file descriptor %d or cancelation pipe file descriptor %d",
		events[0].Fd, f.Fd(), cancelSignalReader.Fd())
}
