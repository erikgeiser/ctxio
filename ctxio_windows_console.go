//go:build windows

package ctxio

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync/atomic"
	"syscall"
	"unicode/utf16"

	"golang.org/x/sys/windows"
)

// WrapConsole returns a ContextIO wrapping the given *os.File as a console
// handle. If the file is os.Stdin, os.Stdout, or os.Stderr, the corresponding
// CONIN$ or CONOUT$ is opened. Otherwise the file is treated as an
// already-opened console handle.
func WrapConsole(file *os.File) (ContextIO, error) {
	handle := windows.Handle(file.Fd())

	var mode uint32
	if syscall.GetConsoleMode(syscall.Handle(handle), &mode) != nil {
		return nil, fmt.Errorf("handle is not a console")
	}

	switch file {
	case os.Stdin:
		return openConsoleReader()
	case os.Stdout, os.Stderr:
		return openConsoleWriter()
	default:
		return newWinConsoleIO(file, handle)
	}
}

// Console opens both CONIN$ and CONOUT$ and returns a single ContextIO that
// reads from CONIN$ and writes to CONOUT$.
func Console() (ContextIO, error) {
	reader, err := openConsoleReader()
	if err != nil {
		return nil, fmt.Errorf("open console reader: %w", err)
	}

	conout, err := openConout()
	if err != nil {
		_ = reader.Close()

		return nil, fmt.Errorf("open console writer: %w", err)
	}

	reader.writer = conout
	reader.ownWriter = true

	return reader, nil
}

func openConsoleReader() (*winConsoleIO, error) {
	conin, err := windows.CreateFile(
		&(utf16.Encode([]rune("CONIN$\x00"))[0]), windows.GENERIC_READ|windows.GENERIC_WRITE,
		fileShareValidFlags, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OVERLAPPED, 0)
	if err != nil {
		return nil, fmt.Errorf("open CONIN$: %w", err)
	}

	resetConsole, err := prepareConsole(conin)
	if err != nil {
		_ = windows.Close(conin)

		return nil, fmt.Errorf("prepare console: %w", err)
	}

	err = flushConsoleInputBuffer(conin)
	if err != nil {
		_ = windows.Close(conin)

		return nil, fmt.Errorf("flush console input buffer: %w", err)
	}

	readCancelEvent, err := windows.CreateEvent(nil, 0, 0, nil)
	if err != nil {
		_ = windows.Close(conin)

		return nil, fmt.Errorf("create read cancel event: %w", err)
	}

	writeCancelEvent, err := windows.CreateEvent(nil, 0, 0, nil)
	if err != nil {
		_ = windows.Close(conin)
		_ = windows.CloseHandle(readCancelEvent)

		return nil, fmt.Errorf("create write cancel event: %w", err)
	}

	return &winConsoleIO{
		conin:            conin,
		readCancelEvent:  readCancelEvent,
		writeCancelEvent: writeCancelEvent,
		resetConsole:     resetConsole,
		name:             "CONIN$",
	}, nil
}

func openConout() (io.WriteCloser, error) {
	conout, err := windows.CreateFile(
		&(utf16.Encode([]rune("CONOUT$\x00"))[0]), windows.GENERIC_WRITE,
		fileShareValidFlags, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return nil, fmt.Errorf("open CONOUT$: %w", err)
	}

	return os.NewFile(uintptr(conout), "CONOUT$"), nil
}

func openConsoleWriter() (*winConsoleIO, error) {
	conout, err := openConout()
	if err != nil {
		return nil, err
	}

	readCancelEvent, err := windows.CreateEvent(nil, 0, 0, nil)
	if err != nil {
		_ = conout.Close()

		return nil, fmt.Errorf("create read cancel event: %w", err)
	}

	writeCancelEvent, err := windows.CreateEvent(nil, 0, 0, nil)
	if err != nil {
		_ = conout.Close()
		_ = windows.CloseHandle(readCancelEvent)

		return nil, fmt.Errorf("create write cancel event: %w", err)
	}

	return &winConsoleIO{
		writer:           conout,
		ownWriter:        true,
		readCancelEvent:  readCancelEvent,
		writeCancelEvent: writeCancelEvent,
		name:             "CONOUT$",
	}, nil
}

