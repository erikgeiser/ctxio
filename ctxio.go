package ctxio

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"syscall"
)

var (
	// ErrCanceled is returned when an I/O operation was canceled.
	ErrCanceled = fmt.Errorf("I/O operation canceled")
	// ErrNotSupported is returned when wrapping an object that is not supported
	// by ctxio.
	ErrNotSupported = fmt.Errorf("not supported")
)

// ContextIO provides context-aware, cancelable I/O operations for objects like
// files, sockets, pipes and more.
type ContextIO interface {
	// ReadWriteCloser is the interface for interfacing with APIs that are not
	// context aware. In this case, cancelation can be achieved explicitly using
	// the Cancel* and Reset* methods. It is recommended not to mix IO using the
	// Read()/Write() API and the explicit cancelation methods with the
	// context-aware IO methods.
	io.ReadWriteCloser

	// ReadContext is like Read but it returns ErrCanceled as soon as the
	// context expires. Calling it with a fresh context resets the ContextIO
	// reader. It is recommended not to mix IO using the Read()/Write() API and
	// the explicit cancelation methods with the context-aware IO methods.
	ReadContext(ctx context.Context, buffer []byte) (int, error)
	// WriteContext is like Write but it returns ErrCanceled as soon as the
	// context expires. Calling it with a fresh context resets the ContextIO
	// writer. It is recommended not to mix IO using the Read()/Write() API and
	// the explicit cancelation methods with the context-aware IO methods.
	WriteContext(ctx context.Context, buffer []byte) (int, error)

	// Name returns the name of object wrapped by the ContextIO.
	Name() string

	// Cancel cancels any ongoing and future Read and Write calls. All
	// subsequent calls to Read/Write will return ErrCanceled until Reset is
	// called. ReadContext/WriteContext are not affected: they clear the
	// canceled state automatically before starting a new operation.
	// It is equivalent to calling both CancelReads and CancelWrites.
	Cancel()
	// CancelReads cancels any ongoing and future Read calls. All subsequent
	// calls to Read will return ErrCanceled until ResetReader is called.
	// ReadContext is not affected as it clears the canceled state automatically
	// before starting a new operation when a fresh context is provided.
	CancelReads()
	// CancelWrites cancels any ongoing and future Write calls. All subsequent
	// calls to Write will return ErrCanceled until ResetWriter is called.
	// WriteContext is not affected as it clears the canceled state
	// automatically before starting a new operation when a fresh context is
	// provided.
	CancelWrites()

	// ResetReader clears the read cancellation state, allowing subsequent Reads
	// to proceed. Must not be called while a Read is in progress.
	ResetReader()
	// ResetWriter clears the write cancellation state, allowing subsequent
	// Writes to proceed. Must not be called while a Write is in progress.
	ResetWriter()
	// Reset clears both read and write cancellation state. Reset must not be
	// called while a Read or Write is in progress.
	Reset()
}

type syscallConner interface {
	SyscallConn() (syscall.RawConn, error)
}

var (
	_ syscallConner = &net.TCPConn{}
	_ syscallConner = &net.UDPConn{}
	_ syscallConner = &net.IPConn{}
	_ syscallConner = &net.UnixConn{}
)

func connName(conn net.Conn) string {
	connType := fmt.Sprintf("%T", conn)
	connType = strings.TrimPrefix(connType, "*")
	connType = strings.TrimPrefix(connType, "net.")
	connType = strings.TrimSuffix(connType, "{}")

	link := conn.LocalAddr().String()
	if conn.RemoteAddr() != nil && conn.RemoteAddr().String() != "" {
		link += "<->" + conn.RemoteAddr().String()
	}

	return fmt.Sprintf("%s(%s)", connType, link)
}

func cancelWhenContextIsDone(ctx context.Context, cancel func()) func() {
	wg := sync.WaitGroup{}
	operationFinished := make(chan struct{})

	wg.Go(func() {
		select {
		case <-ctx.Done():
			cancel()
		case <-operationFinished:
			return
		}
	})

	return func() {
		close(operationFinished)
		wg.Wait()
	}
}

func joinErrors(errs ...error) error {
	if len(errs) == 0 {
		return fmt.Errorf("no errors passed to joinErrors()")
	}

	errsSlice := make([]any, 0, len(errs))

	for _, err := range errs {
		errsSlice = append(errsSlice, err)
	}

	formatter := strings.Repeat("%w, ", len(errs))

	return fmt.Errorf(strings.TrimRight(formatter, ", "), errsSlice...)
}
