package cache

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// newTestStore returns a Store backed by an in-process Redis (miniredis) and
// the handle needed to inspect and fast-forward its clock.
func newTestStore(t *testing.T) (*Store, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	return New(mr.Addr()), mr
}

// deadStore returns a Store pointing at a closed port so cache failures can
// be simulated quickly (R3.6).
func deadStore() *Store {
	return NewWithClient(redis.NewClient(&redis.Options{
		Addr:         "127.0.0.1:1",
		DialTimeout:  100 * time.Millisecond,
		ReadTimeout:  100 * time.Millisecond,
		WriteTimeout: 100 * time.Millisecond,
	}))
}

// TestCreatePersistsAllSessionFields covers R5.1: every mandated field is
// stored, with a 2h expiry (R5.2).
func TestCreatePersistsAllSessionFields(t *testing.T) {
	store, mr := newTestStore(t)
	ctx := context.Background()

	original := NewGameSession("carlo", "medium", time.Now().UTC())
	if err := store.Create(ctx, original); err != nil {
		t.Fatalf("Create() failed: %v", err)
	}

	redisKey := "game_session:" + original.SessionID
	if !mr.Exists(redisKey) {
		t.Fatalf("entry %s not found in redis", redisKey)
	}
	if ttl := mr.TTL(redisKey); ttl != 2*time.Hour {
		t.Errorf("TTL = %s, want exactly 2h", ttl)
	}

	stored, err := store.Get(ctx, original.SessionID)
	if err != nil {
		t.Fatalf("Get() failed: %v", err)
	}
	assertSessionsEqual(t, original, stored)
}

// TestSessionExpiresAfterTwoHours covers R5.2: the entry is removed after
// exactly 2 hours without activity.
func TestSessionExpiresAfterTwoHours(t *testing.T) {
	store, mr := newTestStore(t)
	ctx := context.Background()

	session := NewGameSession("carlo", "easy", time.Now().UTC())
	if err := store.Create(ctx, session); err != nil {
		t.Fatalf("Create() failed: %v", err)
	}

	mr.FastForward(2*time.Hour - time.Second)
	redisKey := "game_session:" + session.SessionID
	if !mr.Exists(redisKey) {
		t.Fatal("session died before the 2h window elapsed")
	}

	// Note: no store.Get here - a read would refresh the window (R5.2), which
	// is covered separately by TestGetRefreshesInactivityWindow.
	mr.FastForward(2 * time.Second)
	if mr.Exists(redisKey) {
		t.Error("session still exists after the 2h window elapsed")
	}
	if _, err := store.Get(ctx, session.SessionID); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("Get() after 2h = %v, want ErrSessionNotFound", err)
	}
}

// TestGetRefreshesInactivityWindow covers the "no read ... on that Cache
// entry" half of R5.2: a read resets the 2h window.
func TestGetRefreshesInactivityWindow(t *testing.T) {
	store, mr := newTestStore(t)
	ctx := context.Background()

	session := NewGameSession("carlo", "easy", time.Now().UTC())
	if err := store.Create(ctx, session); err != nil {
		t.Fatalf("Create() failed: %v", err)
	}

	// 110 minutes in: a read refreshes the window to now + 2h.
	mr.FastForward(110 * time.Minute)
	if _, err := store.Get(ctx, session.SessionID); err != nil {
		t.Fatalf("Get() at 110m failed: %v", err)
	}

	// 110 minutes after the refresh (= 220m absolute): dead without refresh.
	mr.FastForward(110 * time.Minute)
	if _, err := store.Get(ctx, session.SessionID); err != nil {
		t.Fatalf("session died although it was read within the window: %v", err)
	}

	// Past the refreshed window: expired.
	mr.FastForward(121 * time.Minute)
	if _, err := store.Get(ctx, session.SessionID); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("Get() after the refreshed window = %v, want ErrSessionNotFound", err)
	}
}

// TestSaveResetsInactivityWindow covers the "... or write" half of R5.2.
func TestSaveResetsInactivityWindow(t *testing.T) {
	store, mr := newTestStore(t)
	ctx := context.Background()

	session := NewGameSession("carlo", "easy", time.Now().UTC())
	if err := store.Create(ctx, session); err != nil {
		t.Fatalf("Create() failed: %v", err)
	}

	mr.FastForward(110 * time.Minute)
	if err := store.Save(ctx, session); err != nil {
		t.Fatalf("Save() failed: %v", err)
	}
	if ttl := mr.TTL("game_session:" + session.SessionID); ttl != 2*time.Hour {
		t.Errorf("TTL after write = %s, want exactly 2h", ttl)
	}
}

