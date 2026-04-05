//go:build windows

package ctxio

import (
	"context"
	"errors"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	procAllocConsole       = modkernel32.NewProc("AllocConsole")
	procFreeConsole        = modkernel32.NewProc("FreeConsole")
	procWriteConsoleInputW = modkernel32.NewProc("WriteConsoleInputW")
)

// inputRecord corresponds to the INPUT_RECORD structure.
type inputRecord struct {
	EventType uint16
	_         [2]byte // padding
	Event     [16]byte
}

// keyEventRecord corresponds to the KEY_EVENT_RECORD structure.
type keyEventRecord struct {
	KeyDown         int32
	RepeatCount     uint16
	VirtualKeyCode  uint16
	VirtualScanCode uint16
	UnicodeChar     uint16
	ControlKeyState uint32
}

const (
	keyEventType              = 0x0001
	focusEventType            = 0x0010
	windowBufferSizeEventType = 0x0004
)

// focusEventRecord corresponds to the FOCUS_EVENT_RECORD structure.
type focusEventRecord struct {
	SetFocus int32
}

// windowBufferSizeRecord corresponds to the WINDOW_BUFFER_SIZE_RECORD structure.
type windowBufferSizeRecord struct {
	SizeX int16
	SizeY int16
}

// writeNonCharEvents injects non-character console events (focus + window
// resize) into the input buffer. This reproduces the CI condition where
// AllocConsole triggers system events that cause ReadFile to return 0 bytes.
func writeNonCharEvents(t *testing.T, conin windows.Handle) {
	t.Helper()

	records := []inputRecord{
		func() inputRecord {
			var rec inputRecord

			rec.EventType = focusEventType
			*(*focusEventRecord)(unsafe.Pointer(&rec.Event[0])) = focusEventRecord{SetFocus: 1}

			return rec
		}(),
		func() inputRecord {
			var rec inputRecord

			rec.EventType = windowBufferSizeEventType
			*(*windowBufferSizeRecord)(unsafe.Pointer(&rec.Event[0])) = windowBufferSizeRecord{SizeX: 80, SizeY: 24}

			return rec
		}(),
	}

	var written uint32

	r, _, err := procWriteConsoleInputW.Call(
		uintptr(conin),
		uintptr(unsafe.Pointer(&records[0])),
		uintptr(len(records)),
		uintptr(unsafe.Pointer(&written)),
	)
	if r == 0 {
		t.Fatalf("WriteConsoleInputW (non-char events): %v", err)
	}
}

func ensureConsole(t *testing.T) {
	t.Helper()

	r, _, _ := procAllocConsole.Call()
	if r != 0 {
		t.Cleanup(func() {
			procFreeConsole.Call()
		})
	}
}

// writeConsoleInput injects key-down events for each byte into the console
// input buffer via WriteConsoleInputW.
func writeConsoleInput(t *testing.T, conin windows.Handle, data []byte) {
	t.Helper()

	records := make([]inputRecord, len(data))

	for i, b := range data {
		var ker keyEventRecord

		ker.KeyDown = 1
		ker.RepeatCount = 1
		ker.UnicodeChar = uint16(b)

		var rec inputRecord

		rec.EventType = keyEventType
		*(*keyEventRecord)(unsafe.Pointer(&rec.Event[0])) = ker
		records[i] = rec
	}

	var written uint32

	r, _, err := procWriteConsoleInputW.Call(
		uintptr(conin),
		uintptr(unsafe.Pointer(&records[0])),
		uintptr(len(records)),
		uintptr(unsafe.Pointer(&written)),
	)
	if r == 0 {
		t.Fatalf("WriteConsoleInputW: %v", err)
	}
}

func openTestConsoleReader(t *testing.T) (ContextIO, windows.Handle) {
	t.Helper()

	ensureConsole(t)

	cio, err := openConsoleReader()
	if err != nil {
		t.Fatalf("openConsoleReader: %v", err)
	}

	t.Cleanup(func() { cio.Close() })

	return cio, cio.conin
}

func TestConsoleCanceledRead(t *testing.T) { //nolint:paralleltest
	const timeout = 500 * time.Millisecond

	cio, _ := openTestConsoleReader(t)

	testCanceledRead(t, cio, timeout)
}

func TestConsoleContextRead(t *testing.T) { //nolint:paralleltest
	const timeout = 500 * time.Millisecond

	cio, _ := openTestConsoleReader(t)

	testContextRead(t, cio, timeout)
}

func TestConsoleReadData(t *testing.T) { //nolint:paralleltest
	cio, conin := openTestConsoleReader(t)

	input := []byte("hello")

	writeConsoleInput(t, conin, input)

	buf := make([]byte, 256)

	done := make(chan int, 1)

	go func() {
		n, err := cio.Read(buf)
		if err != nil {
			t.Errorf("Read: %v", err)
		}

		done <- n
	}()

	select {
	case n := <-done:
		// Console reads return INPUT_RECORDs as raw bytes, so we verify we got
		// some data back.
		if n == 0 {
			t.Fatal("expected data from console read, got 0 bytes")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("console read did not return in time")
	}
}

// TestConsoleReadDataWithNonCharEvents reproduces the CI failure mode where
// AllocConsole triggers focus/resize events that precede the actual key events.
// Without the retry loop in read(), ReadFile consumes a non-character event and
// returns 0 bytes, causing the test to report 0 bytes read.
func TestConsoleReadDataWithNonCharEvents(t *testing.T) { //nolint:paralleltest
	cio, conin := openTestConsoleReader(t)

	// Inject non-character events first, then the actual key data — this is
	// what happens in CI after AllocConsole.
	writeNonCharEvents(t, conin)
	writeConsoleInput(t, conin, []byte("hello"))

	buf := make([]byte, 256)
	done := make(chan int, 1)

	go func() {
		n, err := cio.Read(buf)
		if err != nil {
			t.Errorf("Read: %v", err)
		}

		done <- n
	}()

	select {
	case n := <-done:
		if n == 0 {
			t.Fatal("expected data from console read, got 0 bytes")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("console read did not return in time")
	}
}

func TestConsoleAlreadyCanceled(t *testing.T) { //nolint:paralleltest
	cio, _ := openTestConsoleReader(t)

	cio.Cancel()

	_, err := cio.Read(make([]byte, 1))
	if !errors.Is(err, ErrCanceled) {
		t.Fatalf("expected ErrCanceled, got %v", err)
	}
}

func TestConsoleAlreadyCanceledContext(t *testing.T) { //nolint:paralleltest
	cio, _ := openTestConsoleReader(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan error, 1)

	go func() {
		_, err := cio.ReadContext(ctx, make([]byte, 1))
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, ErrCanceled) {
			t.Fatalf("expected ErrCanceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ReadContext with canceled context did not return in time")
	}
}

func TestConsoleDoubleCancel(t *testing.T) { //nolint:paralleltest
	cio, _ := openTestConsoleReader(t)

	cio.Cancel()
	cio.Cancel()
}
