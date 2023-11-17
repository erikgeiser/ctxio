package ctxio

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"syscall"
)

// ErrCanceled is returned when a read was canceled.
var ErrCanceled = fmt.Errorf("read cancelled")

type File interface {
	io.ReadWriter
	Fd() uintptr
	Name() string
}

type connFile struct {
	net.Conn
	fd uintptr
}

func (cf *connFile) Fd() uintptr {
	return cf.fd
}

func (cf *connFile) Name() string {
	connType := fmt.Sprintf("%T", cf.Conn)
	connType = strings.TrimPrefix(connType, "*")
	connType = strings.TrimPrefix(connType, "net.")
	connType = strings.TrimSuffix(connType, "{}")

	link := cf.Conn.LocalAddr().String()
	if cf.Conn.RemoteAddr() != nil && cf.Conn.RemoteAddr().String() != "" {
		link += "<->" + cf.Conn.RemoteAddr().String()
	}

	return fmt.Sprintf("%s(%s)", connType, link)
}

type syscallConner interface {
	SyscallConn() (syscall.RawConn, error)
}

var (
	_ syscallConner = &net.TCPConn{}
	_ syscallConner = &net.UDPConn{}
	_ syscallConner = &net.UDPConn{}
	_ syscallConner = &net.IPConn{}
)

func FileFromConn(conn net.Conn) (File, error) {
	c, ok := conn.(syscallConner)
	if !ok {
		return nil, fmt.Errorf("cannot determine file descriptor of %T", conn)
	}

	rawConn, err := c.SyscallConn()
	if err != nil {
		return nil, err
	}

	var fd uintptr

	err = rawConn.Control(func(passedFd uintptr) { fd = passedFd })
	if err != nil {
		return nil, fmt.Errorf("")
	}

	return &connFile{Conn: conn, fd: fd}, nil
}

var _ File = &os.File{}

// CancelableReadWriteCloser is a io.Reader whose Read() calls can be cancelled without data
// being consumed. The CancelableReadWriteCloser has to be closed.
type CancelableReadWriteCloser interface {
	ReadContext(context.Context, []byte) (int, error)
	WriteContext(context.Context, []byte) (int, error)
	io.ReadWriteCloser

	File() File
	// Cancel cancels ongoing and future reads an returns true if it succeeded.
	Cancel() bool
}

func cancelWhenContextIsDone(ctx context.Context, crwc CancelableReadWriteCloser) func() {
	wg := sync.WaitGroup{}
	operationFinished := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()

		select {
		case <-ctx.Done():
			crwc.Cancel()
		case <-operationFinished:
			return
		}
	}()

	return func() {
		close(operationFinished)
		wg.Wait()
	}
}

// cancelStateMixin represents a goroutine-safe cancelation status.
type cancelStateMixin struct {
	unsafeCanceled bool
	lock           sync.Mutex
}

func (c *cancelStateMixin) isCanceled() bool {
	c.lock.Lock()
	defer c.lock.Unlock()

	return c.unsafeCanceled
}

func (c *cancelStateMixin) setCanceled() {
	c.lock.Lock()
	defer c.lock.Unlock()

	c.unsafeCanceled = true
}

func (c *cancelStateMixin) Reset() {
	c.lock.Lock()
	defer c.lock.Unlock()

	c.unsafeCanceled = false
}
