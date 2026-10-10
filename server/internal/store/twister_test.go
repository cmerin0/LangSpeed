package store

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"langspeed/internal/logging"
)

// Tongue-twister selection is SQL (difficulty + active + not-shown, picked
// with ORDER BY random()), so these tests run against a real, throw-away
// PostgreSQL container. Docker is optional: without it - bare Windows dev
// machines - every test here skips; CI and the WSL/Linux environment always
// run them.

var (
	testDBOnce    sync.Once
	testDBHandle  *sql.DB
	testDBInitErr error
)

// testDB returns the shared container-backed database with every migration
// applied, or skips the test when Docker is unavailable.
func testDB(t *testing.T) *sql.DB {
	t.Helper()
	testcontainers.SkipIfProviderIsNotHealthy(t)

	testDBOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()

		pg, err := postgres.Run(ctx, "postgres:16-alpine",
			postgres.WithDatabase("langspeed_test"),
			postgres.WithUsername("test"),
			postgres.WithPassword("test"),
			// Postgres restarts itself once during init; wait for both
			// readiness log lines and for the port to be served before the
			// first dial, otherwise the published port answers with a reset.
			postgres.BasicWaitStrategies(),
		)
		if err != nil {
			testDBInitErr = err
			return
		}
		// The Ryuk reaper removes the container when this test process exits.
		dsn, err := pg.ConnectionString(ctx, "sslmode=disable")
		if err != nil {
			testDBInitErr = err
			return
		}
		db, err := Open(dsn)
		if err != nil {
			testDBInitErr = err
			return
		}
		if err := Migrate(ctx, db, logging.New(io.Discard)); err != nil {
			testDBInitErr = err
			return
		}
		testDBHandle = db
	})

	if testDBInitErr != nil {
		t.Fatalf("test database could not be prepared: %v", testDBInitErr)
	}
	return testDBHandle
}

// wipeTwisters empties the catalogue so every test controls its own pool.
func wipeTwisters(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), `DELETE FROM tongue_twisters`); err != nil {
		t.Fatalf("catalogue could not be cleared: %v", err)
	}
}

// insertTwister adds one catalogue row and returns its id.
func insertTwister(t *testing.T, db *sql.DB, text, difficulty string, active bool) int64 {
	t.Helper()
	var id int64
	err := db.QueryRowContext(context.Background(),
		`INSERT INTO tongue_twisters (text, difficulty, active) VALUES ($1, $2, $3) RETURNING id`,
		text, difficulty, active,
	).Scan(&id)
	if err != nil {
		t.Fatalf("twister could not be inserted: %v", err)
	}
	return id
}

