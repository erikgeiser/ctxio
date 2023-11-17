package ctxio

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"
)

func TestConnectAndClose(t *testing.T) {
	aContent := []byte("a content")
	a := &testRWC{
		Content: aContent,
	}
	bContent := []byte("b content")
	b := &testRWC{
		Content: bContent,
	}

	err := ConnectAndClose(context.Background(), a, b, false)
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

	if !a.Closed() {
		t.Errorf("expected b to be closed")
	}
}

func TestConnectAndCloseCancel(t *testing.T) {
	aContent := []byte("a content")
	a := &testRWC{
		Content: aContent,
	}
	b := blockOnRead()

	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	err := ConnectAndClose(ctx, a, b, false)
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

	if !a.Closed() {
		t.Errorf("expected b to be closed")
	}
}

type testRWC struct {
	Content  []byte
	idx      int
	closed   bool
	Received []byte
}

func (trwc *testRWC) Read(data []byte) (int, error) {
	if trwc.closed {
		return 0, io.ErrClosedPipe
	}

	if trwc.idx == len(trwc.Content) {
		return 0, io.EOF
	}

	end := trwc.idx + len(data)
	if end > len(trwc.Content) {
		end = len(trwc.Content)
	}

	fmt.Println(string(trwc.Content), trwc.idx, end)
	n := copy(data, trwc.Content[trwc.idx:end])
	trwc.idx += n

	return n, nil
}

func (trwc *testRWC) Write(data []byte) (int, error) {
	if trwc.closed {
		return 0, io.ErrClosedPipe
	}

	trwc.Received = append(trwc.Received, data...)

	return len(data), nil
}

func (trwc *testRWC) Close() error {
	trwc.closed = true

	return nil
}

func (trwc *testRWC) Closed() bool {
	return trwc.closed
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