// TestGetUnknownSessionReturnsNotFound covers R5.3.
func TestGetUnknownSessionReturnsNotFound(t *testing.T) {
	store, _ := newTestStore(t)

	_, err := store.Get(context.Background(), "00000000-0000-0000-0000-000000000000")
	if !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("Get() unknown id = %v, want ErrSessionNotFound", err)
	}
}

// TestCacheUnavailableError covers R3.6: an unreachable Cache is reported as
// ErrUnavailable so the API can answer 503.
func TestCacheUnavailableError(t *testing.T) {
	ctx := context.Background()
	store := deadStore()

	err := store.Create(ctx, NewGameSession("carlo", "easy", time.Now().UTC()))
	if !errors.Is(err, ErrUnavailable) {
		t.Errorf("Create() against dead cache = %v, want ErrUnavailable", err)
	}

	_, err = store.Get(ctx, "anything")
	if !errors.Is(err, ErrUnavailable) {
		t.Errorf("Get() against dead cache = %v, want ErrUnavailable", err)
	}
}

// TestCreateOnlyOneSessionPerID covers R5.7: when a Session_ID already has an
// entry, the concurrent creator must not overwrite it - it retries under a
// fresh ID, leaving exactly one session per ID.
func TestCreateOnlyOneSessionPerID(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	first := NewGameSession("first-player", "easy", time.Now().UTC())
	if err := store.Create(ctx, first); err != nil {
		t.Fatalf("first Create() failed: %v", err)
	}
	contestedID := first.SessionID

	// A second creator arrives holding the same Session_ID.
	second := NewGameSession("second-player", "hard", time.Now().UTC())
	second.SessionID = contestedID
	if err := store.Create(ctx, second); err != nil {
		t.Fatalf("second Create() failed: %v", err)
	}

	if second.SessionID == contestedID {
		t.Error("second session kept the contested Session_ID; one of the two sessions was overwritten")
	}

	// The original entry still belongs to the first player.
	stored, err := store.Get(ctx, contestedID)
	if err != nil {
		t.Fatalf("Get(%s) failed: %v", contestedID, err)
	}
	if stored.Nickname != "first-player" {
		t.Errorf("entry under the contested ID belongs to %q, want first-player", stored.Nickname)
	}

	// And the second player has its own, separate entry.
	other, err := store.Get(ctx, second.SessionID)
	if err != nil {
		t.Fatalf("Get(%s) failed: %v", second.SessionID, err)
	}
	if other.Nickname != "second-player" {
		t.Errorf("Nickname = %q, want second-player", other.Nickname)
	}
}

// TestAddShownTwisterPersistsList covers R6.4: the ID is recorded in the
// stored Game_Session, duplicates are ignored, the update resets the 2h
// window (R5.2) and a fresh read sees the full list (R6.1's basis).
func TestAddShownTwisterPersistsList(t *testing.T) {
	store, mr := newTestStore(t)
	ctx := context.Background()

	session := NewGameSession("carlo", "easy", time.Now().UTC())
	if err := store.Create(ctx, session); err != nil {
		t.Fatalf("Create() failed: %v", err)
	}

	updated, err := store.AddShownTwister(ctx, session.SessionID, 7, time.Now().UTC())
	if err != nil {
		t.Fatalf("AddShownTwister(7) failed: %v", err)
	}
	if len(updated.ShownIDs) != 1 || updated.ShownIDs[0] != 7 {
		t.Errorf("ShownIDs = %v, want [7]", updated.ShownIDs)
	}
	if ttl := mr.TTL("game_session:" + session.SessionID); ttl != 2*time.Hour {
		t.Errorf("TTL after mark-shown = %s, want exactly 2h (R5.2)", ttl)
	}

	// The same ID twice stays a single entry.
	if _, err := store.AddShownTwister(ctx, session.SessionID, 7, time.Now().UTC()); err != nil {
		t.Fatalf("second AddShownTwister(7) failed: %v", err)
	}
	if _, err := store.AddShownTwister(ctx, session.SessionID, 9, time.Now().UTC()); err != nil {
		t.Fatalf("AddShownTwister(9) failed: %v", err)
	}

	stored, err := store.Get(ctx, session.SessionID)
	if err != nil {
		t.Fatalf("Get() failed: %v", err)
	}
	if want := []int64{7, 9}; !slices.Equal(stored.ShownIDs, want) {
		t.Errorf("persisted ShownIDs = %v, want %v", stored.ShownIDs, want)
	}
}

