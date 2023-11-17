package ctxio

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"golang.org/x/sync/errgroup"
)

func Connect(ctx context.Context, a File, b File) error {
	cancelableA, err := NewCancelReader(a)
	if err != nil {
		return err
	}

	cancelableB, err := NewCancelReader(b)
	if err != nil {
		return err
	}

	eg, ctx := errgroup.WithContext(ctx)

	copyDone := make(chan struct{})

	ctxMonitoringDone := make(chan struct{})
	defer func() { <-ctxMonitoringDone }()

	go func() {
		defer close(ctxMonitoringDone)

		select {
		case <-ctx.Done():
			cancelableA.Cancel()
			cancelableB.Cancel()
		case <-copyDone:
		}
	}()

	eg.Go(func() error {
		_, err := io.Copy(a, cancelableB)
		if err != nil {
			return fmt.Errorf("%s -> %s: %w", b, a, err)
		}

		return nil
	})

	eg.Go(func() error {
		_, err := io.Copy(b, cancelableA)
		if err != nil {
			return fmt.Errorf("%s -> %s: %w", a, b, err)
		}

		return nil
	})

	err = eg.Wait()
	close(copyDone)

	return err
}

func ConnectAndClose(ctx context.Context, a io.ReadWriteCloser, b io.ReadWriteCloser, closeBothOnError bool) error {
	copyDone := make(chan struct{})
	defer func() { close(copyDone) }()

	var closedByUs atomicFlag

	closeBoth := func() {
		closedByUs.Set()
		_ = a.Close()
		_ = b.Close()
	}

	if !closeBothOnError {
		defer closeBoth()
	}

	go func() {
		select {
		case <-ctx.Done():
			closeBoth()
		case <-copyDone:
		}
	}()

	eg, _ := errgroup.WithContext(ctx)

	eg.Go(func() error {
		if closeBothOnError {
			defer closeBoth()
		}

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
		if closeBothOnError {
			defer closeBoth()
		}

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
			return errors.Join(ctx.Err(), err)
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
	ab.Mutex.Lock()
	defer ab.Mutex.Unlock()

	ab.bool = true
}

func (ab *atomicFlag) IsSet() bool {
	ab.Mutex.Lock()
	defer ab.Mutex.Unlock()

	return ab.bool
}
