//go:build windows

package ctxio

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"os"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestAnonymousPipe(t *testing.T) {
	t.Parallel()

	const timeout = 500 * time.Millisecond

	t.Run("read", func(t *testing.T) {
		t.Parallel()

		r, w, err := os.Pipe()
		if err != nil {
			t.Fatalf("pipe: %v", err)
		}
		defer w.Close()

		cr, err := WrapFile(r)
		if err != nil {
			t.Fatalf("WrapFile(pipe read end): %v", err)
		}

		if _, ok := cr.(*winAnonymousPipeIO); !ok {
			t.Fatalf("expected *winAnonymousPipeIO, got %T", cr)
		}

		testContextRead(t, cr, timeout)
		testCanceledRead(t, cr, timeout)
	})

	t.Run("write", func(t *testing.T) {
		t.Parallel()

		r, w, err := os.Pipe()
		if err != nil {
			t.Fatalf("pipe: %v", err)
		}
		defer r.Close()

		cw, err := WrapFile(w)
		if err != nil {
			t.Fatalf("WrapFile(pipe write end): %v", err)
		}

		if _, ok := cw.(*winAnonymousPipeIO); !ok {
			t.Fatalf("expected *winAnonymousPipeIO, got %T", cw)
		}

		testCanceledWrite(t, writerTestCase{CIO: cw, IsFile: true}, timeout)
	})
}

func TestNamedPipe(t *testing.T) {
	t.Parallel()

	const timeout = 500 * time.Millisecond

	t.Run("read", func(t *testing.T) {
		t.Parallel()

		server, client := createNamedPipePair(t)

		cr, err := WrapFile(server)
		if err != nil {
			t.Fatalf("new cancel reader: %v", err)
		}

		testContextRead(t, cr, timeout)
		testCanceledRead(t, cr, timeout)

		_ = client // keep client alive
	})

	t.Run("write", func(t *testing.T) {
		t.Parallel()

		server, client := createNamedPipePair(t)

		cr, err := WrapFile(server)
		if err != nil {
			t.Fatalf("new cancel reader: %v", err)
		}

		testCanceledWrite(t, writerTestCase{CIO: cr}, timeout)

		_ = client // keep client alive
	})
}

func TestReadFile(t *testing.T) {
	t.Parallel()

	f, err := os.CreateTemp(t.TempDir(), "ctxio-test-*")
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	defer os.Remove(f.Name())
	defer f.Close()

	cr, err := WrapFile(f)
	if err != nil {
		t.Fatalf("new cancel reader for file: %v", err)
	}
	defer cr.Close()

	// For regular files, Read doesn't block (returns EOF), so we just verify
	// that the constructor and basic read work without errors.
	buf := make([]byte, 16)

	n, err := cr.Read(buf)
	if err == nil && n == 0 {
		// Acceptable: 0 bytes read with no error
		return
	}

	if errors.Is(err, io.EOF) {
		return
	}

	// Windows returns syscall.Errno(38) for EOF on empty files
	errno, ok := errors.AsType[syscall.Errno](err)
	if ok && errno == 38 {
		return
	}

	t.Fatalf("expected EOF from empty file, got: %v (type: %T)", err, err)
}

func TestReadSocket(t *testing.T) {
	t.Parallel()

	const timeout = 500 * time.Millisecond

	connA, _ := activeConns(t, "tcp4", "127.0.0.1:0")

	cr, err := WrapConn(connA)
	if err != nil {
		t.Fatalf("new cancel reader: %v", err)
	}

	testContextRead(t, cr, timeout)
	testCanceledRead(t, cr, timeout)
}

// createNamedPipePair creates a connected named pipe pair. Returns (server
// *os.File, client *os.File). Both are cleaned up when the test finishes.
func createNamedPipePair(tb testing.TB) (*os.File, *os.File) { //nolint:unparam
	tb.Helper()

	pipeName := fmt.Sprintf(`\\.\pipe\ctxio-test-%d-%d`, os.Getpid(), rand.Int63())

	pipeNameUTF16, err := windows.UTF16PtrFromString(pipeName)
	if err != nil {
		tb.Fatalf("utf16 pipe name: %v", err)
	}

	serverHandle, err := windows.CreateNamedPipe(
		pipeNameUTF16,
		windows.PIPE_ACCESS_DUPLEX|windows.FILE_FLAG_OVERLAPPED,
		windows.PIPE_TYPE_BYTE|windows.PIPE_READMODE_BYTE|windows.PIPE_WAIT,
		1,    // max instances
		4096, // out buffer size
		4096, // in buffer size
		0,    // default timeout
		nil,  // default security
	)
	if err != nil {
		tb.Fatalf("CreateNamedPipe: %v", err)
	}

	server := os.NewFile(uintptr(serverHandle), pipeName)

	tb.Cleanup(func() { server.Close() })

	// Connect client in a goroutine since ConnectNamedPipe blocks until a
	// client connects.
	clientReady := make(chan *os.File, 1)

	go func() {
		clientHandle, err := windows.CreateFile(
			pipeNameUTF16,
			windows.GENERIC_READ|windows.GENERIC_WRITE,
			0, nil, windows.OPEN_EXISTING,
			windows.FILE_FLAG_OVERLAPPED, 0)
		if err != nil {
			tb.Errorf("open named pipe client: %v", err)

			clientReady <- nil

			return
		}

		clientReady <- os.NewFile(uintptr(clientHandle), pipeName+"-client")
	}()

	// ConnectNamedPipe on an overlapped handle requires a non-nil OVERLAPPED
	// structure. With nil, the call has undefined behavior and may block
	// forever.
	overlapped := &windows.Overlapped{}

	overlapped.HEvent, err = windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		tb.Fatalf("CreateEvent: %v", err)
	}
	defer windows.CloseHandle(overlapped.HEvent)

	err = windows.ConnectNamedPipe(serverHandle, overlapped)
	if err != nil && !errors.Is(err, windows.ERROR_IO_PENDING) &&
		!errors.Is(err, windows.ERROR_PIPE_CONNECTED) {
		tb.Fatalf("ConnectNamedPipe: %v", err)
	}

	if errors.Is(err, windows.ERROR_IO_PENDING) {
		_, err = windows.WaitForSingleObject(overlapped.HEvent, 5000)
		if err != nil {
			tb.Fatalf("WaitForSingleObject: %v", err)
		}
	}

	client := <-clientReady
	if client == nil {
		tb.Fatal("client connection failed")
	}

	tb.Cleanup(func() { client.Close() })

	return server, client
}

// Verify connFile is detected as a socket path.
func TestSocketDetection(t *testing.T) {
	t.Parallel()

	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	connCh := make(chan net.Conn, 1)

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}

		connCh <- conn
	}()

	clientConn, err := (&net.Dialer{}).DialContext(context.Background(), "tcp4", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer clientConn.Close()

	serverConn := <-connCh
	defer serverConn.Close()

	cr, err := WrapConn(clientConn)
	if err != nil {
		t.Fatalf("WrapConn: %v", err)
	}
	defer cr.Close()

	// Verify it's a winSocketIO.
	if _, ok := cr.(*deadlineIO); !ok {
		t.Fatalf("expected *deadlineIO, got %T", cr)
	}
}
