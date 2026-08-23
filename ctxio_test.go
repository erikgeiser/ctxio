//nolint:goconst
package ctxio

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// backend describes a cancellation backend that can be tested independently.
type backend struct {
	name string
	// new constructs a ContextIO wrapping rw. Backends that need a raw file
	// descriptor must extract it themselves so that files are not needlessly
	// put into blocking mode (which would break deadline-based I/O).
	new func(rw io.ReadWriter, name string) (ContextIO, error)
	// skipWriteCancel skips write cancellation tests for this backend.
	// Only the select backend needs this: once rw.Write is in flight,
	// the cancel pipe is no longer watched.
	skipWriteCancel bool
}

// deadlineNew constructs a deadline-based ContextIO. It is used as the `new`
// field of a backend and is available on all platforms.
func deadlineNew(rw io.ReadWriter, name string) (ContextIO, error) {
	d, ok := rw.(Deadliner)
	if !ok {
		return nil, fmt.Errorf("%T does not implement Deadliner", rw)
	}

	return &deadlineIO{d: d, name: name}, nil
}

func TestConnName(t *testing.T) {
	t.Parallel()

	socketPath := filepath.Join(os.TempDir(),
		fmt.Sprintf("cancelreader_conn_name_%d", time.Now().Unix()))

	t.Cleanup(func() { os.RemoveAll(socketPath) })

	testCases := []struct {
		Network string
		Addr    string
	}{
		{Network: "unix", Addr: socketPath},
		{Network: "tcp4", Addr: "127.0.0.1:0"},
		{Network: "tcp6", Addr: "[::1]:0"},
		{Network: "udp4", Addr: "127.0.0.1:0"},
		{Network: "udp6", Addr: "[::1]:0"},
	}

	for _, testCase := range testCases {
		t.Run(fmt.Sprintf("%s:%s", testCase.Network, testCase.Addr), func(t *testing.T) {
			t.Parallel()
			conn, _ := activeConns(t, testCase.Network, testCase.Addr)

			name := connName(conn)

			rawNetwork := strings.TrimSuffix(strings.TrimSuffix(testCase.Network, "4"), "6")
			if !strings.Contains(strings.ToLower(name),
				strings.ToLower(rawNetwork)) {
				t.Errorf("conn name %q does not contain network %q",
					name, rawNetwork)
			}

			if !strings.Contains(name, conn.LocalAddr().String()) {
				t.Errorf("conn name %q does not contain local address %q",
					name, conn.LocalAddr().String())
			}

			remoteAddr := ""
			if ra := conn.RemoteAddr(); ra != nil {
				remoteAddr = ra.String()
			}

			if !strings.Contains(name, remoteAddr) {
				t.Errorf("conn name %q does not contain remote address %q",
					name, remoteAddr)
			}
		})
	}
}

func TestRead(t *testing.T) {
	t.Parallel()

	const timeout = 500 * time.Millisecond

	for _, b := range backends {
		t.Run(b.name, func(t *testing.T) {
			t.Parallel()

			runReaderTest := func(t *testing.T, testCase readerTestCase) {
				t.Helper()

				cr := testCase.CIO

				// test canceled read via context
				testContextRead(t, cr, timeout)

				// test canceled read
				testCanceledRead(t, cr, timeout)

				// test regular read after a read was canceled before
				bufB := []byte("test")
				bufA := make([]byte, len(bufB))

				_, err := testCase.Writer.Write(bufB)
				if err != nil {
					t.Fatalf("write message: %v", err)
				}

				_, err = cr.Read(bufA)
				if err != nil {
					t.Fatalf("read message: %v", err)
				}

				if !bytes.Equal(bufA, bufB) {
					t.Fatalf("buffers differ: bufA=%s, bufB=%s", string(bufA), string(bufB))
				}

				// test canceled read after a successful and a canceled previous
				// read
				testCanceledRead(t, cr, timeout)
			}

			for _, network := range []string{"unix", "tcp4", "tcp6", "udp4", "udp6"} {
				t.Run(network, func(t *testing.T) {
					t.Parallel()
					runReaderTest(t, connReaderTestCase(t, network, b))
				})
			}

			t.Run("pipe", func(t *testing.T) {
				t.Parallel()
				runReaderTest(t, pipeReaderTestCase(t, b))
			})
		})
	}
}

