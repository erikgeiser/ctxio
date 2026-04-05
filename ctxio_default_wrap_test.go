//go:build !darwin && !windows && !linux && !solaris && !freebsd && !netbsd && !openbsd

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

	if _, ok := cio.(*deadlineIO); !ok {
		t.Errorf("WrapFile(pipe) = %T, want *deadlineIO", cio)
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

	if _, ok := cio.(*deadlineIO); !ok {
		t.Errorf("WrapConn(tcp) = %T, want *deadlineIO", cio)
	}
}
