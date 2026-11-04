// Command koala is the Koala API server. Subcommands: serve, migrate,
// healthcheck, version. Wiring lives in internal/app.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	_ "time/tzdata" // distroless/static has no zoneinfo (plan §3.8)

	"github.com/is-koala/koala-api/internal/app"
)

// version is set at build time: -ldflags "-X main.version=<git tag>".
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	go func() {
		// After the first signal, restore default handling so a second
		// Ctrl-C kills the process during the drain.
		<-ctx.Done()
		stop()
	}()

	code := app.Run(ctx, os.Args[1:], app.IO{
		Environ: os.Environ(),
		Stdout:  os.Stdout,
		Stderr:  os.Stderr,
		Version: version,
	})
	stop()
	os.Exit(code)
}