func testCanceledRead(tb testing.TB, cr ContextIO, timeout time.Duration) {
	tb.Helper()

	done := make(chan struct{})

	go func() {
		defer close(done)

		buf := make([]byte, 1)

		_, err := cr.Read(buf)
		if !errors.Is(err, ErrCanceled) {
			tb.Errorf("read returned \"%v\" instead of \"%v\"", err, ErrCanceled)
		}
	}()

	time.Sleep(100 * time.Millisecond)

	select {
	case <-done:
		tb.Fatalf("read is not blocking")
	default:
	}

	cr.Cancel()

	select {
	case <-done:
	case <-time.After(timeout):
		tb.Fatalf("cancel did not interrupt read after %s", timeout)
	}

	cr.Reset()
}

func testContextRead(tb testing.TB, cr ContextIO, timeout time.Duration) {
	tb.Helper()

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})

	go func() {
		defer close(done)

		buf := make([]byte, 1)

		_, err := cr.ReadContext(ctx, buf)
		if !errors.Is(err, ErrCanceled) {
			tb.Errorf("read returned %q instead of %q", err.Error(), ErrCanceled.Error())
		}
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(timeout):
		tb.Fatalf("cancel did not interrupt read after %s", timeout)
	}

	cr.Reset()
}

func TestWrite(t *testing.T) {
	t.Parallel()

	const timeout = 5 * time.Second

	for _, b := range backends {
		t.Run(b.name, func(t *testing.T) {
			t.Parallel()

			if b.skipWriteCancel {
				t.Skip("backend does not support write cancellation")
			}

			runWriterTest := func(t *testing.T, testCase writerTestCase) {
				t.Helper()

				// test canceled write via context
				testContextWrite(t, testCase, timeout)

				// test canceled write via Cancel()
				testCanceledWrite(t, testCase, timeout)

				// verify write works after cancel+reset before continuing
				testNormalWrite(t, testCase)

				testContextWrite(t, testCase, timeout)

				// test canceled write via Cancel()
				testCanceledWrite(t, testCase, timeout)
			}

			socketNetworks := []struct{ network, addr string }{
				{"tcp4", "127.0.0.1:0"},
				{"tcp6", "[::1]:0"},
				{"unix", filepath.Join(t.TempDir(), "w.sock")},
			}

			for _, n := range socketNetworks {
				t.Run(n.network, func(t *testing.T) {
					t.Parallel()

					if runtime.GOOS == "windows" && b.name == "deadline" {
						t.Skip("SetWriteBuffer unreliable on Windows; covered by net.Pipe test")
					}

					runWriterTest(t, connWriterTestCase(t, n.network, n.addr, b))
				})
			}

			t.Run("pipe", func(t *testing.T) {
				t.Parallel()
				runWriterTest(t, pipeWriterTestCase(t, b))
			})

			t.Run("net.Pipe", func(t *testing.T) {
				t.Parallel()
				runWriterTest(t, netPipeWriterTestCase(t, b))
			})
		})
	}
}

func TestStackedCancels(t *testing.T) {
	t.Parallel()

	for _, backend := range backends {
		t.Run(backend.name, func(t *testing.T) {
			t.Parallel()

			r, w, err := os.Pipe()
			if err != nil {
				t.Fatalf("pipe: %v", err)
			}

			defer r.Close()
			defer w.Close()

			cio, err := backend.new(r, "pipe")
			if err != nil {
				t.Skipf("backend does not support pipe: %v", err)
			}

			// Stack 3 cancel signals before any read starts.
			cio.CancelReads()
			cio.CancelReads()
			cio.CancelReads()

			// All reads should fail with ErrCanceled until reset.
			_, err = cio.Read(make([]byte, 1))
			if !errors.Is(err, ErrCanceled) {
				t.Fatalf("expected ErrCanceled after stacked cancels, got %v", err)
			}

			// Reset and verify reads work again.
			cio.ResetReader()

			testData := []byte("abc")

			go func() {
				_, _ = w.Write(testData)
			}()

			buf := make([]byte, 3)

			_, err = cio.Read(buf[0:1])
			if err != nil {
				t.Fatalf("first read after reset failed: %v", err)
			}

			_, err = cio.Read(buf[1:2])
			if err != nil {
				t.Fatalf("second read after reset failed: %v", err)
			}

			_, err = cio.Read(buf[2:3])
			if err != nil {
				t.Fatalf("third read after reset failed: %v", err)
			}

			if !bytes.Equal(buf, testData) {
				t.Fatalf("read %q instead of %q", string(buf), string(testData))
			}
		})
	}
}

