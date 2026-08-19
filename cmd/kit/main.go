package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"kit/internal/app"
	"kit/internal/clierror"
)

func main() {
	// Leave SIGINT on its default action while the terminal is in canonical mode,
	// so a blocked confirmation read still terminates with the conventional 130.
	// The raw selector receives Ctrl-C as a byte and maps it to the same code.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stop()

	application := app.New(os.Stdin, os.Stdout, os.Stderr)
	err := application.Run(ctx, os.Args[1:])
	if err == nil {
		return
	}
	code := clierror.Code(err)
	if errors.Is(ctx.Err(), context.Canceled) {
		code = clierror.Interrupt
	}
	fmt.Fprintf(os.Stderr, "kit ! 오류\n\n  %s\n", err)
	os.Exit(code)
}
