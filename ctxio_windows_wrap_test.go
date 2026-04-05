//go:build windows

package ctxio

import (
	"context"
	"net"
	"os"
	"testing"
	"unicode/utf16"

	"golang.org/x/sys/windows"
)

// TestWrapFileAnonymousPipe verifies that WrapFile on an anonymous pipe uses
// the winAnonymousPipeIO backend (CancelSynchronousIo) since anonymous pipes
// cannot be reopened with FILE_FLAG_OVERLAPPED.
func TestWrapFileAnonymousPipe(t *testing.T) {
	t.Parallel()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}

	defer w.Close()

	cio, err := WrapFile(r)
	if err != nil {
		t.Fatalf("WrapFile(anonymous pipe): %v", err)
	}

	defer cio.Close()

	if _, ok := cio.(*winAnonymousPipeIO); !ok {
		t.Errorf("WrapFile(anonymous pipe) = %T, want *winAnonymousPipeIO", cio)
	}
}

// TestWrapFileNamedPipe verifies that WrapFile on a named pipe uses the
// overlapped I/O backend.
func TestWrapFileNamedPipe(t *testing.T) {
	t.Parallel()

	server, _ := createNamedPipePair(t)

	cio, err := WrapFile(server)
	if err != nil {
		t.Fatalf("WrapFile(named pipe): %v", err)
	}

	defer cio.Close()

	if _, ok := cio.(*winOverlappedIO); !ok {
		t.Errorf("WrapFile(named pipe) = %T, want *winOverlappedIO", cio)
	}
}

// TestWrapFileRegular verifies that WrapFile on a regular disk file uses the
// overlapped I/O backend.
func TestWrapFileRegular(t *testing.T) {
	t.Parallel()

	f, err := os.CreateTemp(t.TempDir(), "ctxio-wrap-test-*")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}

	defer f.Close()

	cio, err := WrapFile(f)
	if err != nil {
		t.Fatalf("WrapFile(regular file): %v", err)
	}

	defer cio.Close()

	if _, ok := cio.(*winOverlappedIO); !ok {
		t.Errorf("WrapFile(regular file) = %T, want *winOverlappedIO", cio)
	}
}

// TestWrapFileConsole verifies that WrapFile on a console handle uses the
// winConsoleIO backend on Windows.
func TestWrapFileConsole(t *testing.T) {
	t.Parallel()

	ensureConsole(t)

	handle, err := windows.CreateFile(
		&(utf16.Encode([]rune("CONIN$\x00"))[0]),
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		fileShareValidFlags,
		nil,
		windows.OPEN_EXISTING,
		0,
		0,
	)
	if err != nil {
		t.Skipf("skipping: cannot open CONIN$: %v", err)
	}

	f := os.NewFile(uintptr(handle), "CONIN$")

	defer f.Close()

	cio, err := WrapFile(f)
	if err != nil {
		t.Fatalf("WrapFile(CONIN$): %v", err)
	}

	defer cio.Close()

	if _, ok := cio.(*winConsoleIO); !ok {
		t.Errorf("WrapFile(CONIN$) = %T, want *winConsoleIO", cio)
	}
}

// TestWrapConnTCP verifies that WrapConn on a TCP connection uses the deadline
// backend on Windows (sockets use deadline-based cancellation).
func TestWrapConnTCP(t *testing.T) {
	t.Parallel()

	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	defer ln.Close()

	connCh := make(chan net.Conn, 1)

	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}

		connCh <- c
	}()

	client, err := (&net.Dialer{}).DialContext(context.Background(), "tcp4", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	server := <-connCh

	defer client.Close()
	defer server.Close()

	cio, err := WrapConn(client)
	if err != nil {
		t.Fatalf("WrapConn(tcp): %v", err)
	}

	defer cio.Close()

	if _, ok := cio.(*deadlineIO); !ok {
		t.Errorf("WrapConn(tcp) = %T, want *deadlineIO", cio)
	}
}
