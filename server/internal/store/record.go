package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/lib/pq"
)

// AttemptResult is one graded attempt as an attempt_details row (R11) needs
// it: which tongue twister, how long the player had it, and whether it was
// correct. attempt_count is deliberately not carried here - it is always 1,
// because a wrong attempt consumes the twister so no twister can be attempted
// twice in one game (decision 13), and the INSERT writes that literal with a
// comment pointing back at this reasoning.
type AttemptResult struct {
	TwisterID   int64
	TimeTakenMS int
	Success     bool
}

// GameResult is a finished game ready to be persisted: R10's Game_Record
// columns plus the R11 attempt breakdown collected from the Game_Session
// while the game ran. StartedAt comes from the session's creation timestamp,
// EndedAt from the session's end stamp (R5.1, R10).
type GameResult struct {
	Nickname   string
	FinalScore int
	Difficulty string
	StartedAt  time.Time
	EndedAt    time.Time
	Attempts   []AttemptResult
}

// Records persists finished games (Requirements 10 and 11) through the
// shared connection pool. *store.Records implements httpserver.gameRecorder;
// HTTP tests substitute a fake, so the HTTP surface stays testable without a
// database.
type Records struct {
	db *sql.DB
}

// NewRecords binds the writer to the shared connection pool.
func NewRecords(db *sql.DB) *Records { return &Records{db: db} }

// SaveGameResult writes the Game_Record (R10) and one Attempt_Detail per
// graded tongue twister (R11) in a single transaction: a game that cannot
// record its attempts must not leave a half-recorded Game_Record behind, and
// a record without its breakdown would be half a result.
//
// A tongue twister deleted from the catalogue while the game was running
// would violate attempt_details' foreign key; such rows are skipped so the
// Game_Record itself still lands - the catalogue row their timing described
// no longer exists, and losing the whole game over it would be worse than
// losing one breakdown line. Everything else, including the batch, commits
// together or not at all.
//
// All database errors are wrapped with dbFailure: callers can log the result
// without ever leaking SQL text, bound parameters or connection details
// (R21.4).
func (r *Records) SaveGameResult(ctx context.Context, result GameResult) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("game record could not be saved: %s", dbFailure(err))
	}
	defer func() { _ = tx.Rollback() }() // no-op once committed

	var recordID int64
	err = tx.QueryRowContext(ctx, `
		INSERT INTO game_records (nickname, final_score, difficulty, started_at, ended_at)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id`,
		result.Nickname, result.FinalScore, result.Difficulty,
		result.StartedAt, result.EndedAt,
	).Scan(&recordID)
	if err != nil {
		return fmt.Errorf("game record could not be saved: %s", dbFailure(err))
	}

	if len(result.Attempts) > 0 {
		existing, err := existingTwisterIDs(ctx, tx, result.Attempts)
		if err != nil {
			return fmt.Errorf("attempt details could not be saved: %s", dbFailure(err))
		}
		for _, attempt := range result.Attempts {
			if !existing[attempt.TwisterID] {
				// Deleted mid-game: the FK could not be satisfied, so this
				// one breakdown line is dropped (see the doc comment).
				continue
			}
			// attempt_count is the literal 1: one attempt per twister, always
			// (decision 13 - a wrong attempt consumes the twister).
			_, err := tx.ExecContext(ctx, `
				INSERT INTO attempt_details (game_record_id, tongue_twister_id, attempt_count, time_taken_ms, success)
				VALUES ($1, $2, 1, $3, $4)`,
				recordID, attempt.TwisterID, attempt.TimeTakenMS, attempt.Success,
			)
			if err != nil {
				return fmt.Errorf("attempt details could not be saved: %s", dbFailure(err))
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("game record could not be saved: %s", dbFailure(err))
	}
	return nil
}

// existingTwisterIDs reports which of the attempts' tongue twisters are still
// in the catalogue, in one query inside the caller's transaction - the
// filter must see the same snapshot the inserts commit against.
func existingTwisterIDs(ctx context.Context, tx *sql.Tx, attempts []AttemptResult) (map[int64]bool, error) {
	ids := make([]int64, 0, len(attempts))
	for _, attempt := range attempts {
		ids = append(ids, attempt.TwisterID)
	}

	rows, err := tx.QueryContext(ctx,
		`SELECT id FROM tongue_twisters WHERE id = ANY($1::bigint[])`, pq.Array(ids))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	existing := make(map[int64]bool, len(ids))
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		existing[id] = true
	}
	return existing, rows.Err()
}