// TestAddShownTwisterUnknownSession covers R5.3 on the write path.
func TestAddShownTwisterUnknownSession(t *testing.T) {
	store, _ := newTestStore(t)

	_, err := store.AddShownTwister(context.Background(), "00000000-0000-0000-0000-000000000000", 1, time.Now().UTC())
	if !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("AddShownTwister() unknown id = %v, want ErrSessionNotFound", err)
	}
}

// TestAddShownTwisterCacheUnavailable covers R3.6 on the write path: an
// unreachable Cache surfaces as ErrUnavailable so the API can answer with the
// R6.6 message.
func TestAddShownTwisterCacheUnavailable(t *testing.T) {
	_, err := deadStore().AddShownTwister(context.Background(), "anything", 1, time.Now().UTC())
	if !errors.Is(err, ErrUnavailable) {
		t.Errorf("AddShownTwister() against dead cache = %v, want ErrUnavailable", err)
	}
}

// TestAddShownTwisterMarksPending covers the R7 half of the /next
// bookkeeping: the served twister becomes the pending attempt, alongside the
// already-shown list (R6.4).
func TestAddShownTwisterMarksPending(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	session := NewGameSession("carlo", "easy", time.Now().UTC())
	if err := store.Create(ctx, session); err != nil {
		t.Fatalf("Create() failed: %v", err)
	}

	updated, err := store.AddShownTwister(ctx, session.SessionID, 7, time.Now().UTC())
	if err != nil {
		t.Fatalf("AddShownTwister(7) failed: %v", err)
	}
	if updated.CurrentTwisterID != 7 {
		t.Errorf("CurrentTwisterID = %d, want 7 - the served twister awaits the attempt (R7)", updated.CurrentTwisterID)
	}

	// Re-serving the same ID keeps exactly one shown entry (R6.1) but is
	// still the pending attempt.
	if _, err := store.AddShownTwister(ctx, session.SessionID, 7, time.Now().UTC()); err != nil {
		t.Fatalf("second AddShownTwister(7) failed: %v", err)
	}
	stored, err := store.Get(ctx, session.SessionID)
	if err != nil {
		t.Fatalf("Get() failed: %v", err)
	}
	if want := []int64{7}; !slices.Equal(stored.ShownIDs, want) {
		t.Errorf("persisted ShownIDs = %v, want %v", stored.ShownIDs, want)
	}
	if stored.CurrentTwisterID != 7 {
		t.Errorf("CurrentTwisterID = %d, want 7", stored.CurrentTwisterID)
	}
}

// TestRecordAttemptCorrectAddsPoints: a correct attempt raises the Score by
// the granted points, leaves Hearts alone, consumes the pending twister and
// resets the 2h window (R5.2).
func TestRecordAttemptCorrectAddsPoints(t *testing.T) {
	store, mr := newTestStore(t)
	ctx := context.Background()

	session := NewGameSession("carlo", "hard", time.Now().UTC())
	if err := store.Create(ctx, session); err != nil {
		t.Fatalf("Create() failed: %v", err)
	}
	if _, err := store.AddShownTwister(ctx, session.SessionID, 42, time.Now().UTC()); err != nil {
		t.Fatalf("AddShownTwister() failed: %v", err)
	}

	updated, err := store.RecordAttempt(ctx, session.SessionID, true, 30, time.Now().UTC())
	if err != nil {
		t.Fatalf("RecordAttempt(correct) failed: %v", err)
	}
	if updated.Score != 30 {
		t.Errorf("Score = %d, want 30", updated.Score)
	}
	if updated.Hearts != InitialHearts {
		t.Errorf("Hearts = %d, want %d (a correct attempt costs no Heart)", updated.Hearts, InitialHearts)
	}
	if updated.CurrentTwisterID != 0 {
		t.Errorf("CurrentTwisterID = %d, want 0 (consumed - one attempt per twister)", updated.CurrentTwisterID)
	}
	if ttl := mr.TTL("game_session:" + session.SessionID); ttl != 2*time.Hour {
		t.Errorf("TTL after attempt = %s, want exactly 2h (R5.2)", ttl)
	}

	// Persisted, not just returned.
	stored, err := store.Get(ctx, session.SessionID)
	if err != nil {
		t.Fatalf("Get() failed: %v", err)
	}
	if stored.Score != 30 || stored.CurrentTwisterID != 0 {
		t.Errorf("persisted session = %+v, want Score 30 and no pending twister", stored)
	}
}

