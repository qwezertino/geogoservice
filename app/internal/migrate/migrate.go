// Package migrate runs SQL migrations against the PostgreSQL database on
// application startup. It uses golang-migrate with an embedded filesystem so
// the migration files are baked into the binary and no external tools are needed.
package migrate

import (
	"errors"
	"fmt"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

// Run applies all pending UP migrations from the embedded SQL files.
// It is idempotent: already-applied migrations are skipped.
// Returns an error only when a migration actually fails; reaching
// migrate.ErrNoChange is treated as success.
func Run(migrations MigrationsFS, dsn string) error {
	src, err := iofs.New(migrations.FS, migrations.Dir)
	if err != nil {
		return fmt.Errorf("open migration source: %w", err)
	}

	// golang-migrate expects a pgx5:// URL, not the standard postgres:// DSN.
	m, err := migrate.NewWithSourceInstance("iofs", src, dsn)
	if err != nil {
		return fmt.Errorf("create migrator: %w", err)
	}
	defer m.Close()

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("run migrations: %w", err)
	}
	return nil
}
