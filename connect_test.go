package ctxio

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

func TestConnectAndClose(t *testing.T) {
	t.Parallel()

	aContent := []byte("a content")
	a := &testRWC{
		Content: aContent,
	}
	bContent := []byte("b content")
	b := &testRWC{
		Content: bContent,
	}

	err := ConnectAndClose(context.Background(), a, b)
	if err != nil {
		t.Fatalf("connect and close: %v", err)
	}

	if !bytes.Equal(bContent, a.Received) {
		t.Errorf("expected file a to have received %q instead of %q",
			string(b.Content), string(a.Received))
	}

	if !a.Closed() {
		t.Errorf("expected a to be closed")
	}

	if !bytes.Equal(aContent, b.Received) {
		t.Errorf("expected file b to have received %q instead of %q",
			string(a.Content), string(b.Received))
	}

	if !b.Closed() {
		t.Errorf("expected b to be closed")
	}
}

func TestConnectAndCloseReturnsWhenOneCopyDirectionFails(t *testing.T) {
	t.Parallel()

	copyErr := errors.New("copy failed")
	blocking := newBlockingReadWriteCloser()
	failing := newFailingReadWriteCloser(copyErr)

	result := make(chan error, 1)
	go func() {
		result <- ConnectAndClose(context.Background(), blocking, failing)
	}()

	select {
	case <-blocking.readStarted:
	case <-time.After(time.Second):
		t.Fatal("reverse copy did not start")
	}

	select {
	case <-failing.readFinished:
	case <-time.After(time.Second):
		t.Fatal("failing copy did not return")
	}

	select {
	case err := <-result:
		if !errors.Is(err, copyErr) {
			t.Fatalf("ConnectAndClose returned %v, want %v", err, copyErr)
		}
	case <-time.After(time.Second):
		_ = blocking.Close()
		_ = failing.Close()

		<-result

		t.Fatal("ConnectAndClose did not unblock the other copy direction after an error")
	}
}

type blockingReadWriteCloser struct {
	readStarted chan struct{}
	unblock     chan struct{}
	startOnce   sync.Once
	closeOnce   sync.Once
}

func newBlockingReadWriteCloser() *blockingReadWriteCloser {
	return &blockingReadWriteCloser{
		readStarted: make(chan struct{}),
		unblock:     make(chan struct{}),
	}
}

func (connection *blockingReadWriteCloser) Read(_ []byte) (int, error) {
	connection.startOnce.Do(func() { close(connection.readStarted) })
	<-connection.unblock

	return 0, io.ErrClosedPipe
}

func (*blockingReadWriteCloser) Write(contents []byte) (int, error) {
	return len(contents), nil
}

func (connection *blockingReadWriteCloser) Close() error {
	connection.closeOnce.Do(func() { close(connection.unblock) })

	return nil
}

type failingReadWriteCloser struct {
	err          error
	readFinished chan struct{}
	finishOnce   sync.Once
}

func newFailingReadWriteCloser(err error) *failingReadWriteCloser {
	return &failingReadWriteCloser{
		err:          err,
		readFinished: make(chan struct{}),
	}
}

func (connection *failingReadWriteCloser) Read(_ []byte) (int, error) {
	connection.finishOnce.Do(func() { close(connection.readFinished) })

	return 0, connection.err
}

func (*failingReadWriteCloser) Write(contents []byte) (int, error) {
	return len(contents), nil
}

func (*failingReadWriteCloser) Close() error {
	return nil
}

func TestConnectAndCloseCancel(t *testing.T) {
	t.Parallel()

	aContent := []byte("a content")
	a := &testRWC{
		Content: aContent,
	}
	b := blockOnRead()

	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	err := ConnectAndClose(ctx, a, b)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("connect and close returned %v instead of %q",
			err, context.Canceled.Error())
	}

	if !a.Closed() {
		t.Errorf("expected a to be closed")
	}

	if !b.Closed() {
		t.Errorf("expected b to be closed")
	}

	if !bytes.Equal(aContent, b.Received) {
		t.Errorf("expected file b to have received %q instead of %q",
			string(a.Content), string(b.Received))
	}
}

type testRWC struct {
	Content  []byte
	idx      int
	closed   atomicFlag
	Received []byte
}

