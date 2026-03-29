//go:build darwin || freebsd || netbsd || openbsd

package ctxio

import (
	"os"
	"testing"
)

func TestWrapFilePipe(t *testing.T) {
	t.Parallel()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}

	defer r.Close()
	defer w.Close()

	cio, err := WrapFile(r)
	if err != nil {
		t.Fatalf("WrapFile(pipe): %v", err)
	}

	defer cio.Close()

	if _, ok := cio.(*kqueueIO); !ok {
		t.Errorf("WrapFile(pipe) = %T, want *kqueueIO", cio)
	}
}

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

	if _, ok := cio.(*kqueueIO); !ok {
		t.Errorf("WrapFile(regular file) = %T, want *kqueueIO", cio)
	}
}

func TestWrapConnTCP(t *testing.T) {
	t.Parallel()

	conn, _ := activeConns(t, "tcp4", "127.0.0.1:0")

	cio, err := WrapConn(conn)
	if err != nil {
		t.Fatalf("WrapConn(tcp): %v", err)
	}

	defer cio.Close()

	if _, ok := cio.(*kqueueIO); !ok {
		t.Errorf("WrapConn(tcp) = %T, want *kqueueIO", cio)
	}
}

func TestWrapFileTTY(t *testing.T) {
	t.Parallel()

	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("skipping: cannot open /dev/tty: %v", err)
	}

	defer tty.Close()

	cio, err := WrapFile(tty)
	if err != nil {
		t.Fatalf("WrapFile(/dev/tty): %v", err)
	}

	defer cio.Close()

	if _, ok := cio.(*selectIO); !ok {
		t.Errorf("WrapFile(/dev/tty) = %T, want *selectIO", cio)
	}
}

func TestWrapConnDeadlineFallback(t *testing.T) {
	t.Parallel()

	conn, _ := activeConns(t, "tcp4", "127.0.0.1:0")

	cio, err := WrapConn(deadlinerOnlyConn{Conn: conn})
	if err != nil {
		t.Fatalf("WrapConn(deadlinerOnlyConn): %v", err)
	}

	defer cio.Close()

	if _, ok := cio.(*deadlineIO); !ok {
		t.Errorf("WrapConn(deadlinerOnlyConn) = %T, want *deadlineIO", cio)
	}
}