func testContextWrite(tb testing.TB, tc writerTestCase, timeout time.Duration) {
	tb.Helper()

	cw := tc.CIO

	if tc.Conn != nil {
		if conn, ok := tc.Conn.(*net.TCPConn); ok {
			fillConnSendBuffer(tb, conn)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})

	go func() {
		defer close(done)

		_, err := cw.WriteContext(ctx, make([]byte, blockingWriteSize(tc)))
		if !errors.Is(err, ErrCanceled) {
			tb.Errorf("write returned \"%v\" instead of \"%v\"", err, ErrCanceled)
		}
	}()

	select {
	case <-done:
		tb.Fatalf("write did not hang")
	case <-time.After(100 * time.Millisecond):
	}

	cancel()

	select {
	case <-done:
	case <-time.After(timeout):
		tb.Fatalf("context cancel did not interrupt write after %s", timeout)
	}

	cw.Reset()
}

func testCanceledWrite(tb testing.TB, tc writerTestCase, timeout time.Duration) {
	tb.Helper()

	cw := tc.CIO

	if tc.Conn != nil {
		if conn, ok := tc.Conn.(*net.TCPConn); ok {
			fillConnSendBuffer(tb, conn)
		}
	}

	done := make(chan struct{})

	go func() {
		defer close(done)

		_, err := cw.Write(make([]byte, blockingWriteSize(tc)))
		if !errors.Is(err, ErrCanceled) {
			tb.Errorf("write returned \"%v\" instead of \"%v\"", err, ErrCanceled)
		}
	}()

	select {
	case <-done:
		tb.Fatalf("write did not hang")
	case <-time.After(100 * time.Millisecond):
	}

	cw.Cancel()

	select {
	case <-done:
	case <-time.After(timeout):
		tb.Fatalf("cancel did not interrupt write after %s", timeout)
	}

	cw.Reset()
}

// testNormalWrite verifies that a write completes successfully after a
// cancel+reset cycle. It first drains any data left over in the peer's receive
// buffer from the previous blocked write, then does a small write and verifies
// the data arrives.
func testNormalWrite(tb testing.TB, tc writerTestCase) {
	tb.Helper()

	type readDeadliner interface {
		SetReadDeadline(t time.Time) error
	}

	// Drain any data left over from the previous blocked write so the kernel
	// send/pipe buffer has room for our test payload.
	if dl, ok := tc.Reader.(readDeadliner); ok {
		_ = dl.SetReadDeadline(time.Now().Add(100 * time.Millisecond))

		buf := make([]byte, 64*1024)

		for {
			_, err := tc.Reader.Read(buf)
			if err != nil {
				break
			}
		}

		_ = dl.SetReadDeadline(time.Time{})
	}

	const payload = "write-works-check"

	readDone := make(chan error, 1)
	readBuf := make([]byte, len(payload))

	go func() {
		_, err := io.ReadFull(tc.Reader, readBuf)
		readDone <- err
	}()

	_, err := tc.CIO.Write([]byte(payload))
	if err != nil {
		tb.Fatalf("write after cancel+reset failed: %v", err)
	}

	select {
	case err := <-readDone:
		if err != nil {
			tb.Fatalf("write after cancel+reset: read failed: %v", err)
		}

		if string(readBuf) != payload {
			tb.Fatalf("write after cancel+reset: got %q, want %q", readBuf, payload)
		}
	case <-time.After(2 * time.Second):
		tb.Fatal("write after cancel+reset: timed out reading back data")
	}
}

func testAlreadyCanceledContextRead(tb testing.TB, cr ContextIO) {
	tb.Helper()

	defer cr.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan error, 1)

	go func() {
		_, err := cr.ReadContext(ctx, make([]byte, 1))
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, ErrCanceled) {
			tb.Fatalf("expected ErrCanceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		tb.Fatal("ReadContext with already-canceled context did not return in time")
	}
}

func testAlreadyCanceledContextWrite(tb testing.TB, cw ContextIO) {
	tb.Helper()

	defer cw.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan error, 1)

	go func() {
		_, err := cw.WriteContext(ctx, []byte("hello"))
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, ErrCanceled) {
			tb.Fatalf("expected ErrCanceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		tb.Fatal("WriteContext with already-canceled context did not return in time")
	}
}

func activeConns(tb testing.TB, network string, addr string) (net.Conn, net.Conn) {
	tb.Helper()

	if network != "unix" && isIPv6(tb, addr) && !hasLoopbackIPv6(tb) {
		tb.Skip("no IPv6 loopback available")
	}

	if strings.HasPrefix(network, "udp") || network == "unixpacket" {
		return activeUDPConns(tb, network, addr)
	}

	listener, err := (&net.ListenConfig{}).Listen(tb.Context(), network, addr)
	if err != nil {
		tb.Fatalf("listen: %v", err)
	}

	secondConnReady := make(chan []byte)

	var wg sync.WaitGroup

	done := make(chan struct{})

	var dialerConn net.Conn

	wg.Go(func() {
		conn, err := (&net.Dialer{}).DialContext(tb.Context(),
			listener.Addr().Network(), listener.Addr().String())
		if err != nil {
			tb.Fatalf("dial: %v", err)
		}

		dialerConn = conn

		close(secondConnReady)

		<-done
	})

	listenerConn, err := listener.Accept()
	if err != nil {
		tb.Fatalf("accept: %v", err)
	}

	<-secondConnReady

	if dialerConn == nil {
		tb.Fatalf("dialerConn was not set")
	}

	tb.Cleanup(func() {
		close(done)

		_ = dialerConn.Close()
		_ = listenerConn.Close()
		_ = listener.Close()

		wg.Wait()
	})

	return listenerConn, dialerConn
}