func newWinConsoleIO(file *os.File, handle windows.Handle) (*winConsoleIO, error) {
	if m := uint32(0); syscall.GetConsoleMode(syscall.Handle(handle), &m) != nil {
		return nil, fmt.Errorf("handle is not a console")
	}

	// Open CONIN$ in overlapped mode so we can use WaitForMultipleObjects.
	conin, err := windows.CreateFile(
		&(utf16.Encode([]rune("CONIN$\x00"))[0]), windows.GENERIC_READ|windows.GENERIC_WRITE,
		fileShareValidFlags, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OVERLAPPED, 0)
	if err != nil {
		return nil, fmt.Errorf("open CONIN$ in overlapping mode: %w", err)
	}

	resetConsole, err := prepareConsole(conin)
	if err != nil {
		return nil, fmt.Errorf("prepare console: %w", err)
	}

	// Flush stale input that could trigger WaitForMultipleObjects without
	// ReadFile being able to consume it.
	err = flushConsoleInputBuffer(conin)
	if err != nil {
		return nil, fmt.Errorf("flush console input buffer: %w", err)
	}

	readCancelEvent, err := windows.CreateEvent(nil, 0, 0, nil)
	if err != nil {
		return nil, fmt.Errorf("create read cancel event: %w", err)
	}

	writeCancelEvent, err := windows.CreateEvent(nil, 0, 0, nil)
	if err != nil {
		_ = windows.CloseHandle(readCancelEvent)

		return nil, fmt.Errorf("create write cancel event: %w", err)
	}

	return &winConsoleIO{
		file:             file,
		writer:           file,
		conin:            conin,
		readCancelEvent:  readCancelEvent,
		writeCancelEvent: writeCancelEvent,
		resetConsole:     resetConsole,
		name:             file.Name(),
	}, nil
}

type winConsoleIO struct {
	file             *os.File  // original file passed by user (nil when we opened CONIN$/CONOUT$ ourselves)
	writer           io.Writer // for writes (may be CONOUT$ or the original file)
	ownWriter        bool      // true if we opened the writer and must close it
	conin            windows.Handle
	name             string
	readCancelEvent  windows.Handle
	writeCancelEvent windows.Handle
	readCanceled     atomic.Bool
	writeCanceled    atomic.Bool

	resetConsole func() error
}

func (r *winConsoleIO) Name() string { return r.name }

