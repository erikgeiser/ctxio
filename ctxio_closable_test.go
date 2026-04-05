package ctxio

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

func TestClosableNormalIO(t *testing.T) {
	t.Parallel()

	pr, pw := io.Pipe()

	cio, err := WrapClosableReadWriter(struct {
		io.Reader
		io.Writer
		io.Closer
	}{pr, pw, pw}, "pipe")
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}

	defer cio.Close()

	testData := []byte("hello")

	go func() {
		_, _ = pw.Write(testData)
	}()

	buf := make([]byte, len(testData))

	_, err = cio.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	if !bytes.Equal(buf, testData) {
		t.Fatalf("got %q, want %q", buf, testData)
	}
}

func TestClosableReadCancel(t *testing.T) {
	t.Parallel()

	pr, pw := io.Pipe()
	defer pw.Close()

	cio, _ := WrapClosableReader(pr, "pipe-reader")

	done := make(chan error, 1)

	go func() {
		_, err := cio.Read(make([]byte, 1))
		done <- err
	}()

	time.Sleep(10 * time.Millisecond)

	cio.CancelReads()

	select {
	case err := <-done:
		if !errors.Is(err, ErrCanceled) {
			t.Fatalf("expected ErrCanceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancel did not interrupt read")
	}
}

func TestClosableWriteCancel(t *testing.T) {
	t.Parallel()

	pr, pw := io.Pipe()
	defer pr.Close()

	cio, _ := WrapClosableWriter(pw, "pipe-writer")

	done := make(chan error, 1)

	go func() {
		_, err := cio.Write(make([]byte, 1))
		done <- err
	}()

	time.Sleep(10 * time.Millisecond)

	cio.CancelWrites()

	select {
	case err := <-done:
		if !errors.Is(err, ErrCanceled) {
			t.Fatalf("expected ErrCanceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancel did not interrupt write")
	}
}

func TestClosableContextRead(t *testing.T) {
	t.Parallel()

	pr, pw := io.Pipe()
	defer pw.Close()

	cio, _ := WrapClosableReader(pr, "pipe-reader")

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)

	go func() {
		_, err := cio.ReadContext(ctx, make([]byte, 1))
		done <- err
	}()

	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, ErrCanceled) {
			t.Fatalf("expected ErrCanceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("context cancel did not interrupt read")
	}
}

func TestClosableContextWrite(t *testing.T) {
	t.Parallel()

	pr, pw := io.Pipe()
	defer pr.Close()

	cio, _ := WrapClosableWriter(pw, "pipe-writer")

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)

	go func() {
		_, err := cio.WriteContext(ctx, []byte("hello"))
		done <- err
	}()

	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, ErrCanceled) {
			t.Fatalf("expected ErrCanceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("context cancel did not interrupt write")
	}
}

func TestClosableAlreadyCanceled(t *testing.T) {
	t.Parallel()

	pr, pw := io.Pipe()
	defer pw.Close()

	cio, _ := WrapClosableReader(pr, "pipe-reader")

	cio.CancelReads()

	_, err := cio.Read(make([]byte, 1))
	if !errors.Is(err, ErrCanceled) {
		t.Fatalf("expected ErrCanceled, got %v", err)
	}

	// Also test with already-canceled context.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	cio2, _ := WrapClosableReader(pr, "pipe-reader-2")

	_, err = cio2.ReadContext(ctx, make([]byte, 1))
	if !errors.Is(err, ErrCanceled) {
		t.Fatalf("expected ErrCanceled, got %v", err)
	}
}

func TestClosableResetAfterCancel(t *testing.T) {
	t.Parallel()

	pr, pw := io.Pipe()
	defer pw.Close()

	cio, _ := WrapClosableReader(pr, "pipe-reader")

	cio.CancelReads()
	cio.ResetReader()

	// Flag is cleared, but underlying object is closed — read returns an
	// error that is NOT ErrCanceled (since the flag was reset).
	_, err := cio.Read(make([]byte, 1))
	if err == nil {
		t.Fatal("expected error after cancel+reset, got nil")
	}

	if errors.Is(err, ErrCanceled) {
		t.Fatal("expected non-ErrCanceled error after reset")
	}
}

func TestClosableReadOnWriteOnly(t *testing.T) {
	t.Parallel()

	_, pw := io.Pipe()

	cio, _ := WrapClosableWriter(pw, "pipe-writer")
	defer cio.Close()

	_, err := cio.Read(make([]byte, 1))
	if !errors.Is(err, ErrNotSupported) {
		t.Fatalf("expected ErrNotSupported, got %v", err)
	}

	_, err = cio.ReadContext(context.Background(), make([]byte, 1))
	if !errors.Is(err, ErrNotSupported) {
		t.Fatalf("expected ErrNotSupported from ReadContext, got %v", err)
	}
}

func TestClosableWriteOnReadOnly(t *testing.T) {
	t.Parallel()

	pr, _ := io.Pipe()

	cio, _ := WrapClosableReader(pr, "pipe-reader")
	defer cio.Close()

	_, err := cio.Write([]byte("hello"))
	if !errors.Is(err, ErrNotSupported) {
		t.Fatalf("expected ErrNotSupported, got %v", err)
	}

	_, err = cio.WriteContext(context.Background(), []byte("hello"))
	if !errors.Is(err, ErrNotSupported) {
		t.Fatalf("expected ErrNotSupported from WriteContext, got %v", err)
	}
}

func TestClosableClose(t *testing.T) {
	t.Parallel()

	pr, pw := io.Pipe()

	cio, _ := WrapClosableReadWriter(struct {
		io.Reader
		io.Writer
		io.Closer
	}{pr, pw, pr}, "pipe")

	err := cio.Close()
	if err != nil {
		t.Fatalf("close: %v", err)
	}

	// Underlying reader should be closed.
	_, err = pr.Read(make([]byte, 1))
	if err == nil {
		t.Fatal("expected error after close, got nil")
	}
}

func TestClosableDoubleCancel(t *testing.T) {
	t.Parallel()

	pr, pw := io.Pipe()
	defer pw.Close()

	cio, _ := WrapClosableReader(pr, "pipe-reader")

	// Must not panic or deadlock.
	cio.Cancel()
	cio.Cancel()
}
