package ctxio

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestFileFromConn(t *testing.T) {
	socketPath := filepath.Join(os.TempDir(),
		fmt.Sprintf("cancelreader_file_from_conn_%d", time.Now().Unix()))
	defer os.RemoveAll(socketPath)

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
		testCase := testCase

		t.Run(fmt.Sprintf("%s:%s", testCase.Network, testCase.Addr), func(t *testing.T) {
			conn, _ := activeConns(t, testCase.Network, testCase.Addr)

			f, err := FileFromConn(conn)
			if err != nil {
				t.Fatalf("file from conn: %v", err)
			}

			if f.Fd() == 0 {
				t.Fatalf("unexpected file descriptor %d", f.Fd())
			}

			rawNetwork := strings.TrimSuffix(strings.TrimSuffix(testCase.Network, "4"), "6")
			if !strings.Contains(strings.ToLower(f.Name()),
				strings.ToLower(rawNetwork)) {
				t.Errorf("file name %q  does not contain network %q",
					f.Name(), rawNetwork)
			}

			if !strings.Contains(f.Name(), conn.LocalAddr().String()) {
				t.Errorf("file name %q  does not contain local address %q",
					f.Name(), conn.LocalAddr().String())
			}

			remoteAddr := ""
			if ra := conn.RemoteAddr(); ra != nil {
				remoteAddr = ra.String()
			}

			if !strings.Contains(f.Name(), remoteAddr) {
				t.Errorf("file name %q  does not contain remote address %q",
					f.Name(), remoteAddr)
			}
		})
	}
}

func TestRead(t *testing.T) {
	const timeout = 500 * time.Millisecond

	socketPath := filepath.Join(os.TempDir(),
		fmt.Sprintf("cancelreader_file_from_conn_%d", time.Now().Unix()))
	defer os.RemoveAll(socketPath)

	testCases := []readerTestCase{
		connReaderTestCase(t, "unix", socketPath),
		connReaderTestCase(t, "tcp4", "127.0.0.1:0"),
		// connReaderTestCase(t, "tcp6", "[::1]:0"),
		connReaderTestCase(t, "udp4", "127.0.0.1:0"),
		// connReaderTestCase(t, "udp6", "[::1]:0"),
		pipeReaderTestCase(t),
	}

	for _, testCase := range testCases {
		testCase := testCase

		t.Run(testCase.String(), func(t *testing.T) {
			cr, err := NewCancelReader(testCase.File)
			if err != nil {
				t.Fatalf("new cancel reader: %v", err)
			}

			// test cancelled read via context
			testContextRead(t, cr, timeout)

			// test cancelled read
			testCancelledRead(t, cr, timeout)

			// // test regular read after a read was cancelled before
			// bufB := []byte("test")
			// bufA := make([]byte, len(bufB))

			// _, err = testCase.Writer.Write(bufB)
			// if err != nil {
			// 	t.Fatalf("write message: %v", err)
			// }

			// _, err = cr.Read(bufA)
			// if err != nil {
			// 	t.Fatalf("read message: %v", err)
			// }

			// if !bytes.Equal(bufA, bufB) {
			// 	t.Fatalf("buffers differ: bufA=%s, bufB=%s", string(bufA), string(bufB))
			// }

			// // test cancelled read after a successful and a cancelled previous read
			// testCancelledRead(t, cr, timeout)

			// // test canceling before reading
			// if !cr.Cancel() {
			// 	t.Fatalf("canceling failed")
			// }

			// _, err = cr.Read(make([]byte, 1))
			// if !errors.Is(err, ErrCanceled) {
			// 	t.Fatalf("read returned \"%v\" insteaf of %q", err, ErrCanceled)
			// }
		})
	}
}

func testCancelledRead(tb testing.TB, cr CancelableReadWriteCloser, timeout time.Duration) {
	done := make(chan struct{})

	go func() {
		defer close(done)
		buf := make([]byte, 1)

		_, err := cr.Read(buf)
		if !errors.Is(err, ErrCanceled) {
			tb.Errorf("read returned \"%v\" instead of \"%v\"", err, ErrCanceled)
		}
	}()

	ok := cr.Cancel()
	if !ok {
		tb.Fatalf("cancelling was not successful")
	}

	select {
	case <-done:
	case <-time.After(timeout):
		tb.Fatalf("cancel did not interrupt read after %s", timeout)
	}
}

func testContextRead(tb testing.TB, cr CancelableReadWriteCloser, timeout time.Duration) {
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})

	go func() {
		defer close(done)
		buf := make([]byte, 1)

		_, err := cr.ReadContext(ctx, buf)
		if !errors.Is(err, ErrCanceled) {
			tb.Errorf("read returned \"%v\" instead of \"%v\"", err, ErrCanceled)
		}
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(timeout):
		tb.Fatalf("cancel did not interrupt read after %s", timeout)
	}
}

