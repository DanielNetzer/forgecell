package main

import (
	"context"
	"github.com/DanielNetzer/forgecell/lab/internal/cli"
	"os"
	"os/signal"
	"syscall"
)

var version = "dev-go"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// After the first signal, restore default handling so a second one ends the
	// process at once. Locks are flocks the operating system drops on exit.
	go func() {
		<-ctx.Done()
		stop()
	}()
	os.Exit(cli.Run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr, version))
}
