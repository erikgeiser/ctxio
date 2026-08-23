package ctxio

import (
	"context"
	"fmt"
	"io"
	"sync"

	"golang.org/x/sync/errgroup"
)

// Connect copies data between a and b in both directions until an error occurs
// or ctx is canceled. It uses the ContextIO cancellation mechanism so that no
// data is consumed from either side when the context expires. If both copy
// directions fail, Connect returns the first error.
func Connect(ctx context.Context, a ContextIO, b ContextIO) error {
	eg, ctx := errgroup.WithContext(ctx)

	copyDone := make(chan struct{})

	ctxMonitoringDone := make(chan struct{})

	defer func() { <-ctxMonitoringDone }()

	go func() {
		defer close(ctxMonitoringDone)

		select {
		case <-ctx.Done():
			a.Cancel()
			b.Cancel()
		case <-copyDone:
		}
	}()

	eg.Go(func() error {
		_, err := io.Copy(a, b)
		if err != nil {
			return fmt.Errorf("%s -> %s: %w", b.Name(), a.Name(), err)
		}

		return nil
	})

	eg.Go(func() error {
		_, err := io.Copy(b, a)
		if err != nil {
			return fmt.Errorf("%s -> %s: %w", a.Name(), b.Name(), err)
		}

		return nil
	})

	err := eg.Wait()

	close(copyDone)

	return err
}

// ConnectAndClose is a convenience wrapper around Connect that accepts plain
// io.ReadWriteCloser values and closes both sides when it returns.
func ConnectAndClose(ctx context.Context, a io.ReadWriteCloser, b io.ReadWriteCloser) error {
	// Fallback: cancel by closing both sides.
	var (
		closedByUs atomicFlag
		closeOnce  sync.Once
	)

	forceCloseBoth := func() {
		closeOnce.Do(func() {
			closedByUs.Set()

			_ = a.Close()
			_ = b.Close()
		})
	}

	eg, groupCtx := errgroup.WithContext(ctx)

	stopClosing := context.AfterFunc(groupCtx, forceCloseBoth)
	defer func() {
		stopClosing()
		forceCloseBoth()
	}()

	eg.Go(func() error {
		_, err := io.Copy(a, b)
		if err != nil {
			if closedByUs.IsSet() {
				return ctx.Err()
			}

			return err
		}

		return nil
	})

	eg.Go(func() error {
		_, err := io.Copy(b, a)
		if err != nil {
			if closedByUs.IsSet() {
				return ctx.Err()
			}

			return err
		}

		return nil
	})

	err := eg.Wait()
	if err != nil {
		if ctx.Err() != nil {
			return joinErrors(ctx.Err(), err)
		}

		return err
	}

	return ctx.Err()
}

type atomicFlag struct {
	bool
	sync.Mutex
}

func (ab *atomicFlag) Set() {
	ab.Lock()
	defer ab.Unlock()

	ab.bool = true
}

func (ab *atomicFlag) IsSet() bool {
	ab.Lock()
	defer ab.Unlock()

	return ab.bool
}
