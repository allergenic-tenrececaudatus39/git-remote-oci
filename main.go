package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/mrueg/git-remote-oci/pkg/cli"
)

// version is overridden at release time via -ldflags.
var version = "dev"

func main() {
	// run() owns every defer; main only turns its result into an exit code, so
	// that signal handling is torn down before the process exits.
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "git-remote-oci: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return cli.Run(ctx, cli.Env{
		Args:    os.Args[1:],
		Version: version,
		Stdin:   os.Stdin,
		Stdout:  os.Stdout,
		Stderr:  os.Stderr,
	})
}
