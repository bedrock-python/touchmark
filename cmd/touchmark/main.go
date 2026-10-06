// Command touchmark keeps shared files in sync across many repositories.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/bedrock-python/touchmark/internal/cli"
)

func main() {
	// SIGTERM is what CI cancellation and `docker stop` send: stop between
	// entries like on Ctrl-C, so no temp file is left in the target.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Main(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