func (r *winConsoleIO) ReadContext(ctx context.Context, data []byte) (int, error) {
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

func (r *winConsoleIO) Read(data []byte) (int, error) {
	if r.readCanceled.Load() {
		return 0, ErrCanceled
	}

	return r.read(data)
}

func (r *winConsoleIO) read(data []byte) (int, error) {
	if r.conin == 0 {
		return 0, fmt.Errorf("console not opened for reading")
	}

	for {
		err := waitForMultipleObjects(r.conin, r.readCancelEvent)
		if err != nil {
			return 0, err
		}

		if r.readCanceled.Load() {
			return 0, ErrCanceled
		}

		n, err := overlappedRead(r.conin, data)
		if errors.Is(err, windows.ERROR_OPERATION_ABORTED) {
			if r.readCanceled.Load() {
				return 0, ErrCanceled
			}

			// The OS aborted the read because a non-character console event
			// (e.g. focus change, window resize) was consumed. Loop back and
			// wait for real input.
			continue
		}

		if n > 0 || err != nil {
			return n, err
		}

		// n == 0, err == nil: ReadFile consumed a non-character console event.
		// Loop back and wait for real input.
	}
}

func (r *winConsoleIO) WriteContext(ctx context.Context, data []byte) (int, error) {
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

func (r *winConsoleIO) Write(data []byte) (int, error) {
	if r.writeCanceled.Load() {
		return 0, ErrCanceled
	}

	return r.write(data)
}

// write delegates to the writer — console output does not block meaningfully.
func (r *winConsoleIO) write(data []byte) (int, error) {
	if r.writer == nil {
		return 0, fmt.Errorf("console not opened for writing")
	}

	return r.writer.Write(data)
}

func (r *winConsoleIO) CancelReads() {
	r.readCanceled.Store(true)
	_ = windows.SetEvent(r.readCancelEvent)

	if r.conin != 0 {
		_ = windows.CancelIoEx(r.conin, nil)
	}
}

func (r *winConsoleIO) CancelWrites() {
	r.writeCanceled.Store(true)
}

func (r *winConsoleIO) Cancel() {
	r.CancelReads()
	r.CancelWrites()
}

func (r *winConsoleIO) ResetReader() {
	r.readCanceled.Store(false)
	_ = windows.ResetEvent(r.readCancelEvent)
}

func (r *winConsoleIO) ResetWriter() {
	r.writeCanceled.Store(false)
}

func (r *winConsoleIO) Reset() {
	r.ResetReader()
	r.ResetWriter()
}

func (r *winConsoleIO) Close() error {
	r.Cancel()

	var errs []error

	err := windows.CloseHandle(r.readCancelEvent)
	if err != nil {
		errs = append(errs, fmt.Errorf("closing read cancel event: %w", err))
	}

	err = windows.CloseHandle(r.writeCancelEvent)
	if err != nil {
		errs = append(errs, fmt.Errorf("closing write cancel event: %w", err))
	}

	if r.resetConsole != nil {
		err := r.resetConsole()
		if err != nil {
			errs = append(errs, fmt.Errorf("resetting console mode: %w", err))
		}
	}

	if r.conin != 0 {
		err := windows.Close(r.conin)
		if err != nil {
			errs = append(errs, fmt.Errorf("closing CONIN$: %w", err))
		}
	}

	if r.ownWriter {
		if c, ok := r.writer.(io.Closer); ok {
			err := c.Close()
			if err != nil {
				errs = append(errs, fmt.Errorf("closing CONOUT$: %w", err))
			}
		}
	}

	// close the original file if one was passed by the user
	if r.file != nil {
		err := r.file.Close()
		if err != nil {
			errs = append(errs, fmt.Errorf("closing underlying file: %w", err))
		}
	}

	if len(errs) > 0 {
		return joinErrors(errs...)
	}

	return nil
}

func overlappedRead(handle windows.Handle, data []byte) (int, error) {
	hevent, err := windows.CreateEvent(nil, 0, 0, nil)
	if err != nil {
		return 0, fmt.Errorf("create event: %w", err)
	}
	defer windows.CloseHandle(hevent)

	overlapped := windows.Overlapped{HEvent: hevent}

	var n uint32

	err = windows.ReadFile(handle, data, &n, &overlapped)
	if err == nil {
		return int(n), nil
	}

	if !errors.Is(err, windows.ERROR_IO_PENDING) {
		return int(n), err
	}

	err = windows.GetOverlappedResult(handle, &overlapped, &n, true)
	if err != nil {
		return int(n), err
	}

	return int(n), nil
}

func prepareConsole(input windows.Handle) (reset func() error, err error) {
	var originalMode uint32

	err = windows.GetConsoleMode(input, &originalMode)
	if err != nil {
		return nil, fmt.Errorf("get console mode: %w", err)
	}

	newMode := originalMode
	// ENABLE_ECHO_INPUT is only valid with ENABLE_LINE_INPUT
	newMode &^= windows.ENABLE_LINE_INPUT
	newMode &^= windows.ENABLE_ECHO_INPUT

	// ENABLE_EXTENDED_FLAGS must already be set in the original mode before we
	// can clear ENABLE_QUICK_EDIT_MODE — setting ENABLE_EXTENDED_FLAGS in
	// isolation (e.g. on a freshly allocated console) causes
	// ERROR_INVALID_PARAMETER. If it was already set, quick-edit is active and
	// we need to clear it. If it was not set, quick-edit is not in effect.
	if originalMode&windows.ENABLE_EXTENDED_FLAGS != 0 {
		newMode &^= windows.ENABLE_QUICK_EDIT_MODE
	}

	err = windows.SetConsoleMode(input, newMode)
	if err != nil {
		return nil, fmt.Errorf("set console mode: %w", err)
	}

	return func() error {
		err := windows.SetConsoleMode(input, originalMode)
		if err != nil {
			return fmt.Errorf("reset console mode: %w", err)
		}

		return nil
	}, nil
}
