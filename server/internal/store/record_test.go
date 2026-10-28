package store

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"
)

// Finished-game persistence is SQL (game_records + attempt_details in one
// transaction), so it runs against the same throw-away PostgreSQL container
// as the selection tests above - with the same Docker-optional skip.

// wipeRecords empties the result tables so every test counts its own rows
// (attempt_details goes with game_records via ON DELETE CASCADE).
func wipeRecords(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), `DELETE FROM game_records`); err != nil {
		t.Fatalf("game records could not be cleared: %v", err)
	}
}

// countRows is a test-side cross-check of what a table holds.
func countRows(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var count int
	if err := db.QueryRowContext(context.Background(), `SELECT count(*) FROM `+table).Scan(&count); err != nil {
		t.Fatalf("%s could not be counted: %v", table, err)
	}
	return count
}

// TestSaveGameResultPersistsRecordAndDetails covers R10 and R11: the
// Game_Record lands with the session's final state, and every graded attempt
// becomes exactly one attempt_details row - attempt_count 1 (one attempt per
// twister, decision 13), the measured time and the outcome, all linked to
// the record and all committed together.
func TestSaveGameResultPersistsRecordAndDetails(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	wipeTwisters(t, db)
	wipeRecords(t, db)

	twisterA := insertTwister(t, db, "alpha text", "medium", true)
	twisterB := insertTwister(t, db, "beta text", "medium", true)

	started := time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)
	ended := started.Add(90 * time.Second)
	records := NewRecords(db)
	err := records.SaveGameResult(ctx, GameResult{
		Nickname:   "carlo",
		FinalScore: 50,
		Difficulty: "medium",
		StartedAt:  started,
		EndedAt:    ended,
		Attempts: []AttemptResult{
			{TwisterID: twisterA, TimeTakenMS: 12345, Success: true},
			{TwisterID: twisterB, TimeTakenMS: 6789, Success: false},
		},
	})
	if err != nil {
		t.Fatalf("SaveGameResult() failed: %v", err)
	}

	var recordID int64
	var nickname, difficulty string
	var score int
	var gotStarted, gotEnded time.Time
	err = db.QueryRowContext(ctx, `
		SELECT id, nickname, final_score, difficulty, started_at, ended_at
		FROM game_records`).
		Scan(&recordID, &nickname, &score, &difficulty, &gotStarted, &gotEnded)
	if err != nil {
		t.Fatalf("Game_Record could not be read back: %v", err)
	}
	if nickname != "carlo" || score != 50 || difficulty != "medium" {
		t.Errorf("record = %q/%d/%q, want carlo/50/medium", nickname, score, difficulty)
	}
	if !gotStarted.Equal(started) || !gotEnded.Equal(ended) {
		t.Errorf("timestamps = %v..%v, want %v..%v", gotStarted, gotEnded, started, ended)
	}

	rows, err := db.QueryContext(ctx, `
		SELECT tongue_twister_id, attempt_count, time_taken_ms, success
		FROM attempt_details
		WHERE game_record_id = $1
		ORDER BY id`, recordID)
	if err != nil {
		t.Fatalf("attempt details could not be listed: %v", err)
	}
	defer func() { _ = rows.Close() }()

	want := []AttemptResult{
		{TwisterID: twisterA, TimeTakenMS: 12345, Success: true},
		{TwisterID: twisterB, TimeTakenMS: 6789, Success: false},
	}
	var got []AttemptResult
	var attemptCounts []int
	for rows.Next() {
		var detail AttemptResult
		var attemptCount int
		if err := rows.Scan(&detail.TwisterID, &attemptCount, &detail.TimeTakenMS, &detail.Success); err != nil {
			t.Fatalf("attempt detail could not be read: %v", err)
		}
		got = append(got, detail)
		attemptCounts = append(attemptCounts, attemptCount)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("attempt details could not be read: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("attempt details = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("attempt_details[%d] = %+v, want %+v", i, got[i], want[i])
		}
		if attemptCounts[i] != 1 {
			t.Errorf("attempt_details[%d].attempt_count = %d, want 1 (one attempt per twister, decision 13)",
				i, attemptCounts[i])
		}
	}
}