func TestWrite(t *testing.T) {
	const timeout = 5 * time.Second

	socketPath := filepath.Join(os.TempDir(),
		fmt.Sprintf("cancelreader_file_from_conn_%d", time.Now().Unix()))
	defer os.RemoveAll(socketPath)

	f, err := os.CreateTemp("", "test")
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	t.Cleanup(func() {
		err = f.Close()
		if err != nil {
			t.Fatalf("closing file")
		}

		os.Remove(f.Name())
	})

	testCases := []writerTestCase{
		// connWriterTestCase(t, "tcp4", "127.0.0.1:0"),
		pipeWriterTestCase(t),
		// {File: f, Reader: nil},
	}

	for _, testCase := range testCases {
		testCase := testCase

		t.Run(testCase.String(), func(t *testing.T) {
			cr, err := NewCancelReader(testCase.File)
			if err != nil {
				t.Fatalf("new cancel reader: %v", err)
			}

			// test cancelled read via context
			testCancelledWrite(t, cr, timeout)
		})
	}
}

func testCancelledWrite(tb testing.TB, cw CancelableReadWriteCloser, timeout time.Duration) {
	done := make(chan struct{})

	blockingBufferSize := 1024

	switch f := cw.File().(type) {
	case *connFile:
		switch c := f.Conn.(type) {
		case *net.TCPConn:
			err := c.SetWriteBuffer(1)
			if err != nil {
				tb.Fatalf("set write buffer: %v", err)
			}
		default:
			tb.Fatalf("test does not support %T", f)
		}
	case *os.File:
		blockingBufferSize = 1024 * 1024
	default:
		tb.Fatalf("test does not support %T", f)
	}

	go func() {
		defer close(done)

		_, err := cw.Write(make([]byte, blockingBufferSize))
		if !errors.Is(err, ErrCanceled) {
			tb.Errorf("write returned \"%v\" instead of \"%v\"", err, ErrCanceled)
		}
	}()

	select {
	case <-done:
		tb.Fatalf("write did not hang")
	case <-time.After(100 * time.Millisecond):
	}

	ok := cw.Cancel()
	if !ok {
		tb.Fatalf("cancelling was not successful")
	}

	select {
	case <-done:
	case <-time.After(timeout):
		tb.Fatalf("cancel did not interrupt read after %s", timeout)
	}
}

func testFileFromConn(tb testing.TB, conn net.Conn) File {
	tb.Helper()

	f, err := FileFromConn(conn)
	if err != nil {
		tb.Fatalf("file from conn: %v", err)
	}

	return f
}

func activeConns(tb testing.TB, network string, addr string) (net.Conn, net.Conn) {
	tb.Helper()

	if network != "unix" && isIPv6(tb, addr) && !hasLoopbackIPv6(tb) {
		tb.Skip()
	}

	if strings.HasPrefix(network, "udp") || network == "unixpacket" {
		return activeUDPConns(tb, network, addr)
	}

	listener, err := net.Listen(network, addr)
	if err != nil {
		tb.Fatalf("listen: %v", err)
	}

	secondConnReady := make(chan []byte)

	var wg sync.WaitGroup

	done := make(chan struct{})

	var dialerConn net.Conn

	wg.Add(1)
	go func() {
		defer wg.Done()

		conn, err := net.Dial(listener.Addr().Network(), listener.Addr().String())
		if err != nil {
			tb.Fatalf("dial: %v", err)
		}

		dialerConn = conn
		close(secondConnReady)

		<-done
	}()

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

	tb.Cleanup(func() { _ = listenerConn.Close })

	dialerConn, err := net.DialUDP(network, &net.UDPAddr{IP: ip, Port: 0}, listenerAddr)
	if err != nil {
		tb.Fatalf("dial: %v", err)
	}

	tb.Cleanup(func() { _ = dialerConn.Close })

	return listenerConn, dialerConn
}

type readerTestCase struct {
	File   File
	Writer io.Writer
}

func (rtc *readerTestCase) String() string {
	return rtc.File.Name()
}

func connReaderTestCase(tb testing.TB, network string, addr string) readerTestCase {
	tb.Helper()

	connA, connB := activeConns(tb, network, addr)

	return readerTestCase{File: testFileFromConn(tb, connA), Writer: connB}
}

func pipeReaderTestCase(tb testing.TB) readerTestCase {
	tb.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		tb.Fatalf("pipe: %v", err)
	}

	tb.Cleanup(func() {
		err = r.Close()
		if err != nil {
			tb.Fatalf("closing pipe reader: %v", err)
		}

		err = w.Close()
		if err != nil {
			tb.Fatalf("closing pipe writer: %v", err)
		}
	})

	return readerTestCase{File: r, Writer: w}
}

type writerTestCase struct {
	File   File
	Reader io.Reader
}

func (wtc *writerTestCase) String() string {
	return wtc.File.Name()
}

func connWriterTestCase(tb testing.TB, network string, addr string) writerTestCase {
	tb.Helper()

	connA, connB := activeConns(tb, network, addr)

	return writerTestCase{File: testFileFromConn(tb, connA), Reader: connB}
}

func pipeWriterTestCase(tb testing.TB) writerTestCase {
	tb.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		tb.Fatalf("pipe: %v", err)
	}

	tb.Cleanup(func() {
		err = r.Close()
		if err != nil {
			tb.Fatalf("closing pipe reader: %v", err)
		}

		err = w.Close()
		if err != nil {
			tb.Fatalf("closing pipe writer: %v", err)
		}
	})

	return writerTestCase{File: w, Reader: r}
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
			tb.Fatalf("listing addrs of interace %s: %v", iface.Name, err)
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
