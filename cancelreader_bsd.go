//go:build darwin || freebsd || netbsd || openbsd

package ctxio

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// NewkqueueCancelReader returns a reader and a cancel function. If the input
// reader is a File, the cancel function can be used to interrupt a blocking
// call read call. In this case, the cancel function returns true if the call
// was cancelled successfully. The macOS and *BSD implementation is based on the
// kqueue mechanism with a fallback to the cancel mechanisms when File is
// "/dev/tty".
func NewCancelReader(file File) (CancelableReadWriteCloser, error) {
	if int(file.Fd()) == -1 {
		return nil, fmt.Errorf("unsuitable file: %w", os.ErrClosed)
	}

	// kqueue returns instantly when polling /dev/tty so fallback to select
	if file.Name() == "/dev/tty" {
		return NewSelectCancelReader(file)
	}

	kQueue, err := unix.Kqueue()
	if err != nil {
		return nil, fmt.Errorf("create kqueue: %w", err)
	}

	r := &kqueueCancelReader{
		file:   file,
		kQueue: kQueue,
	}

	r.cancelSignalReader, r.cancelSignalWriter, err = os.Pipe()
	if err != nil {
		_ = unix.Close(kQueue)

		return nil, err
	}

	unix.SetKevent(&r.readEvent, int(file.Fd()), unix.EVFILT_READ, unix.EV_ADD)
	unix.SetKevent(&r.writeEvent, int(file.Fd()), unix.EVFILT_WRITE, unix.EV_ADD)
	unix.SetKevent(&r.cancelevent, int(r.cancelSignalReader.Fd()), unix.EVFILT_READ, unix.EV_ADD)

	return r, nil
}

type kqueueCancelReader struct {
	file               File
	cancelSignalReader *os.File
	cancelSignalWriter *os.File
	kQueue             int
	writeEvent         unix.Kevent_t
	readEvent          unix.Kevent_t
	cancelevent        unix.Kevent_t
	cancelEventBuffer  [1]byte
}

func (r *kqueueCancelReader) File() File {
	return r.file
}

func (r *kqueueCancelReader) ReadContext(ctx context.Context, data []byte) (int, error) {
	done := cancelWhenContextIsDone(ctx, r)
	defer done()

	return r.Read(data)
}

func (r *kqueueCancelReader) Read(data []byte) (int, error) {
	bytesAvailable, err := r.wait(r.readEvent)
	if err != nil {
		if errors.Is(err, ErrCanceled) {
			// remove signal from pipe
			_, errRead := r.cancelSignalReader.Read(r.cancelEventBuffer[:])
			if errRead != nil {
				return 0, fmt.Errorf("reading cancel signal: %w", errRead)
			}
		}

		return 0, err
	}

	// if the kevent did not return a sensible amount of available bytes, pass
	// the whole byte byte array and let the File implementation decide how much
	// to read
	if bytesAvailable < 1 {
		bytesAvailable = int64(len(data))
	}

	return r.file.Read(data[:bytesAvailable])
}

func (r *kqueueCancelReader) WriteContext(ctx context.Context, data []byte) (int, error) {
	done := cancelWhenContextIsDone(ctx, r)
	defer done()

	return r.Write(data)
}

func (r *kqueueCancelReader) Write(data []byte) (int, error) {
	bytesWritten := 0

	for bytesWritten < len(data) {
		bufferSize, err := r.wait(r.writeEvent)
		if err != nil {
			if errors.Is(err, ErrCanceled) {
				// remove signal from pipe
				_, errRead := r.cancelSignalReader.Read(r.cancelEventBuffer[:])
				if errRead != nil {
					return 0, fmt.Errorf("reading cancel signal: %w", errRead)
				}
			}

			return 0, err
		}

		// kqueue does not return buffer size of regular files and instead
		// returns the only safe size to write which is 1. However, writing one
		// byte at a time is incredibly slow, so we go for the risky full read
		// instead.
		if _, ok := r.file.(*os.File); ok && bytesWritten == 0 && bufferSize < 2 {
			bufferSize = int64(len(data))
		}

		maxOffset := bytesWritten + int(bufferSize)
		if maxOffset > len(data) {
			maxOffset = len(data)
		}

		fmt.Printf("len(data)=%d: writing from %d to %d (%d)\n", len(data), bytesWritten, maxOffset, bufferSize)
		n, err := r.file.Write(data[bytesWritten:maxOffset])
		if err != nil {
			return bytesWritten + n, err
		}

		bytesWritten += n
	}

	return bytesWritten, nil
}

func (r *kqueueCancelReader) Cancel() bool {
	_, err := r.cancelSignalWriter.Write(r.cancelEventBuffer[:]) // send cancel signal
	return err == nil
}

func (r *kqueueCancelReader) Close() error {
	var errMsgs []string

	// close kqueue
	err := unix.Close(r.kQueue)
	if err != nil {
		errMsgs = append(errMsgs, fmt.Sprintf("closing kqueue: %v", err))
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

func (r *kqueueCancelReader) wait(event unix.Kevent_t) (int64, error) {
	activeEvent := make([]unix.Kevent_t, 1)

	for {
		_, err := unix.Kevent(r.kQueue, []unix.Kevent_t{event, r.cancelevent}, activeEvent, nil)
		if errors.Is(err, unix.EINTR) {
			continue // try again if the syscall was interrupted
		}

		if err != nil {
			return 0, fmt.Errorf("kevent: %w", err)
		}

		break
	}

	switch activeEvent[0].Ident {
	case uint64(r.file.Fd()):
		return activeEvent[0].Data, nil
	case uint64(r.cancelSignalReader.Fd()):
		return 0, ErrCanceled
	}

	return 0, fmt.Errorf(
		"kevent identifier %d does not match primary file descriptor %d "+
			"or cancelation pipe file descriptor %d",
		activeEvent[0].Ident, r.file.Fd(), r.cancelSignalReader.Fd())
}