// TestRecordAttemptWrongCostsHeart: a wrong attempt spends one Heart, adds no
// points and consumes the pending twister. The Heart count bottoms out at
// zero - R7.3 defines the remaining attempts as exactly 3, 2 or 1.
func TestRecordAttemptWrongCostsHeart(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	session := NewGameSession("carlo", "easy", time.Now().UTC())
	if err := store.Create(ctx, session); err != nil {
		t.Fatalf("Create() failed: %v", err)
	}

	for wantHearts := InitialHearts - 1; wantHearts >= 0; wantHearts-- {
		if _, err := store.AddShownTwister(ctx, session.SessionID, int64(wantHearts+1), time.Now().UTC()); err != nil {
			t.Fatalf("AddShownTwister() failed: %v", err)
		}
		updated, err := store.RecordAttempt(ctx, session.SessionID, false, 10, time.Now().UTC())
		if err != nil {
			t.Fatalf("RecordAttempt(wrong) failed: %v", err)
		}
		if updated.Hearts != wantHearts {
			t.Errorf("Hearts = %d, want %d", updated.Hearts, wantHearts)
		}
		if updated.Score != 0 {
			t.Errorf("Score = %d, want 0 (a wrong attempt awards nothing)", updated.Score)
		}
	}
}

// TestRecordAttemptWithoutPendingTwister: grading nothing is refused with
// ErrNoPendingAttempt and leaves the stored entry untouched - no Hearts, no
// Score, nothing.
func TestRecordAttemptWithoutPendingTwister(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	session := NewGameSession("carlo", "easy", time.Now().UTC())
	if err := store.Create(ctx, session); err != nil {
		t.Fatalf("Create() failed: %v", err)
	}

	if _, err := store.RecordAttempt(ctx, session.SessionID, false, 0, time.Now().UTC()); !errors.Is(err, ErrNoPendingAttempt) {
		t.Errorf("RecordAttempt() without a pending twister = %v, want ErrNoPendingAttempt", err)
	}

	stored, err := store.Get(ctx, session.SessionID)
	if err != nil {
		t.Fatalf("Get() failed: %v", err)
	}
	if stored.Hearts != InitialHearts || stored.Score != 0 {
		t.Errorf("session changed although no attempt was graded: %+v", stored)
	}
}

// TestRecordAttemptUnknownSession / TestRecordAttemptCacheUnavailable: the
// R5.3 / R3.6 error contract on the new write path.
func TestRecordAttemptErrors(t *testing.T) {
	_, err := deadStore().RecordAttempt(context.Background(), "anything", true, 10, time.Now().UTC())
	if !errors.Is(err, ErrUnavailable) {
		t.Errorf("RecordAttempt() against dead cache = %v, want ErrUnavailable", err)
	}

	store, _ := newTestStore(t)
	_, err = store.RecordAttempt(context.Background(), "00000000-0000-0000-0000-000000000000", true, 10, time.Now().UTC())
	if !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("RecordAttempt() unknown id = %v, want ErrSessionNotFound", err)
	}
}

// ---------------------------------------------------------------------------
// Game end: serve/grade timing, end stamp, leaderboard (R10-R12)
// ---------------------------------------------------------------------------

// TestAddShownTwisterStampsServedAt covers R11's clock start: /next's serve
// moment is stored with the pending twister, ready for the grade that
// follows.
func TestAddShownTwisterStampsServedAt(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	session := NewGameSession("carlo", "easy", time.Now().UTC())
	if err := store.Create(ctx, session); err != nil {
		t.Fatalf("Create() failed: %v", err)
	}

	servedAt := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	updated, err := store.AddShownTwister(ctx, session.SessionID, 7, servedAt)
	if err != nil {
		t.Fatalf("AddShownTwister() failed: %v", err)
	}
	if !updated.ServedAt.Equal(servedAt) {
		t.Errorf("ServedAt = %v, want the serve time %v", updated.ServedAt, servedAt)
	}

	stored, err := store.Get(ctx, session.SessionID)
	if err != nil {
		t.Fatalf("Get() failed: %v", err)
	}
	if !stored.ServedAt.Equal(servedAt) {
		t.Errorf("persisted ServedAt = %v, want %v", stored.ServedAt, servedAt)
	}
}