// activeCountFor is a test-side cross-check of what the catalogue holds.
func activeCountFor(t *testing.T, db *sql.DB, difficulty string) int64 {
	t.Helper()
	var count int64
	err := db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM tongue_twisters WHERE difficulty = $1 AND active`, difficulty,
	).Scan(&count)
	if err != nil {
		t.Fatalf("catalogue could not be counted: %v", err)
	}
	return count
}

// TestSeedMigrationFillsEmptyCatalogue covers the content prerequisite of R4
// and R6: a fresh installation ships with an active catalogue per difficulty
// (every text within R14.3's 500 characters), and the seed is guarded - it
// never adds rows to a catalogue an admin has already populated (R14).
func TestSeedMigrationFillsEmptyCatalogue(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	wipeTwisters(t, db)

	script, err := migrationsFS.ReadFile("migrations/0005_seed_tongue_twisters.sql")
	if err != nil {
		t.Fatalf("seed migration could not be read: %v", err)
	}
	if _, err := db.ExecContext(ctx, string(script)); err != nil {
		t.Fatalf("seed migration failed on an empty catalogue: %v", err)
	}

	wantPerDifficulty := map[string]int64{"easy": 12, "medium": 10, "hard": 8}
	var total int64
	for difficulty, want := range wantPerDifficulty {
		got := activeCountFor(t, db, difficulty)
		if got != want {
			t.Errorf("seeded %s rows = %d, want %d", difficulty, got, want)
		}
		total += got
	}

	rows, err := db.QueryContext(ctx, `SELECT text, active FROM tongue_twisters`)
	if err != nil {
		t.Fatalf("catalogue could not be listed: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var seen int64
	for rows.Next() {
		var text string
		var active bool
		if err := rows.Scan(&text, &active); err != nil {
			t.Fatalf("catalogue row could not be read: %v", err)
		}
		seen++
		if !active {
			t.Error("seeded row is not active; selection (R4.5) could never return it")
		}
		if len(text) > 500 {
			t.Errorf("seeded text exceeds R14.3's 500 characters (%d): %.40s...", len(text), text)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("catalogue rows could not be read: %v", err)
	}
	if seen != total {
		t.Errorf("listed %d rows, counted %d", seen, total)
	}

	// Guard: re-running the seed against a populated catalogue is a no-op.
	if _, err := db.ExecContext(ctx, string(script)); err != nil {
		t.Fatalf("seed migration re-run failed: %v", err)
	}
	for difficulty, want := range wantPerDifficulty {
		if got := activeCountFor(t, db, difficulty); got != want {
			t.Errorf("after re-run %s rows = %d, want %d unchanged", difficulty, got, want)
		}
	}
}

// TestNextTwisterSelectionRules covers R6 in one pool: only active rows of
// the session's difficulty (R4.4, R4.5), never a shown ID (R6.1), and once
// every eligible row is shown the pool reports exhausted (R6.3) while other
// difficulties keep working.
func TestNextTwisterSelectionRules(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	wipeTwisters(t, db)

	alpha := insertTwister(t, db, "alpha text", "easy", true)
	beta := insertTwister(t, db, "beta text", "easy", true)
	insertTwister(t, db, "inactive text", "easy", false) // R4.5: never selected
	insertTwister(t, db, "hard text", "hard", true)

	texts := map[int64]string{alpha: "alpha text", beta: "beta text"}
	content := NewContent(db)

	// First pick: an eligible easy row - never the inactive one.
	first, err := content.NextTwister(ctx, "easy", nil)
	if err != nil {
		t.Fatalf("first NextTwister() failed: %v", err)
	}
	if first.Difficulty != "easy" {
		t.Errorf("Difficulty = %q, want easy (R4.4)", first.Difficulty)
	}
	if first.ID != alpha && first.ID != beta {
		t.Errorf("first ID = %d, want %d or %d", first.ID, alpha, beta)
	}
	if first.Text != texts[first.ID] {
		t.Errorf("Text = %q, want %q for id %d", first.Text, texts[first.ID], first.ID)
	}

	// Second pick excludes the shown ID (R6.1) and yields the other row.
	second, err := content.NextTwister(ctx, "easy", []int64{first.ID})
	if err != nil {
		t.Fatalf("second NextTwister() failed: %v", err)
	}
	wantOther := alpha
	if first.ID == alpha {
		wantOther = beta
	}
	if second.ID != wantOther {
		t.Errorf("second ID = %d, want the un-shown row %d (R6.1)", second.ID, wantOther)
	}

	// Everything eligible is shown: R6.3, not an error of R4.6.
	_, err = content.NextTwister(ctx, "easy", []int64{alpha, beta})
	if !errors.Is(err, ErrAllShown) {
		t.Errorf("exhausted pool = %v, want ErrAllShown", err)
	}

	// Shown IDs from other difficulties do not hide this pool, and the hard
	// row comes back verbatim.
	hard, err := content.NextTwister(ctx, "hard", []int64{alpha, beta})
	if err != nil {
		t.Fatalf("hard NextTwister() failed: %v", err)
	}
	if hard.Text != "hard text" || hard.Difficulty != "hard" {
		t.Errorf("hard pick = %+v, want the hard row (R4.4)", hard)
	}
}

// TestNextTwisterNoActiveContent covers R4.6 and the distinction from R6.3:
// an empty (or fully inactive) catalogue for the difficulty is ErrNoContent
// regardless of the shown list, while content that merely has been shown all
// is ErrAllShown.
func TestNextTwisterNoActiveContent(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	wipeTwisters(t, db)
	content := NewContent(db)

	// Only an inactive row for medium: nothing can ever be served (R4.5).
	insertTwister(t, db, "inactive text", "medium", false)
	if _, err := content.NextTwister(ctx, "medium", nil); !errors.Is(err, ErrNoContent) {
		t.Errorf("inactive-only catalogue = %v, want ErrNoContent (R4.6)", err)
	}

	// No rows at all for hard - even with a non-empty shown list.
	if _, err := content.NextTwister(ctx, "hard", []int64{123, 456}); !errors.Is(err, ErrNoContent) {
		t.Errorf("empty catalogue = %v, want ErrNoContent (R4.6)", err)
	}

	// Active content exists but this session has seen it: R6.3, not R4.6.
	solo := insertTwister(t, db, "solo text", "easy", true)
	if _, err := content.NextTwister(ctx, "easy", []int64{solo}); !errors.Is(err, ErrAllShown) {
		t.Errorf("fully shown catalogue = %v, want ErrAllShown (R6.3)", err)
	}
}
