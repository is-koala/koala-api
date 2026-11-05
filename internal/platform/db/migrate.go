package db

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

// Migrator applies goose migrations (ADR-0012) as RoleOwner, so every
// object it creates is owned by koala_owner whatever login connects.
type Migrator struct {
	provider *goose.Provider
}

// NewMigrator returns a migrator for the SQL migrations at the root of
// fsys. Concurrent migrators on one database serialize on a Postgres
// advisory lock.
func NewMigrator(url string, fsys fs.FS, logger *slog.Logger) (*Migrator, error) {
	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		return nil, errors.New("parse database URL: invalid connection string")
	}
	applyRuntimeDefaults(cfg, RoleOwner, settingsFor(RoleOwner))
	setRole := setRoleHook(RoleOwner)
	sqlDB := stdlib.OpenDB(*cfg, stdlib.OptionAfterConnect(func(ctx context.Context, conn *pgx.Conn) error {
		if err := CheckNotSuperuser(ctx, conn); err != nil {
			return err
		}
		return setRole(ctx, conn)
	}))

	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("create migration lock: %w", err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, fsys,
		goose.WithSessionLocker(locker),
		goose.WithDisableGlobalRegistry(true),
		goose.WithSlog(logger),
	)
	if err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("load migrations: %w", err)
	}
	return &Migrator{provider: provider}, nil
}

// Up applies every pending migration, each file in its own transaction
// unless it is marked `-- +goose NO TRANSACTION`.
func (m *Migrator) Up(ctx context.Context) ([]*goose.MigrationResult, error) {
	res, err := m.provider.Up(ctx)
	if err != nil {
		return res, fmt.Errorf("migrate up: %w", err)
	}
	return res, nil
}

// Down rolls back the most recently applied migration. It is a development
// tool: River's down migrations drop job tables with their rows, a NO
// TRANSACTION file is not atomic, and `koala migrate` refuses down in
// production (ADR-0017).
func (m *Migrator) Down(ctx context.Context) (*goose.MigrationResult, error) {
	res, err := m.provider.Down(ctx)
	if err != nil {
		return res, fmt.Errorf("migrate down: %w", err)
	}
	return res, nil
}

// Status lists every migration with its state.
func (m *Migrator) Status(ctx context.Context) ([]*goose.MigrationStatus, error) {
	st, err := m.provider.Status(ctx)
	if err != nil {
		return nil, fmt.Errorf("migration status: %w", err)
	}
	return st, nil
}

// Close closes the migrator's connections.
func (m *Migrator) Close() error {
	if err := m.provider.Close(); err != nil {
		return fmt.Errorf("close migrator: %w", err)
	}
	return nil
}