func activeUDPConns(tb testing.TB, network string, addr string) (net.Conn, net.Conn) {
	tb.Helper()

	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		tb.Fatalf("split host port: %v", err)
	}

	ip := net.ParseIP(host)
	if ip == nil {
		tb.Fatalf("parse addr: %v", err)
	}

	port, err := strconv.Atoi(portStr)
	if err != nil {
		tb.Fatalf("parse port: %v", err)
	}

	udpAddr := &net.UDPAddr{IP: ip, Port: port}

	listenerConn, err := net.ListenUDP(network, udpAddr)
	if err != nil {
		tb.Fatalf("listen: %v", err)
	}

	listenerAddr, ok := listenerConn.LocalAddr().(*net.UDPAddr)
	if !ok {
		tb.Fatalf("unexpected type for local address %T", listenerConn.LocalAddr())
	}

	tb.Cleanup(func() { _ = listenerConn.Close() })

	dialerConn, err := net.DialUDP(network, &net.UDPAddr{IP: ip, Port: 0}, listenerAddr)
	if err != nil {
		tb.Fatalf("dial: %v", err)
	}

	tb.Cleanup(func() { _ = dialerConn.Close() })

	return listenerConn, dialerConn
}

type readerTestCase struct {
	Name   string
	CIO    ContextIO
	Writer io.Writer
}

func connReaderTestCase(tb testing.TB, network string, b backend) readerTestCase {
	tb.Helper()

	var addr string

	switch strings.ToLower(network) {
	case "unix":
		addr = filepath.Join(tb.TempDir(), "conn.sock")
	case "tcp", "tcp4", "udp", "udp4":
		addr = "127.0.0.1:0"
	case "tcp6", "udp6":
		addr = "[::1]:0"
	default:
		tb.Fatalf("unsupported network: %q", network)
	}

	connA, connB := activeConns(tb, network, addr)

	cio, err := b.new(connA, connName(connA))
	if err != nil {
		tb.Skipf("backend %s not available for %s conn: %v", b.name, network, err)
	}

	return readerTestCase{Name: connName(connA), CIO: cio, Writer: connB}
}

func pipeReaderTestCase(tb testing.TB, b backend) readerTestCase {
	tb.Helper()

	if runtime.GOOS == "windows" {
		tb.Skip("anonymous pipes are tested in Windows-specific tests")
	}

	r, w, err := os.Pipe()
	if err != nil {
		tb.Fatalf("pipe: %v", err)
	}

	tb.Cleanup(func() {
		// r may already be closed if the ContextIO backend owns it (e.g.
		// deadlineIO).
		_ = r.Close()

		err = w.Close()
		if err != nil {
			tb.Fatalf("closing pipe writer: %v", err)
		}
	})

	cio, err := b.new(r, r.Name())
	if err != nil {
		tb.Skipf("backend %s not available for pipe: %v", b.name, err)
	}

	return readerTestCase{Name: r.Name(), CIO: cio, Writer: w}
}

type writerTestCase struct {
	Name         string
	CIO          ContextIO
	Reader       io.Reader
	Conn         net.Conn // non-nil for connection-based test cases
	IsFile       bool
	blockingSize int // default is 0 (4 MB for conns, 1 MB for files)
}

// fillConnSendBuffer exhausts the TCP send buffer of conn so that subsequent
// writes will block. It does this by writing chunks with a short deadline until
// a timeout occurs, which means the kernel buffer is full. The peer must NOT be
// reading during this call. Returns the number of bytes written into the
// buffer.
func fillConnSendBuffer(tb testing.TB, conn net.Conn) int { //nolint:unparam
	tb.Helper()

	chunk := make([]byte, 4096)
	total := 0

	for {
		err := conn.SetWriteDeadline(time.Now().Add(10 * time.Millisecond))
		if err != nil {
			tb.Fatalf("set write deadline: %v", err)
		}

		n, err := conn.Write(chunk)
		total += n

		if err != nil {
			// Deadline exceeded means the buffer is full — that's what we want.
			break
		}
	}

	// Clear the deadline so future writes are not affected.
	err := conn.SetWriteDeadline(time.Time{})
	if err != nil {
		tb.Fatalf("clear write deadline: %v", err)
	}

	return total
}

