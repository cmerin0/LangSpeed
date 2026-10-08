package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"

	"golang.org/x/crypto/bcrypt"

	"langspeed/internal/logging"
)

// bcryptCost is the hashing cost for stored credentials. Requirement 13.7
// demands bcrypt with a cost factor of at least 12.
const bcryptCost = 12

// SeedAdmin implements the Seed_Script from the glossary - the one-time
// initialization that runs on first startup, after migrations:
//
//   - missing credential variables abort the seed with an error naming them,
//     without creating any user record (R2.9);
//   - an existing admin makes it a no-op with an informational log (R2.10);
//   - otherwise exactly one user with role 'admin' is created (R2.8).
func SeedAdmin(ctx context.Context, db *sql.DB, log *logging.Logger) error {
	username := strings.TrimSpace(os.Getenv("ADMIN_USERNAME"))
	password := os.Getenv("ADMIN_PASSWORD")

	var missing []string
	if username == "" {
		missing = append(missing, "ADMIN_USERNAME")
	}
	if password == "" {
		missing = append(missing, "ADMIN_PASSWORD")
	}
	if len(missing) > 0 {
		return fmt.Errorf("seed aborted: missing credential environment variable(s): %s (no user record was created)",
			strings.Join(missing, ", "))
	}

	var exists bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE role = 'admin')`).Scan(&exists); err != nil {
		return fmt.Errorf("seed aborted: could not inspect the users table: %s", dbFailure(err))
	}
	if exists {
		log.Info("admin user already exists, seed skipped", "action", "seed")
		return nil
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return fmt.Errorf("seed aborted: password could not be hashed: %w", err)
	}

	if _, err := db.ExecContext(ctx,
		`INSERT INTO users (username, hashed_password, role) VALUES ($1, $2, 'admin')`,
		username, string(hash),
	); err != nil {
		return fmt.Errorf("seed aborted: admin user could not be inserted: %s", dbFailure(err))
	}

	log.Info("admin user created", "action", "seed", "username", username, "role", "admin", "bcrypt_cost", bcryptCost)
	return nil
}