// TestSaveGameResultSkipsTwistersDeletedMidGame: an attempt whose tongue
// twister was removed from the catalogue after the game would violate the
// foreign key - that one breakdown line is dropped, the rest of the result
// still lands. Losing a whole Game_Record over one deleted row would be
// worse than losing the line.
func TestSaveGameResultSkipsTwistersDeletedMidGame(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	wipeTwisters(t, db)
	wipeRecords(t, db)

	deleted := insertTwister(t, db, "soon gone", "easy", true)
	survivor := insertTwister(t, db, "still here", "easy", true)
	if _, err := db.ExecContext(ctx, `DELETE FROM tongue_twisters WHERE id = $1`, deleted); err != nil {
		t.Fatalf("twister could not be deleted: %v", err)
	}

	started := time.Date(2026, 10, 27, 11, 0, 0, 0, time.UTC)
	err := NewRecords(db).SaveGameResult(ctx, GameResult{
		Nickname:   "carlo",
		FinalScore: 20,
		Difficulty: "easy",
		StartedAt:  started,
		EndedAt:    started.Add(time.Second),
		Attempts: []AttemptResult{
			{TwisterID: deleted, TimeTakenMS: 1000, Success: true},
			{TwisterID: survivor, TimeTakenMS: 2000, Success: true},
		},
	})
	if err != nil {
		t.Fatalf("SaveGameResult() with a deleted twister = %v, want no error", err)
	}

	if got := countRows(t, db, "game_records"); got != 1 {
		t.Errorf("game_records rows = %d, want 1 (the record must still land)", got)
	}
	var keptTwisterID int64
	err = db.QueryRowContext(ctx, `
		SELECT tongue_twister_id FROM attempt_details`).Scan(&keptTwisterID)
	if err != nil {
		t.Fatalf("attempt detail could not be read: %v", err)
	}
	if keptTwisterID != survivor {
		t.Errorf("kept attempt for twister %d, want only the surviving %d", keptTwisterID, survivor)
	}
}

// TestSaveGameResultRollsBackAtomically: a Game_Record that cannot store its
// attempts must not linger half-recorded - an invalid row (here: a
// difficulty outside the CHECK constraint) aborts the transaction and
// leaves nothing behind, with an error that stays free of SQL text (R21.4).
func TestSaveGameResultRollsBackAtomically(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	wipeTwisters(t, db)
	wipeRecords(t, db)

	twister := insertTwister(t, db, "alpha text", "easy", true)
	started := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	err := NewRecords(db).SaveGameResult(ctx, GameResult{
		Nickname:   "carlo",
		FinalScore: 10,
		Difficulty: "insane", // violates the difficulty CHECK (R2.5)
		StartedAt:  started,
		EndedAt:    started.Add(time.Second),
		Attempts:   []AttemptResult{{TwisterID: twister, TimeTakenMS: 500, Success: true}},
	})
	if err == nil {
		t.Fatal("SaveGameResult() with an invalid difficulty should fail")
	}
	for _, leak := range []string{"INSERT", "SELECT", "game_records ("} {
		if strings.Contains(strings.ToUpper(err.Error()), leak) {
			t.Errorf("error leaked SQL text (R21.4): %q", err)
		}
	}
	if got := countRows(t, db, "game_records"); got != 0 {
		t.Errorf("game_records rows = %d after the rollback, want 0 (no half-recorded game)", got)
	}
}

// TestSaveGameResultWithoutAttempts: a game with no graded attempts (nothing
// was ever typed) still records the Game_Record - the batch is simply empty.
func TestSaveGameResultWithoutAttempts(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	wipeTwisters(t, db)
	wipeRecords(t, db)

	started := time.Date(2026, 10, 27, 13, 0, 0, 0, time.UTC)
	err := NewRecords(db).SaveGameResult(ctx, GameResult{
		Nickname:   "carlo",
		FinalScore: 0,
		Difficulty: "hard",
		StartedAt:  started,
		EndedAt:    started.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("SaveGameResult() without attempts failed: %v", err)
	}
	if got := countRows(t, db, "game_records"); got != 1 {
		t.Errorf("game_records rows = %d, want 1", got)
	}
	if got := countRows(t, db, "attempt_details"); got != 0 {
		t.Errorf("attempt_details rows = %d, want 0", got)
	}
}
