package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"strings"

	"langspeed/internal/logging"
)

// migrationsFS holds the ordered SQL migrations, compiled into the binary so
// the image never needs to ship loose files.
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

// schemaMigrationsTable records which migrations have been applied.
const schemaMigrationsTable = `
CREATE TABLE IF NOT EXISTS schema_migrations (
	name       text        PRIMARY KEY,
	applied_at timestamptz NOT NULL DEFAULT now()
)`

// Migrate applies every pending migration in filename order, each inside its
// own transaction (R2.1). On the first failure it stops - later migrations are
// not applied - and reports which migration failed (R2.2).
func Migrate(ctx context.Context, db *sql.DB, log *logging.Logger) error {
	if _, err := db.ExecContext(ctx, schemaMigrationsTable); err != nil {
		return fmt.Errorf("migration bookkeeping table could not be prepared: %s", dbFailure(err))
	}

	applied, err := appliedMigrations(ctx, db)
	if err != nil {
		return err
	}

	// fs.ReadDir returns entries sorted by filename, which matches the
	// numeric prefixes (0001_, 0002_, ...) used by the migration files.
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("embedded migrations could not be read: %w", err)
	}

	pending := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		name := entry.Name()
		if applied[name] {
			continue
		}
		pending++

		script, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("migration %s could not be read: %w", name, err)
		}

		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("migration %s could not start: %s", name, dbFailure(err))
		}
		if _, err := tx.ExecContext(ctx, string(script)); err != nil {
			_ = tx.Rollback()
			// Names the failing migration (R2.2); the error carries only the
			// server message and SQLSTATE, never SQL text (R21.4).
			return fmt.Errorf("migration %s failed, later migrations were not applied: %s", name, dbFailure(err))
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (name) VALUES ($1)`, name); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migration %s could not be recorded as applied: %s", name, dbFailure(err))
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("migration %s could not be committed: %s", name, dbFailure(err))
		}
		log.Info("migration applied", "migration", name)
	}

	log.Info("database schema is up to date", "migrations_pending", pending, "migrations_total", len(entries))
	return nil
}

// appliedMigrations returns the set of migration names already recorded.
func appliedMigrations(ctx context.Context, db *sql.DB) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT name FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("applied migrations could not be listed: %s", dbFailure(err))
	}
	defer func() { _ = rows.Close() }()

	applied := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("applied migrations could not be read: %s", dbFailure(err))
		}
		applied[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("applied migrations could not be read: %s", dbFailure(err))
	}
	return applied, nil
}
