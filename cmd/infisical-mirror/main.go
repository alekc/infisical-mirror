// Command infisical-mirror keeps two Infisical instances in step.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/alekc/infisical-mirror/internal/cli"
)

func main() {
	// A cancelled context stops the run between calls rather than mid-write,
	// which matters more once this command can write.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	// Not deferred: os.Exit does not run deferred functions, so a defer here
	// would read as cleanup that happens and never happen.
	code := cli.Run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
