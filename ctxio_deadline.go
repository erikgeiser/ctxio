package ctxio

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync/atomic"
	"time"
)

// Deadliner is implemented by types that support deadline-based I/O, such as
// *os.File (for pollable file descriptors) and net.Conn.
type Deadliner interface {
	io.ReadWriter
	SetReadDeadline(t time.Time) error
	SetWriteDeadline(t time.Time) error
}

// Verify that common types satisfy Deadliner.
var (
	_ Deadliner = (*os.File)(nil)
	_ Deadliner = (*net.TCPConn)(nil)
)

// WrapDeadliner returns a ContextIO wrapping any value that implements the
// Deadliner interface. This is useful for types that are not an *os.File or
// net.Conn but still support deadline-based I/O (e.g. TLS connections, QUIC
// streams). Returns an error if the value does not actually support deadlines
// at runtime.
func WrapDeadliner(d Deadliner, name string) (ContextIO, error) {
	err := d.SetReadDeadline(time.Time{})
	if err != nil {
		return nil, fmt.Errorf("%T: %w (SetReadDeadline test failed: %w)", d, ErrNotSupported, err)
	}

	return &deadlineIO{d: d, name: name}, nil
}

// deadlineIO implements ContextIO using deadline-based cancelation. This works
// for any type that supports SetReadDeadline/SetWriteDeadline, including
// net.Conn and *os.File backed by pollable file descriptors (sockets, pipes,
// etc. — but not regular files).
type deadlineIO struct {
	d    Deadliner
	name string

	readCanceled  atomic.Bool
	writeCanceled atomic.Bool
}

func (r *deadlineIO) Name() string { return r.name }

func (r *deadlineIO) ReadContext(ctx context.Context, data []byte) (int, error) {
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

func (r *deadlineIO) Read(data []byte) (int, error) {
	if r.readCanceled.Load() {
		return 0, ErrCanceled
	}

	return r.read(data)
}

func (r *deadlineIO) read(data []byte) (int, error) {
	_ = r.d.SetReadDeadline(time.Time{})

	n, err := r.d.Read(data)
	if err != nil && isTimeoutError(err) && r.readCanceled.Load() {
		return 0, ErrCanceled
	}

	return n, err
}

func (r *deadlineIO) WriteContext(ctx context.Context, data []byte) (int, error) {
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

func (r *deadlineIO) Write(data []byte) (int, error) {
	if r.writeCanceled.Load() {
		return 0, ErrCanceled
	}

	return r.write(data)
}

func (r *deadlineIO) write(data []byte) (int, error) {
	_ = r.d.SetWriteDeadline(time.Time{})

	n, err := r.d.Write(data)
	if err != nil && isTimeoutError(err) && r.writeCanceled.Load() {
		return 0, ErrCanceled
	}

	return n, err
}

func (r *deadlineIO) CancelReads() {
	r.readCanceled.Store(true)
	_ = r.d.SetReadDeadline(time.Now())
}

func (r *deadlineIO) CancelWrites() {
	r.writeCanceled.Store(true)
	_ = r.d.SetWriteDeadline(time.Now())
}

func (r *deadlineIO) Cancel() {
	r.CancelReads()
	r.CancelWrites()
}

func (r *deadlineIO) ResetReader() {
	r.readCanceled.Store(false)
	_ = r.d.SetReadDeadline(time.Time{})
}

func (r *deadlineIO) ResetWriter() {
	r.writeCanceled.Store(false)
	_ = r.d.SetWriteDeadline(time.Time{})
}

func (r *deadlineIO) Reset() {
	r.ResetReader()
	r.ResetWriter()
}

func (r *deadlineIO) Close() error {
	r.Cancel()

	closer, ok := r.d.(io.Closer)
	if ok {
		return closer.Close()
	}

	return nil
}

// isTimeoutError reports whether err is a timeout error (e.g. from an expired
// deadline). Both net.Conn and *os.File deadline errors satisfy net.Error.
func isTimeoutError(err error) bool {
	if netErr, ok := errors.AsType[net.Error](err); ok {
		return netErr.Timeout()
	}

	return false
}
