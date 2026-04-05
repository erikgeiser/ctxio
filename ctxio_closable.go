package ctxio

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
)

// WrapClosableReader returns a ContextIO wrapping an io.ReadCloser using
// close-based cancellation. Cancellation works by closing the underlying
// object, which means the object cannot be reused after cancellation.
// Reset clears the cancellation flag but does not reopen the object.
// Write and WriteContext always return ErrNotSupported.
func WrapClosableReader(rc io.ReadCloser, name string) (ContextIO, error) {
	return &closeIO{
		r:      rc,
		closer: rc,
		name:   name,
	}, nil
}

// WrapClosableWriter returns a ContextIO wrapping an io.WriteCloser using
// close-based cancellation. Cancellation works by closing the underlying
// object, which means the object cannot be reused after cancellation.
// Reset clears the cancellation flag but does not reopen the object.
// Read and ReadContext always return ErrNotSupported.
func WrapClosableWriter(wc io.WriteCloser, name string) (ContextIO, error) {
	return &closeIO{
		w:      wc,
		closer: wc,
		name:   name,
	}, nil
}

// WrapClosableReadWriter returns a ContextIO wrapping an io.ReadWriteCloser
// using close-based cancellation. Cancellation works by closing the underlying
// object, which means the object cannot be reused after cancellation.
// Reset clears the cancellation flag but does not reopen the object.
func WrapClosableReadWriter(rwc io.ReadWriteCloser, name string) (ContextIO, error) {
	return &closeIO{
		r:      rwc,
		w:      rwc,
		closer: rwc,
		name:   name,
	}, nil
}

// closeIO implements ContextIO using close-based cancellation. When a
// cancellation is requested, the underlying object is closed, which unblocks
// any in-flight Read or Write. Because closing is destructive, the object
// cannot be reused after cancellation — Reset clears the cancellation flag
// but does not reopen the underlying object.
type closeIO struct {
	r      io.Reader // nil if writer-only
	w      io.Writer // nil if reader-only
	closer io.Closer
	name   string

	mu            sync.Mutex
	closed        bool
	readCanceled  atomic.Bool
	writeCanceled atomic.Bool
}

func (c *closeIO) Name() string { return c.name }

func (c *closeIO) ReadContext(ctx context.Context, data []byte) (int, error) {
	if ctx.Err() != nil {
		return 0, ErrCanceled
	}

	if c.readCanceled.Load() {
		return 0, ErrCanceled
	}

	done := cancelWhenContextIsDone(ctx, c.CancelReads)
	defer done()

	return c.read(data)
}

func (c *closeIO) Read(data []byte) (int, error) {
	if c.readCanceled.Load() {
		return 0, ErrCanceled
	}

	return c.read(data)
}

func (c *closeIO) read(data []byte) (int, error) {
	if c.r == nil {
		return 0, ErrNotSupported
	}

	n, err := c.r.Read(data)
	if err != nil && c.readCanceled.Load() {
		return 0, ErrCanceled
	}

	return n, err
}

func (c *closeIO) WriteContext(ctx context.Context, data []byte) (int, error) {
	if ctx.Err() != nil {
		return 0, ErrCanceled
	}

	if c.writeCanceled.Load() {
		return 0, ErrCanceled
	}

	done := cancelWhenContextIsDone(ctx, c.CancelWrites)
	defer done()

	return c.write(data)
}

func (c *closeIO) Write(data []byte) (int, error) {
	if c.writeCanceled.Load() {
		return 0, ErrCanceled
	}

	return c.write(data)
}

func (c *closeIO) write(data []byte) (int, error) {
	if c.w == nil {
		return 0, ErrNotSupported
	}

	n, err := c.w.Write(data)
	if err != nil && c.writeCanceled.Load() {
		return 0, ErrCanceled
	}

	return n, err
}

func (c *closeIO) CancelReads() {
	c.readCanceled.Store(true)
	c.close()
}

func (c *closeIO) CancelWrites() {
	c.writeCanceled.Store(true)
	c.close()
}

func (c *closeIO) Cancel() {
	c.CancelReads()
	c.CancelWrites()
}

func (c *closeIO) ResetReader() { c.readCanceled.Store(false) }
func (c *closeIO) ResetWriter() { c.writeCanceled.Store(false) }
func (c *closeIO) Reset()       { c.ResetReader(); c.ResetWriter() }

func (c *closeIO) Close() error {
	c.Cancel()

	return c.close()
}

// close closes the underlying object idempotently.
func (c *closeIO) close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return nil
	}

	c.closed = true

	return c.closer.Close()
}
