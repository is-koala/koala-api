// Package app wires the service by hand (no DI container, ADR-0002) and
// implements the koala subcommands. Each domain module gets its own
// wire<Module> function in this package as it lands.
package app

import (
	"context"
	"fmt"
	"io"
)

// Exit codes.
const (
	ExitOK    = 0
	ExitError = 1
	ExitUsage = 2
)

const usage = `usage: koala <command>

commands:
  serve        run the HTTP server
  bootstrap    create the database group roles (idempotent; before migrate)
  migrate      apply database migrations: up, down, status (goose)
  maint        maintenance tasks for scheduled ops jobs: reindex, audit-purge
  staff        staff accounts as koala_maint: create, disable
  healthcheck  probe the local server's /livez (Docker HEALTHCHECK)
  keygen       print a new signing key entry or login pepper
  version      print the build version
`

// IO is the process environment a command runs in.
type IO struct {
	Environ []string // os.Environ format
	Stdout  io.Writer
	Stderr  io.Writer
	Version string
}

// Run executes the subcommand in args and returns the process exit code.
// ctx is canceled on SIGINT/SIGTERM.
func Run(ctx context.Context, args []string, sio IO) int {
	if len(args) == 0 {
		_, _ = io.WriteString(sio.Stderr, usage)
		return ExitUsage
	}
	switch cmd, rest := args[0], args[1:]; cmd {
	case "serve":
		return runServe(ctx, rest, sio)
	case "healthcheck":
		return runHealthcheck(ctx, rest, sio)
	case "bootstrap":
		return runBootstrap(ctx, rest, sio)
	case "migrate":
		return runMigrate(ctx, rest, sio)
	case "maint":
		return runMaint(ctx, rest, sio)
	case "staff":
		return runStaff(ctx, rest, sio)
	case "keygen":
		return runKeygen(rest, sio)
	case "version":
		_, _ = fmt.Fprintln(sio.Stdout, sio.Version)
		return ExitOK
	case "-h", "--help", "help":
		_, _ = io.WriteString(sio.Stdout, usage)
		return ExitOK
	default:
		_, _ = fmt.Fprintf(sio.Stderr, "koala: unknown command %q\n\n%s", cmd, usage)
		return ExitUsage
	}
}

func rejectArgs(name string, args []string, sio IO) bool {
	if len(args) == 0 {
		return false
	}
	_, _ = fmt.Fprintf(sio.Stderr, "koala %s: takes no arguments; configure with KOALA_* variables\n", name)
	return true
}