func blockingWriteSize(tc writerTestCase) int {
	if tc.blockingSize > 0 {
		return tc.blockingSize
	}

	if tc.IsFile {
		return 1024 * 1024
	}

	return 4 * 1024 * 1024
}

func netPipeWriterTestCase(tb testing.TB, b backend) writerTestCase {
	tb.Helper()

	connA, connB := net.Pipe()

	tb.Cleanup(func() { _ = connA.Close(); _ = connB.Close() })

	cio, err := b.new(connA, connName(connA))
	if err != nil {
		tb.Skipf("backend %s not available for net.Pipe: %v", b.name, err)
	}

	// net.Pipe is unbuffered; any write blocks without a reader. Conn is
	// intentionally nil so no SetWriteBuffer call is attempted.
	return writerTestCase{Name: "net.Pipe", CIO: cio, Reader: connB, blockingSize: 1}
}

func connWriterTestCase(tb testing.TB, network string, addr string, b backend) writerTestCase {
	tb.Helper()

	connA, connB := activeConns(tb, network, addr)

	cio, err := b.new(connA, connName(connA))
	if err != nil {
		tb.Skipf("backend %s not available for %s conn: %v", b.name, network, err)
	}

	return writerTestCase{Name: connName(connA), CIO: cio, Reader: connB, Conn: connA}
}

func pipeWriterTestCase(tb testing.TB, b backend) writerTestCase {
	tb.Helper()

	if runtime.GOOS == "windows" {
		tb.Skip("anonymous pipes are tested in Windows-specific tests")
	}

	r, w, err := os.Pipe()
	if err != nil {
		tb.Fatalf("pipe: %v", err)
	}

	tb.Cleanup(func() {
		err = r.Close()
		if err != nil {
			tb.Fatalf("closing pipe reader: %v", err)
		}

		// w may already be closed if the ContextIO backend owns it (e.g.
		// deadlineIO).
		_ = w.Close()
	})

	cio, err := b.new(w, w.Name())
	if err != nil {
		tb.Skipf("backend %s not available for pipe: %v", b.name, err)
	}

	return writerTestCase{Name: w.Name(), CIO: cio, Reader: r, IsFile: true}
}

func TestAlreadyCanceledContext(t *testing.T) {
	t.Parallel()

	for _, b := range backends {
		t.Run(b.name, func(t *testing.T) {
			t.Parallel()
			t.Run("read/pipe", func(t *testing.T) {
				t.Parallel()
				tc := pipeReaderTestCase(t, b)
				testAlreadyCanceledContextRead(t, tc.CIO)
			})

			t.Run("read/tcp", func(t *testing.T) {
				t.Parallel()
				tc := connReaderTestCase(t, "tcp4", b)
				testAlreadyCanceledContextRead(t, tc.CIO)
			})

			t.Run("write/pipe", func(t *testing.T) {
				t.Parallel()
				tc := pipeWriterTestCase(t, b)
				testAlreadyCanceledContextWrite(t, tc.CIO)
			})

			t.Run("write/tcp", func(t *testing.T) {
				t.Parallel()
				tc := connWriterTestCase(t, "tcp4", "127.0.0.1:0", b)
				testAlreadyCanceledContextWrite(t, tc.CIO)
			})
		})
	}
}

func TestDoubleCancel(t *testing.T) {
	t.Parallel()

	for _, b := range backends {
		t.Run(b.name, func(t *testing.T) {
			t.Parallel()
			t.Run("pipe", func(t *testing.T) {
				t.Parallel()

				tc := pipeReaderTestCase(t, b)
				defer tc.CIO.Close()
				// Double cancel must not panic or deadlock.
				tc.CIO.Cancel()
				tc.CIO.Cancel()
			})

			t.Run("tcp", func(t *testing.T) {
				t.Parallel()

				tc := connReaderTestCase(t, "tcp4", b)
				defer tc.CIO.Close()
				// Double cancel must not panic or deadlock.
				tc.CIO.Cancel()
				tc.CIO.Cancel()
			})
		})
	}
}

func TestWrapClosedFile(t *testing.T) {
	t.Parallel()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}

	w.Close()
	r.Close()

	_, err = WrapFile(r)
	if err == nil {
		t.Fatal("expected error for closed file, got nil")
	}
}

