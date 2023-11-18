package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/erikgeiser/ctxio"
	"golang.org/x/term"
)

func run() error {
	state, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return fmt.Errorf("set terminal to raw mode: %w", err)
	}

	defer term.Restore(int(os.Stdin.Fd()), state) // nolint:errcheck

	crwc, err := ctxio.NewCancelReader(os.Stdin)
	if err != nil {
		return fmt.Errorf("create cancel reader: %w", err)
	}

	fmt.Printf("Testing %T\n\r", crwc)

	timeout := 3 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	fmt.Printf("Canceling reader after %s\n\r", timeout)

	buffer := make([]byte, 32)

	for {
		n, err := crwc.ReadContext(ctx, buffer)
		if err != nil {
			fmt.Printf("read: %v\n\r", err)

			break
		}

		fmt.Printf("received: %s\n\r", buffer[:n])
	}

	fmt.Print("Trying to read from stdin again:\n\r")

	n, err := os.Stdin.Read(buffer)
	if err != nil {
		return fmt.Errorf("read after cancelation: %w", err)
	}

	fmt.Printf("received: %s\n\r", buffer[:n])

	return nil
}

func main() {
	err := run()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)

		os.Exit(1)
	}
}