// TestRecordAttemptAppendsAttemptRecord covers R11's per-attempt data: the
// graded twister, its serve-to-grade duration and the outcome are collected
// in the session (the source of attempt_details at game end), while the
// pending twister and its serve time are cleared so the next /next starts a
// fresh measurement.
func TestRecordAttemptAppendsAttemptRecord(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	session := NewGameSession("carlo", "hard", time.Now().UTC())
	if err := store.Create(ctx, session); err != nil {
		t.Fatalf("Create() failed: %v", err)
	}

	servedAt := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	if _, err := store.AddShownTwister(ctx, session.SessionID, 42, servedAt); err != nil {
		t.Fatalf("AddShownTwister() failed: %v", err)
	}
	gradedAt := servedAt.Add(5*time.Second + 500*time.Millisecond)
	updated, err := store.RecordAttempt(ctx, session.SessionID, true, 30, gradedAt)
	if err != nil {
		t.Fatalf("RecordAttempt() failed: %v", err)
	}
	if len(updated.Attempts) != 1 {
		t.Fatalf("Attempts = %+v, want exactly one record", updated.Attempts)
	}
	if want := (AttemptRecord{TwisterID: 42, TimeTakenMS: 5500, Correct: true}); updated.Attempts[0] != want {
		t.Errorf("Attempts[0] = %+v, want %+v (5.5s served -> graded)", updated.Attempts[0], want)
	}
	if !updated.ServedAt.IsZero() {
		t.Errorf("ServedAt = %v, want zero after grading", updated.ServedAt)
	}

	// A wrong attempt appends its own record with success=false.
	if _, err := store.AddShownTwister(ctx, session.SessionID, 43, gradedAt); err != nil {
		t.Fatalf("AddShownTwister() failed: %v", err)
	}
	wrong, err := store.RecordAttempt(ctx, session.SessionID, false, 0, gradedAt.Add(2*time.Second))
	if err != nil {
		t.Fatalf("RecordAttempt(wrong) failed: %v", err)
	}
	if want := (AttemptRecord{TwisterID: 43, TimeTakenMS: 2000, Correct: false}); wrong.Attempts[1] != want {
		t.Fatalf("Attempts[1] = %+v, want %+v", wrong.Attempts[1], want)
	}

	// Both records survive the round trip game end reads them back from.
	stored, err := store.Get(ctx, session.SessionID)
	if err != nil {
		t.Fatalf("Get() failed: %v", err)
	}
	if !slices.Equal(stored.Attempts, wrong.Attempts) {
		t.Errorf("persisted Attempts = %+v, want %+v", stored.Attempts, wrong.Attempts)
	}
}