func TestStackedCancelWrites(t *testing.T) {
	t.Parallel()

	for _, b := range backends {
		t.Run(b.name, func(t *testing.T) {
			t.Parallel()

			if b.skipWriteCancel {
				t.Skip("backend does not support write cancellation")
			}

			connA, connB := activeConns(t, "tcp4", "127.0.0.1:0")

			cio, err := b.new(connA, connName(connA))
			if err != nil {
				t.Skipf("backend %s not available for tcp conn: %v", b.name, err)
			}

			defer cio.Close()

			// Stack 3 cancel signals — must not panic or deadlock.
			cio.CancelWrites()
			cio.CancelWrites()
			cio.CancelWrites()

			// All writes should fail with ErrCanceled.
			_, err = cio.Write([]byte("hello"))
			if !errors.Is(err, ErrCanceled) {
				t.Fatalf("expected ErrCanceled after stacked cancels, got %v", err)
			}

			// A single reset should clear all stacked cancels.
			cio.ResetWriter()

			testData := []byte("abc")

			_, err = cio.Write(testData)
			if err != nil {
				t.Fatalf("write after reset failed: %v", err)
			}

			buf := make([]byte, len(testData))

			_, err = io.ReadFull(connB, buf)
			if err != nil {
				t.Fatalf("read failed: %v", err)
			}

			if !bytes.Equal(buf, testData) {
				t.Fatalf("read %q instead of %q", string(buf), string(testData))
			}
		})
	}
}

func TestWriteAfterReset(t *testing.T) {
	t.Parallel()

	for _, b := range backends {
		t.Run(b.name, func(t *testing.T) {
			t.Parallel()

			if b.skipWriteCancel {
				t.Skip("backend does not support write cancellation")
			}

			tc := connWriterTestCase(t, "tcp4", "127.0.0.1:0", b)
			cio := tc.CIO

			defer cio.Close()

			// Cancel writes, verify they fail.
			cio.CancelWrites()

			_, err := cio.Write([]byte("should fail"))
			if !errors.Is(err, ErrCanceled) {
				t.Fatalf("expected ErrCanceled, got %v", err)
			}

			cio.ResetWriter()

			// Write real data after reset and verify it arrives.
			testData := []byte("after reset")

			_, err = cio.Write(testData)
			if err != nil {
				t.Fatalf("write after reset: %v", err)
			}

			buf := make([]byte, len(testData))

			_, err = io.ReadFull(tc.Reader, buf)
			if err != nil {
				t.Fatalf("read: %v", err)
			}

			if !bytes.Equal(buf, testData) {
				t.Fatalf("got %q, want %q", string(buf), string(testData))
			}
		})
	}
}

func TestCancelReadsWriteIndependence(t *testing.T) {
	t.Parallel()

	for _, b := range backends {
		t.Run(b.name, func(t *testing.T) {
			t.Parallel()
			connA, connB := activeConns(t, "tcp4", "127.0.0.1:0")

			cio, err := b.new(connA, connName(connA))
			if err != nil {
				t.Skipf("backend %s not available for tcp conn: %v", b.name, err)
			}

			defer cio.Close()

			// Cancel reads, verify writes still work.
			cio.CancelReads()

			_, err = cio.Read(make([]byte, 1))
			if !errors.Is(err, ErrCanceled) {
				t.Fatalf("expected read ErrCanceled, got %v", err)
			}

			testData := []byte("still works")

			_, err = cio.Write(testData)
			if err != nil {
				t.Fatalf("write should work after CancelReads: %v", err)
			}

			buf := make([]byte, len(testData))

			_, err = io.ReadFull(connB, buf)
			if err != nil {
				t.Fatalf("read from peer: %v", err)
			}

			if !bytes.Equal(buf, testData) {
				t.Fatalf("got %q, want %q", string(buf), string(testData))
			}

			cio.ResetReader()

			if b.skipWriteCancel {
				return
			}

			// Cancel writes, verify reads still work.
			cio.CancelWrites()

			_, err = cio.Write([]byte("should fail"))
			if !errors.Is(err, ErrCanceled) {
				t.Fatalf("expected write ErrCanceled, got %v", err)
			}

			testData2 := []byte("read works")

			_, err = connB.Write(testData2)
			if err != nil {
				t.Fatalf("write from peer: %v", err)
			}

			buf2 := make([]byte, len(testData2))

			_, err = io.ReadFull(cio, buf2)
			if err != nil {
				t.Fatalf("read should work after CancelWrites: %v", err)
			}

			if !bytes.Equal(buf2, testData2) {
				t.Fatalf("got %q, want %q", string(buf2), string(testData2))
			}
		})
	}
}

