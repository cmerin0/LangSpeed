package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/lib/pq"
)

// Selection outcomes. The HTTP layer maps them to distinct responses:
// ErrNoContent is the "no content available" error of R4.6, ErrAllShown is the
// pool-exhausted condition of R6.3.
var (
	// ErrNoContent: no active tongue-twister exists for this Difficulty at
	// all, so none can ever be served (R4.6).
	ErrNoContent = errors.New("no content is available for the selected difficulty")
	// ErrAllShown: content exists, but this Game_Session has already been
	// shown every eligible tongue-twister (R6.3).
	ErrAllShown = errors.New("all tongue-twisters for the current difficulty have been completed")
)

// Twister is one selectable tongue-twister: exactly the columns R4 and R6
// reason about (id, text, difficulty).
type Twister struct {
	ID         int64  `json:"tongue_twister_id"`
	Text       string `json:"text"`
	Difficulty string `json:"difficulty"`
}

// Content answers "which tongue-twister comes next?" queries against the
// tongue_twisters catalogue (Requirements 4 and 6).
type Content struct {
	db *sql.DB
}

// NewContent binds the selector to the shared connection pool.
func NewContent(db *sql.DB) *Content { return &Content{db: db} }

// NextTwister picks the next tongue-twister for a session:
//
//   - only rows matching difficulty and active (R4.4, R4.5);
//   - never a tongue-twister the session has already been shown (R6.1);
//   - uniformly random among the remaining rows (R6.2, ORDER BY random()).
//
// It returns ErrNoContent when the difficulty has no active content (R4.6)
// and ErrAllShown when content exists but every eligible row is already in
// shown (R6.3). Any database failure is wrapped with dbFailure so callers can
// log it without leaking SQL, parameters or connection details (R21.4).
func (c *Content) NextTwister(ctx context.Context, difficulty string, shown []int64) (*Twister, error) {
	if shown == nil {
		shown = []int64{}
	}

	twister, err := c.selectNext(ctx, difficulty, shown)
	if errors.Is(err, sql.ErrNoRows) {
		// Distinguish R4.6 (no content at all) from R6.3 (content exists but
		// this session has seen it all): only an empty catalogue is R4.6.
		count, countErr := c.activeCount(ctx, difficulty)
		if countErr != nil {
			return nil, fmt.Errorf("available tongue-twisters could not be counted: %s", dbFailure(countErr))
		}
		if count == 0 {
			return nil, ErrNoContent
		}
		return nil, ErrAllShown
	}
	if err != nil {
		return nil, fmt.Errorf("next tongue-twister could not be selected: %s", dbFailure(err))
	}
	return twister, nil
}

// selectNext runs the R6.2 selection in a single round-trip: eligible rows
// (difficulty + active + not already shown), picked uniformly at random.
func (c *Content) selectNext(ctx context.Context, difficulty string, shown []int64) (*Twister, error) {
	const query = `
		SELECT id, text, difficulty
		FROM tongue_twisters
		WHERE difficulty = $1
		  AND active
		  AND NOT (id = ANY($2::bigint[]))
		ORDER BY random()
		LIMIT 1`

	row := c.db.QueryRowContext(ctx, query, difficulty, pq.Array(shown))
	var twister Twister
	if err := row.Scan(&twister.ID, &twister.Text, &twister.Difficulty); err != nil {
		return nil, err
	}
	return &twister, nil
}

// activeCount reports how many active tongue-twisters exist for difficulty -
// the R4.6 vs. R6.3 discriminator.
func (c *Content) activeCount(ctx context.Context, difficulty string) (int64, error) {
	const query = `SELECT count(*) FROM tongue_twisters WHERE difficulty = $1 AND active`

	var count int64
	if err := c.db.QueryRowContext(ctx, query, difficulty).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}