// TestRecordAttemptClampsTimeTakenMS: attempt_details.time_taken_ms is an
// int4 column, so the measured duration must stay in [0, MaxInt32] or the
// game-end insert would fail and lose the Game_Record. A clock running
// backwards reports 0, an absurd gap saturates, and a session served before
// ServedAt existed reports 0 rather than a garbage value.
func TestRecordAttemptClampsTimeTakenMS(t *testing.T) {
	ctx := context.Background()

	t.Run("clock running backwards", func(t *testing.T) {
		store, _ := newTestStore(t)
		session := NewGameSession("carlo", "easy", time.Now().UTC())
		if err := store.Create(ctx, session); err != nil {
			t.Fatalf("Create() failed: %v", err)
		}
		servedAt := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
		if _, err := store.AddShownTwister(ctx, session.SessionID, 1, servedAt); err != nil {
			t.Fatalf("AddShownTwister() failed: %v", err)
		}
		updated, err := store.RecordAttempt(ctx, session.SessionID, true, 10, servedAt.Add(-time.Hour))
		if err != nil {
			t.Fatalf("RecordAttempt() failed: %v", err)
		}
		if updated.Attempts[0].TimeTakenMS != 0 {
			t.Errorf("TimeTakenMS = %d, want 0 (negative durations clamp up)", updated.Attempts[0].TimeTakenMS)
		}
	})

	t.Run("duration beyond int4", func(t *testing.T) {
		store, _ := newTestStore(t)
		session := NewGameSession("carlo", "easy", time.Now().UTC())
		if err := store.Create(ctx, session); err != nil {
			t.Fatalf("Create() failed: %v", err)
		}
		// 40 days between serve and grade: 3_456_000_000 ms, past int4.
		gradedAt := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
		if _, err := store.AddShownTwister(ctx, session.SessionID, 1, gradedAt.AddDate(0, 0, -40)); err != nil {
			t.Fatalf("AddShownTwister() failed: %v", err)
		}
		updated, err := store.RecordAttempt(ctx, session.SessionID, true, 10, gradedAt)
		if err != nil {
			t.Fatalf("RecordAttempt() failed: %v", err)
		}
		if got, want := updated.Attempts[0].TimeTakenMS, int(maxTimeTakenMS); got != want {
			t.Errorf("TimeTakenMS = %d, want the int4 cap %d", got, want)
		}
	})

	t.Run("no serve timestamp", func(t *testing.T) {
		store, _ := newTestStore(t)
		// A pending twister without ServedAt: the field arrived after the
		// session was written - grading must still produce a valid record.
		session := NewGameSession("carlo", "easy", time.Now().UTC())
		session.CurrentTwisterID = 9
		if err := store.Create(ctx, session); err != nil {
			t.Fatalf("Create() failed: %v", err)
		}
		updated, err := store.RecordAttempt(ctx, session.SessionID, false, 0, time.Now().UTC())
		if err != nil {
			t.Fatalf("RecordAttempt() failed: %v", err)
		}
		if updated.Attempts[0].TimeTakenMS != 0 {
			t.Errorf("TimeTakenMS = %d, want 0 without a serve timestamp", updated.Attempts[0].TimeTakenMS)
		}
	})
}

// TestMarkEndedStampsOnce covers R10's exactly-once premise: the first call
// stamps and persists EndedAt, a later call gets ErrAlreadyEnded - the guard
// that keeps two racing game-end paths from writing two Game_Records - and
// the original stamp never moves. The R5.3 / R3.6 error contract holds like
// on every other session operation.
func TestMarkEndedStampsOnce(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	session := NewGameSession("carlo", "easy", time.Now().UTC())
	if err := store.Create(ctx, session); err != nil {
		t.Fatalf("Create() failed: %v", err)
	}

	endedAt := time.Date(2026, 10, 27, 10, 30, 0, 0, time.UTC)
	ended, err := store.MarkEnded(ctx, session.SessionID, endedAt)
	if err != nil {
		t.Fatalf("MarkEnded() failed: %v", err)
	}
	if !ended.EndedAt.Equal(endedAt) {
		t.Errorf("EndedAt = %v, want %v", ended.EndedAt, endedAt)
	}
	stored, err := store.Get(ctx, session.SessionID)
	if err != nil {
		t.Fatalf("Get() failed: %v", err)
	}
	if !stored.EndedAt.Equal(endedAt) {
		t.Errorf("persisted EndedAt = %v, want %v", stored.EndedAt, endedAt)
	}

	// The second end stands down and leaves the first stamp in place.
	if _, err := store.MarkEnded(ctx, session.SessionID, endedAt.Add(time.Hour)); !errors.Is(err, ErrAlreadyEnded) {
		t.Errorf("second MarkEnded() = %v, want ErrAlreadyEnded", err)
	}
	stored, err = store.Get(ctx, session.SessionID)
	if err != nil {
		t.Fatalf("Get() failed: %v", err)
	}
	if !stored.EndedAt.Equal(endedAt) {
		t.Errorf("EndedAt moved to %v, want the first stamp %v", stored.EndedAt, endedAt)
	}

	if _, err := store.MarkEnded(ctx, "00000000-0000-0000-0000-000000000000", endedAt); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("MarkEnded() unknown id = %v, want ErrSessionNotFound", err)
	}
	if _, err := deadStore().MarkEnded(ctx, "anything", endedAt); !errors.Is(err, ErrUnavailable) {
		t.Errorf("MarkEnded() against dead cache = %v, want ErrUnavailable", err)
	}
}

