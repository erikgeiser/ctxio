//go:build linux

package ctxio

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/iceber/iouring-go"
)

func NewIOUringCancelReader(file File) (CancelableReadWriteCloser, error) {
	if int(file.Fd()) == -1 {
		return nil, fmt.Errorf("unsuitable file: %w", os.ErrClosed)
	}

	iour, err := iouring.New(2)
	if err != nil {
		return nil, fmt.Errorf("create io_uring: %w", err)
	}

	return &iouringCancelReader{file: file, iour: iour, cancel: make(chan struct{})}, nil
}

type iouringCancelReader struct {
	file File
	iour *iouring.IOURing

	cancel      chan struct{}
	cancelMutex sync.Mutex

	cancelStateMixin
}

func (r *iouringCancelReader) File() File {
	return r.file
}

func (r *iouringCancelReader) ReadContext(ctx context.Context, data []byte) (int, error) {
	done := cancelWhenContextIsDone(ctx, r)
	defer done()

	return r.Read(data)
}

func (r *iouringCancelReader) Read(data []byte) (int, error) {
	return r.runRequest(iouring.Read(int(r.file.Fd()), data))
}

func (r *iouringCancelReader) WriteContext(ctx context.Context, data []byte) (int, error) {
	done := cancelWhenContextIsDone(ctx, r)
	defer done()

	return r.Write(data)
}

func (r *iouringCancelReader) Write(data []byte) (int, error) {
	return r.runRequest(iouring.Write(int(r.file.Fd()), data))
}

func (r *iouringCancelReader) runRequest(request iouring.PrepRequest) (int, error) {
	if r.cancelStateMixin.isCanceled() {
		r.cancelStateMixin.Reset()
		return 0, ErrCanceled
	}
	resultChan := make(chan iouring.Result, 1)

	submittedRequest, err := r.iour.SubmitRequest(request, resultChan)
	if err != nil {
		return 0, fmt.Errorf("submit request: %w", err)
	}

	select {
	case <-r.cancel:
		r.cancel = make(chan struct{})
		r.cancelMutex.Lock()
		r.cancelStateMixin.Reset()
		r.cancelMutex.Unlock()

		_, err := submittedRequest.Cancel()
		if errors.Is(err, iouring.ErrRequestCompleted) {
			return (<-resultChan).ReturnInt()
		} else if err != nil {
			go func() { <-resultChan }()
			return 0, errors.Join(ErrCanceled, err)
		}

		return 0, ErrCanceled
	case res := <-resultChan:
		return res.ReturnInt()
	}
}

func (r *iouringCancelReader) Cancel() bool {
	r.cancelStateMixin.setCanceled()
	r.cancelMutex.Lock()
	close(r.cancel)
	r.cancelMutex.Unlock()

	return true
}

func (r *iouringCancelReader) Close() error {
	return r.iour.Close()
}
