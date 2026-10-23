package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/cli"
	"github.com/DanielNetzer/forgecell/lab/internal/resources"
)

var version = "dev-go"

func main() {
	// Every start sweeps the current user's leaked forgecell-* temp directories: those
	// whose owner pid is dead, or that have no owner file and are over a day old.
	if _, err := resources.SweepStale(os.TempDir(), time.Now()); err != nil {
		fmt.Fprintln(os.Stderr, "forgecell: stale temp sweep:", err)
	}
	// The first SIGINT or SIGTERM cancels the run; a second one ends the process at once.
	// Both paths, and a normal return, remove every registered temp directory first.
	// Locks are flocks the operating system drops on exit.
	os.Exit(resources.RunWithCleanup(func(ctx context.Context) int {
		return cli.Run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr, version)
	}, os.Stderr))
}