// TestLeaderboardKeepsHighestScore covers R12.6 via ZADD's GT flag: a
// nickname's entry only ever rises, so a later worse run never demotes it.
// Boards per difficulty stay independent (decision 16), and equal scores of
// different nicknames simply coexist - no tie-break exists (decision 5).
func TestLeaderboardKeepsHighestScore(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	for _, score := range []int{50, 30, 80} {
		if err := store.SaveLeaderboardScore(ctx, "easy", "carlo", score); err != nil {
			t.Fatalf("SaveLeaderboardScore(%d) failed: %v", score, err)
		}
	}
	easy, err := store.TopLeaderboard(ctx, "easy", 10)
	if err != nil {
		t.Fatalf("TopLeaderboard() failed: %v", err)
	}
	if len(easy) != 1 || easy[0].Nickname != "carlo" || easy[0].Score != 80 {
		t.Errorf("easy board = %+v, want a single carlo entry at 80 (highest run kept)", easy)
	}

	// The medium board is untouched by easy traffic.
	if err := store.SaveLeaderboardScore(ctx, "medium", "carlo", 5); err != nil {
		t.Fatalf("SaveLeaderboardScore(medium) failed: %v", err)
	}
	medium, err := store.TopLeaderboard(ctx, "medium", 10)
	if err != nil {
		t.Fatalf("TopLeaderboard() failed: %v", err)
	}
	if len(medium) != 1 || medium[0].Score != 5 {
		t.Errorf("medium board = %+v, want carlo at 5 on its own board", medium)
	}

	// Equal scores coexist: both nicknames are listed, nothing is broken.
	if err := store.SaveLeaderboardScore(ctx, "easy", "marta", 80); err != nil {
		t.Fatalf("SaveLeaderboardScore(marta) failed: %v", err)
	}
	easy, err = store.TopLeaderboard(ctx, "easy", 10)
	if err != nil {
		t.Fatalf("TopLeaderboard() failed: %v", err)
	}
	if len(easy) != 2 || easy[0].Score != 80 || easy[1].Score != 80 {
		t.Errorf("easy board = %+v, want carlo and marta tied at 80", easy)
	}
}

// TestTopLeaderboardOrdersAndLimits: highest score first, capped at the
// requested number - and an empty board is an empty non-nil slice, so the
// HTTP response renders [] instead of null.
func TestTopLeaderboardOrdersAndLimits(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	empty, err := store.TopLeaderboard(ctx, "hard", 10)
	if err != nil {
		t.Fatalf("TopLeaderboard() on an empty board failed: %v", err)
	}
	if empty == nil || len(empty) != 0 {
		t.Errorf("empty board = %#v, want a non-nil empty slice", empty)
	}

	for i := 1; i <= 12; i++ {
		if err := store.SaveLeaderboardScore(ctx, "hard", fmt.Sprintf("player-%02d", i), i*10); err != nil {
			t.Fatalf("SaveLeaderboardScore(%d) failed: %v", i*10, err)
		}
	}
	top, err := store.TopLeaderboard(ctx, "hard", 10)
	if err != nil {
		t.Fatalf("TopLeaderboard() failed: %v", err)
	}
	if len(top) != 10 {
		t.Fatalf("board length = %d, want the limit 10", len(top))
	}
	if top[0].Nickname != "player-12" || top[0].Score != 120 {
		t.Errorf("top entry = %+v, want player-12 at 120", top[0])
	}
	if top[9].Nickname != "player-03" || top[9].Score != 30 {
		t.Errorf("tenth entry = %+v, want player-03 at 30", top[9])
	}
	for i := 1; i < len(top); i++ {
		if top[i-1].Score < top[i].Score {
			t.Fatalf("board not ordered by score descending at %d: %+v", i, top)
		}
	}
}

// TestLeaderboardCacheUnavailable: both directions report ErrUnavailable so
// the API can answer 503 (R3.6 style).
func TestLeaderboardCacheUnavailable(t *testing.T) {
	dead := deadStore()
	if err := dead.SaveLeaderboardScore(context.Background(), "easy", "carlo", 10); !errors.Is(err, ErrUnavailable) {
		t.Errorf("SaveLeaderboardScore() against dead cache = %v, want ErrUnavailable", err)
	}
	if _, err := dead.TopLeaderboard(context.Background(), "easy", 10); !errors.Is(err, ErrUnavailable) {
		t.Errorf("TopLeaderboard() against dead cache = %v, want ErrUnavailable", err)
	}
}