func TestCancelStickyWithoutReset(t *testing.T) {
	t.Parallel()

	for _, b := range backends {
		t.Run(b.name, func(t *testing.T) {
			t.Parallel()

			r, w, err := os.Pipe()
			if err != nil {
				t.Fatalf("pipe: %v", err)
			}

			defer r.Close()
			defer w.Close()

			cio, err := b.new(r, "pipe")
			if err != nil {
				t.Skipf("backend does not support pipe: %v", err)
			}

			defer cio.Close()

			// Cancel without reset — Read should keep returning ErrCanceled.
			cio.CancelReads()

			for i := range 3 {
				_, err = cio.Read(make([]byte, 1))
				if !errors.Is(err, ErrCanceled) {
					t.Fatalf("read attempt %d: expected ErrCanceled, got %v", i, err)
				}
			}
		})
	}
}

func TestCloseClosesUnderlying(t *testing.T) {
	t.Parallel()

	for _, b := range backends {
		t.Run(b.name, func(t *testing.T) {
			t.Parallel()
			t.Run("pipe", func(t *testing.T) {
				t.Parallel()

				r, w, err := os.Pipe()
				if err != nil {
					t.Fatalf("pipe: %v", err)
				}

				defer w.Close()

				cio, err := b.new(r, "pipe")
				if err != nil {
					t.Skipf("backend does not support pipe: %v", err)
				}

				err = cio.Close()
				if err != nil {
					t.Fatalf("close: %v", err)
				}

				// The underlying file should be closed. Reading from it should fail.
				_, err = r.Read(make([]byte, 1))
				if err == nil {
					t.Fatal("expected error reading from underlying file after Close, got nil")
				}
			})

			t.Run("tcp", func(t *testing.T) {
				t.Parallel()
				connA, _ := activeConns(t, "tcp4", "127.0.0.1:0")

				cio, err := b.new(connA, connName(connA))
				if err != nil {
					t.Skipf("backend %s not available for tcp conn: %v", b.name, err)
				}

				err = cio.Close()
				if err != nil {
					t.Fatalf("close: %v", err)
				}

				// The underlying conn should be closed.
				_, err = connA.Read(make([]byte, 1))
				if err == nil {
					t.Fatal("expected error reading from underlying conn after Close, got nil")
				}
			})
		})
	}
}

func TestCancelDuringBlockedRead(t *testing.T) {
	t.Parallel()

	const timeout = 2 * time.Second

	for _, b := range backends {
		t.Run(b.name, func(t *testing.T) {
			t.Parallel()
			// Use a TCP connection rather than a pipe: on Windows, anonymous
			// pipes do not support deadlines, so the deadline backend cannot
			// cancel a blocked pipe read.
			connA, _ := activeConns(t, "tcp4", "127.0.0.1:0")

			cio, err := b.new(connA, connName(connA))
			if err != nil {
				t.Skipf("backend %s not available for tcp conn: %v", b.name, err)
			}

			defer cio.Close()

			done := make(chan error, 1)

			go func() {
				_, err := cio.Read(make([]byte, 1))
				done <- err
			}()

			// Give goroutine time to block in Read.
			time.Sleep(50 * time.Millisecond)

			cio.CancelReads()

			select {
			case err := <-done:
				if !errors.Is(err, ErrCanceled) {
					t.Fatalf("expected ErrCanceled, got %v", err)
				}
			case <-time.After(timeout):
				t.Fatal("CancelReads did not unblock Read")
			}
		})
	}
}

func isIPv6(tb testing.TB, addr string) bool {
	tb.Helper()

	ip := net.ParseIP(addr)
	if ip == nil {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			tb.Fatalf("cannot parse address %s", addr)
		}

		ip = net.ParseIP(host)
		if ip == nil {
			tb.Fatalf("hostnames are not supported")
		}
	}

	return ip.To4() == nil
}

func hasLoopbackIPv6(tb testing.TB) bool {
	tb.Helper()

	ifaces, err := net.Interfaces()
	if err != nil {
		tb.Fatalf("listing interfaces: %v", err)
	}

	loopbackIPv6 := net.ParseIP("::1")
	if loopbackIPv6 == nil {
		tb.Fatalf("invalid loopback IPv6")
	}

	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback == 0 {
			continue
		}

		addrs, err := iface.Addrs()
		if err != nil {
			tb.Fatalf("listing addrs of interface %s: %v", iface.Name, err)
		}

		for _, addr := range addrs {
			ifaceIP, ok := addr.(*net.IPNet)
			if !ok {
				tb.Fatalf("unexpected IP type: %T", addr)
			}

			if ifaceIP.IP.Equal(loopbackIPv6) {
				return true
			}
		}
	}

	return false
}

