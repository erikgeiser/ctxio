<p align="center">
  <h1 align="center"><b>ctxio</b></h1>
  <p align="center"><i>Context-aware, cancelable I/O for Go.</i></p>
  <p align="center">
    <a href="https://github.com/erikgeiser/ctxio/releases/latest"><img alt="Release" src="https://img.shields.io/github/release/erikgeiser/ctxio.svg?style=for-the-badge"></a>
    <a href="https://pkg.go.dev/github.com/erikgeiser/ctxio"><img alt="Go Doc" src="https://img.shields.io/badge/godoc-reference-blue.svg?style=for-the-badge"></a>
    <a href="https://github.com/erikgeiser/ctxio/actions?workflow=CI"><img alt="GitHub Action: CI" src="https://img.shields.io/github/actions/workflow/status/erikgeiser/ctxio/ci.yml?branch=main&style=for-the-badge"></a>
    <a href="/LICENSE"><img alt="Software License" src="https://img.shields.io/badge/license-MIT-brightgreen.svg?style=for-the-badge"></a>
  </p>
</p>

`ctxio` wraps files, network connections, pipes, TTYs, etc. so that reads and
writes can be canceled via a `context.Context` without consuming data from the
underlying file descriptor. This is useful when you need to interrupt a blocking
`Read` or `Write` and then continue using the same connection or file
afterwards.

The origin of `ctxio` is [this PR](https://github.com/charmbracelet/bubbletea/pull/120) for the [bubbletea](https://github.com/charmbracelet/bubbletea) library, which solves the more specific use-case: A read on `os.Stdin` which may block due to a lack of input needs to be interrupted while ensuring that the next read won't miss input bytes.

## Cancelation Mechanisms

The standard library does not support canceling a blocking read without closing
the file descriptor. `ctxio` solves this by using OS-level I/O multiplexing
primitives to wait for data _before_ issuing the actual read or write:

| Platform     | Mechanism                                                                  |
| ------------ | -------------------------------------------------------------------------- |
| Linux        | `epoll`, `select`, deadlines                                               |
| macOS, BSD   | `kqueue`, `select`, deadline                                               |
| Windows      | Overlapped I/O, `WaitForMultipleObjects`, deadlines, `CancelSynchronousIo` |
| Generic Unix | `select` , deadlines                                                       |
| Others       | deadlines                                                                  |

When the context is canceled, the wait is interrupted and `ErrCanceled` is
returned. No data is lost because the actual `read(2)` / `write(2)` never
happened. Note that the `io_uring` mechanism for Linux was conciously not
implemented because it is disabled by default or compiled out of the kernel on
some distributions due to security concerns while the `epoll` backend achieves
the same results reliably.

## Usage

Use a suitable `Wrap*` for your IO object and use either the context-aware
`ReadContext`/`WriteContext` methods or the regular `io.ReadWriter` interface
together with `Cancel()`, `CancelReads()`, `CancelWrites()`, `Reset()`,
`ResetReader()` and `ResetWriter()`.

```go
rawTTY, _ := os.Open("/dev/tty")
tty, _ := ctxio.WrapFile(rawTTY)
defer tty.Close()

buf := make([]byte, 256)

ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
defer cancel()

// cancel with a context
n, err := tty.ReadContext(ctx, buf)
if errors.Is(err, ctxio.ErrCanceled) {
    fmt.Println("read timed out, but the file is still usable")
}

tty.Reset()

// or cancel explicitly
go func() {
  time.Sleep(1 * time.Second)
  tty.Cancel()
} ()

_, err = tty.Read(buf)
```

## Cancelation Methods

There are two ways to cancel an operation:

1. **Context-based**: Use `ReadContext` / `WriteContext` with a
   cancelable context.
2. **Explicit**: Call `Cancel()` directly. This is useful when working with APIs
   that expect a plain `io.Reader` / `io.Writer`. The `Read` and `Write`
   methods on `ContextIO` can be interrupted by calling `Cancel()`,
   `CancelReads()` or `CancelWrites()` from another goroutine.

### Cancelation and Reset Semantics

When `Cancel()` (or `CancelReads()` / `CancelWrites()`) is called, it affects
subsequent operations differently depending on which method you use:

- **`Read` / `Write`** return `ErrCanceled` immediately **until** the
  corresponding `Reset()` (or `ResetReader()` / `ResetWriter()`) is called.
- **`ReadContext` / `WriteContext`** with a fresh context automatically clear
  the canceled state before starting, so a fresh context is sufficient to resume
  I/O without an explicit `Reset()` call.

```go
cio, _ := ctxio.WrapFile(file)

cio.Cancel()
cio.Read(buf)  // returns ErrCanceled (sticky)

// ReadContext auto-clears the canceled state and proceeds normally:
ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
defer cancel()
cio.ReadContext(ctx, buf)  // proceeds normally

// To resume plain Read/Write after Cancel, call Reset explicitly:
cio.Cancel()
cio.Reset()
cio.Read(buf)  // now works again
```

### Bidirectional proxying

`Connect` copies data between two `ContextIO` values in both directions and
respects context cancelation and in case an error occurs, it returns this error
instead of subsequent ones that result from the broken connection.

```go
cioA, _ := ctxio.WrapFile(fileA)
cioB, _ := ctxio.WrapFile(fileB)

err := ctxio.Connect(ctx, cioA, cioB)
```

`ConnectAndClose` does the same for arbitrary io.ReadWriteClosers. It achieves
this by closing both sides when an error occurs or the context is cancelled.

```go
err := ctxio.ConnectAndClose(ctx, connA, connB)
```

## Closable Backend

For objects that don't support the standard mechanisms (e.g., pipes, streams
from external libraries), the closable backend with `WrapClosableReader`,
`WrapClosableWriter`, or `WrapClosableReadWriter`, which provides the same API
with some caveats:

- Cancellation works by **closing the underlying object**, making it permanently
  unusable after cancellation.
- The object **cannot be reused** after `Cancel()` is called, even after calling
  `Reset()`, `Reset()` only clears the cancellation flag.
- This backend is best used for one-shot operations or when the object is
  expected to be closed anyway.
