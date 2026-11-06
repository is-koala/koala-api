package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/is-koala/koala-api/internal/audit"
	"github.com/is-koala/koala-api/internal/audit/auditdb"
	"github.com/is-koala/koala-api/internal/authn"
	"github.com/is-koala/koala-api/internal/authz"
	"github.com/is-koala/koala-api/internal/authz/authzsql"
	"github.com/is-koala/koala-api/internal/platform/clock"
	"github.com/is-koala/koala-api/internal/platform/config"
	"github.com/is-koala/koala-api/internal/platform/db"
	"github.com/is-koala/koala-api/internal/platform/log"
	"github.com/is-koala/koala-api/internal/staff"
)

const staffUsage = `usage: koala staff <create|disable> [flags]

  create   --email <address> --role <ROLE> [--role <ROLE> …]
           open a staff account with the given roles and print its
           one-time password once; the member changes it at the first
           sign-in. The first super admin: --role OWNER.
  disable  --email <address>
           take the account out of service; its token version moves, so
           every token of the account dies (ADR-0025 §6)

Connects with KOALA_MAINT_DATABASE_URL, a login that is a member of
koala_maint: the application cannot create staff (ADR-0025, ADR-0026).
Every change is written to the audit trail.
`

// roleFlags collects repeated --role flags.
type roleFlags []string

func (r *roleFlags) String() string { return strings.Join(*r, ",") }

func (r *roleFlags) Set(v string) error {
	*r = append(*r, strings.ToUpper(strings.TrimSpace(v)))
	return nil
}

// runStaff manages staff accounts as koala_maint (ADR-0026). Staff
// management over the API lands in Faz 8.
func runStaff(ctx context.Context, args []string, sio IO) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(sio.Stderr, staffUsage)
		return ExitUsage
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("koala staff "+sub, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	email := fs.String("email", "", "login address")
	var roles roleFlags
	switch sub {
	case "create":
		fs.Var(&roles, "role", "role code, repeatable")
	case "disable":
	case "-h", "--help", "help":
		_, _ = fmt.Fprint(sio.Stdout, staffUsage)
		return ExitOK
	default:
		_, _ = fmt.Fprintf(sio.Stderr, "koala staff: unknown command %q\n\n%s", sub, staffUsage)
		return ExitUsage
	}
	if err := fs.Parse(rest); err != nil || fs.NArg() > 0 || *email == "" || (sub == "create" && len(roles) == 0) {
		_, _ = fmt.Fprint(sio.Stderr, staffUsage)
		return ExitUsage
	}

	cfg, err := config.Load(sio.Environ)
	if err != nil {
		log.New(sio.Stderr, slog.LevelInfo).ErrorContext(ctx, "staff failed", slog.Any("error", err))
		return ExitError
	}
	logger := log.New(sio.Stderr, cfg.LogLevel)
	if cfg.MaintDatabaseURL == "" {
		logger.ErrorContext(ctx, "staff failed", slog.String("error", config.VarMaintDatabaseURL+": required by staff"))
		return ExitError
	}
	maint, err := db.Open(ctx, db.Config{URL: cfg.MaintDatabaseURL.Reveal(), Role: db.RoleMaint, Logger: logger})
	if err != nil {
		logger.ErrorContext(ctx, "staff failed", slog.Any("error", err))
		return ExitError
	}
	defer maint.Close()

	admin, err := newStaffAdmin(maint, clock.Real{})
	if err != nil {
		logger.ErrorContext(ctx, "staff failed", slog.Any("error", err))
		return ExitError
	}
	switch sub {
	case "create":
		c, err := admin.Create(ctx, *email, roles)
		if err != nil {
			logger.ErrorContext(ctx, "staff create failed", slog.Any("error", err))
			return ExitError
		}
		// The one-time password goes to the operator's terminal once; it
		// is not logged.
		_, _ = fmt.Fprintf(sio.Stdout, "staff member %s created with roles %s\none-time password: %s\n",
			c.ID, strings.Join(c.Roles, ","), c.Password)
	case "disable":
		d, err := admin.Disable(ctx, *email)
		if err != nil {
			logger.ErrorContext(ctx, "staff disable failed", slog.Any("error", err))
			return ExitError
		}
		_, _ = fmt.Fprintf(sio.Stdout, "staff member %s disabled; token version %d\n", d.ID, d.TokenVersion)
	}
	return ExitOK
}

// newStaffAdmin wires staff management on a koala_maint pool.
func newStaffAdmin(maint *db.DB, clk clock.Clock) (*staff.Admin, error) {
	model, err := authz.DefaultModel()
	if err != nil {
		return nil, fmt.Errorf("wire staff: %w", err)
	}
	passwords, err := authn.NewPasswords(authn.DefaultParams(), 1)
	if err != nil {
		return nil, fmt.Errorf("wire staff: %w", err)
	}
	return staff.NewAdmin(maint, passwords, audit.NewRecorder(), authzsql.NewChangeLog(model, clk), clk), nil
}

// staffModule is the staff surface of serve: the permission gate and the
// staff handlers on the application pool (ADR-0026).
type staffModule struct {
	http     *staff.HTTP
	checker  *authzsql.StaffChecker
	recorder *audit.Recorder
	// writes is where the gate writes audit rows: the application pool,
	// each row its own short transaction.
	writes auditdb.DBTX
}

// wireStaff builds the staff checker and handlers; the gate needs the
// contract and is built by mountAPI.
func wireStaff(database *db.DB, users *authn.StaffService) (staffModule, error) {
	model, err := authz.DefaultModel()
	if err != nil {
		return staffModule{}, fmt.Errorf("wire staff: %w", err)
	}
	if users == nil {
		return staffModule{}, errors.New("wire staff: no staff authentication")
	}
	checker := authzsql.NewStaffChecker(model, database.Pool())
	return staffModule{
		http: staff.NewHTTP(checker, users), checker: checker, recorder: audit.NewRecorder(),
		writes: database.Pool(),
	}, nil
}