func (trwc *testRWC) Read(data []byte) (int, error) {
	if trwc.closed.IsSet() {
		return 0, io.ErrClosedPipe
	}

	if trwc.idx == len(trwc.Content) {
		return 0, io.EOF
	}

	end := min(trwc.idx+len(data), len(trwc.Content))

	n := copy(data, trwc.Content[trwc.idx:end])
	trwc.idx += n

	return n, nil
}

func (trwc *testRWC) Write(data []byte) (int, error) {
	if trwc.closed.IsSet() {
		return 0, io.ErrClosedPipe
	}

	trwc.Received = append(trwc.Received, data...)

	return len(data), nil
}

func (trwc *testRWC) Close() error {
	trwc.closed.Set()

	return nil
}

func (trwc *testRWC) Closed() bool {
	return trwc.closed.IsSet()
}

type blockingRWC struct {
	closed   atomicFlag
	close    chan struct{}
	Received []byte
}

func blockOnRead() *blockingRWC {
	return &blockingRWC{
		close: make(chan struct{}),
	}
}

func (brwc *blockingRWC) Read(data []byte) (int, error) {
	if brwc.closed.IsSet() {
		return 0, io.ErrClosedPipe
	}

	<-brwc.close

	return 0, io.ErrClosedPipe
}

func (brwc *blockingRWC) Write(data []byte) (int, error) {
	select {
	case <-brwc.close:
		return 0, io.ErrClosedPipe
	default:
	}

	brwc.Received = append(brwc.Received, data...)

	return len(data), nil
}

func (brwc *blockingRWC) Close() error {
	if brwc.closed.IsSet() {
		return fmt.Errorf("already closed")
	}

	brwc.closed.Set()
	close(brwc.close)

	return nil
}

func (brwc *blockingRWC) Closed() bool {
	return brwc.closed.IsSet()
}

// duplexConns creates a pair of connected TCP connections for bidirectional tests.
func duplexConns(tb testing.TB) (net.Conn, net.Conn) {
	tb.Helper()

	return activeConns(tb, "tcp4", "127.0.0.1:0")
}

func TestConnect(t *testing.T) {
	t.Parallel()

	connA, connB := duplexConns(t)
	defer connA.Close()
	defer connB.Close()

	ca, err := WrapConn(connA)
	if err != nil {
		t.Fatalf("new context io a: %v", err)
	}
	defer ca.Close()

	cb, err := WrapConn(connB)
	if err != nil {
		t.Fatalf("new context io b: %v", err)
	}
	defer cb.Close()

	// Use a short timeout to verify Connect returns when canceled.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err = Connect(ctx, ca, cb)
	if err == nil {
		// No data to exchange, so either EOF or cancel is acceptable
		return
	}

	// ErrCanceled from the context timeout is the expected path
	if !errors.Is(err, ErrCanceled) {
		t.Fatalf("connect: %v", err)
	}
}

func TestConnectCancel(t *testing.T) {
	t.Parallel()

	connA, connB := duplexConns(t)
	defer connA.Close()
	defer connB.Close()

	ca, err := WrapConn(connA)
	if err != nil {
		t.Fatalf("new context io a: %v", err)
	}
	defer ca.Close()

	cb, err := WrapConn(connB)
	if err != nil {
		t.Fatalf("new context io b: %v", err)
	}
	defer cb.Close()

	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	err = Connect(ctx, ca, cb)
	if err == nil {
		t.Fatal("expected error from canceled connect, got nil")
	}
}

func TestConnectAndCloseAlreadyCanceled(t *testing.T) {
	t.Parallel()

	a := &testRWC{Content: []byte("data")}
	b := &testRWC{Content: []byte("data")}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := ConnectAndClose(ctx, a, b)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}

	if !a.Closed() {
		t.Error("expected a to be closed")
	}

	if !b.Closed() {
		t.Error("expected b to be closed")
	}
}

func TestConnectAndCloseBothEmpty(t *testing.T) {
	t.Parallel()

	a := &testRWC{}
	b := &testRWC{}

	err := ConnectAndClose(context.Background(), a, b)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}

	if !a.Closed() {
		t.Error("expected a to be closed")
	}

	if !b.Closed() {
		t.Error("expected b to be closed")
	}

	if len(a.Received) != 0 {
		t.Errorf("expected a to receive nothing, got %q", a.Received)
	}

	if len(b.Received) != 0 {
		t.Errorf("expected b to receive nothing, got %q", b.Received)
	}
}