// TestSmokeTestHelpers verifies that testCanceledRead, testContextRead,
// testCanceledWrite, and testContextWrite actually report failures when given a
// ContextIO implementation that does not support cancellation.
func TestSmokeTestHelpers(t *testing.T) {
	t.Parallel()

	const timeout = 500 * time.Millisecond

	t.Run("testCanceledRead", func(t *testing.T) {
		t.Parallel()

		cio := newBrokenCancelIO()

		t.Cleanup(func() { _ = cio.Close() })

		assertTestFails(t, func(tb testing.TB) {
			tb.Helper()
			testCanceledRead(tb, cio, timeout)
		})
	})

	t.Run("testContextRead", func(t *testing.T) {
		t.Parallel()

		cio := newBrokenCancelIO()

		t.Cleanup(func() { _ = cio.Close() })

		assertTestFails(t, func(tb testing.TB) {
			tb.Helper()
			testContextRead(tb, cio, timeout)
		})
	})

	t.Run("testCanceledWrite", func(t *testing.T) {
		t.Parallel()

		cio := newBrokenCancelIO()

		t.Cleanup(func() { _ = cio.Close() })

		assertTestFails(t, func(tb testing.TB) {
			tb.Helper()
			testCanceledWrite(tb, writerTestCase{CIO: cio, IsFile: true}, timeout)
		})
	})

	t.Run("testContextWrite", func(t *testing.T) {
		t.Parallel()

		cio := newBrokenCancelIO()

		t.Cleanup(func() { _ = cio.Close() })

		assertTestFails(t, func(tb testing.TB) {
			tb.Helper()
			testContextWrite(tb, writerTestCase{CIO: cio, IsFile: true}, timeout)
		})
	})
}

// noopCancelIO blocks on every Read/Write until Close() is called.
// It ignores all cancellation signals to verify that test helpers actually
// detect missing cancellation support.
type noopCancelIO struct {
	done      chan struct{}
	closeOnce sync.Once
}

func newBrokenCancelIO() *noopCancelIO {
	return &noopCancelIO{done: make(chan struct{})}
}

func (n *noopCancelIO) Read(_ []byte) (int, error) {
	<-n.done

	return 0, io.EOF
}

func (n *noopCancelIO) Write(p []byte) (int, error) {
	<-n.done

	return 0, io.EOF
}

func (n *noopCancelIO) Close() error {
	n.closeOnce.Do(func() { close(n.done) })

	return nil
}

func (n *noopCancelIO) ReadContext(_ context.Context, p []byte) (int, error) {
	<-n.done

	return 0, io.EOF
}

func (n *noopCancelIO) WriteContext(_ context.Context, p []byte) (int, error) {
	<-n.done

	return 0, io.EOF
}

func (n *noopCancelIO) Name() string  { return "<Broken ContextIO>" }
func (n *noopCancelIO) Cancel()       {}
func (n *noopCancelIO) CancelReads()  {}
func (n *noopCancelIO) CancelWrites() {}
func (n *noopCancelIO) ResetReader()  {}
func (n *noopCancelIO) ResetWriter()  {}
func (n *noopCancelIO) Reset()        {}

// recordingTB captures test failures without propagating them to the real test,
// allowing callers to assert that a helper did fail.
type recordingTB struct {
	testing.TB
	mu     sync.Mutex
	failed bool
}

func (r *recordingTB) Helper() {}

func (r *recordingTB) Errorf(format string, args ...any) {
	r.mu.Lock()
	r.failed = true
	r.mu.Unlock()
}

func (r *recordingTB) Error(args ...any) {
	r.mu.Lock()
	r.failed = true
	r.mu.Unlock()
}

func (r *recordingTB) Fatalf(format string, args ...any) {
	r.mu.Lock()
	r.failed = true
	r.mu.Unlock()
	runtime.Goexit()
}

func (r *recordingTB) Fatal(args ...any) {
	r.mu.Lock()
	r.failed = true
	r.mu.Unlock()
	runtime.Goexit()
}

func (r *recordingTB) didFail() bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.failed
}

func assertTestFails(t *testing.T, fn func(tb testing.TB)) {
	t.Helper()

	rec := &recordingTB{TB: t}

	done := make(chan struct{})

	go func() {
		defer close(done)

		fn(rec)
	}()

	<-done

	if !rec.didFail() {
		t.Fatal("test did not fail")
	}
}

func TestJoinErrors(t *testing.T) {
	t.Parallel()

	errA := errors.New("a")
	errB := errors.New("b")

	errJoined := joinErrors(errA, errB)

	if !errors.Is(errJoined, errA) {
		t.Fatalf("errors.Is failed for errA")
	}

	if !errors.Is(errJoined, errB) {
		t.Fatalf("errors.Is failed for errB")
	}
}
