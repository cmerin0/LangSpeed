package cache

import (
	"context"
	"errors"
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

	updated, err := store.AddShownTwister(ctx, session.SessionID, 7)
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
	if _, err := store.AddShownTwister(ctx, session.SessionID, 7); err != nil {
		t.Fatalf("second AddShownTwister(7) failed: %v", err)
	}
	if _, err := store.AddShownTwister(ctx, session.SessionID, 9); err != nil {
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

	_, err := store.AddShownTwister(context.Background(), "00000000-0000-0000-0000-000000000000", 1)
	if !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("AddShownTwister() unknown id = %v, want ErrSessionNotFound", err)
	}
}

// TestAddShownTwisterCacheUnavailable covers R3.6 on the write path: an
// unreachable Cache surfaces as ErrUnavailable so the API can answer with the
// R6.6 message.
func TestAddShownTwisterCacheUnavailable(t *testing.T) {
	_, err := deadStore().AddShownTwister(context.Background(), "anything", 1)
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

	updated, err := store.AddShownTwister(ctx, session.SessionID, 7)
	if err != nil {
		t.Fatalf("AddShownTwister(7) failed: %v", err)
	}
	if updated.CurrentTwisterID != 7 {
		t.Errorf("CurrentTwisterID = %d, want 7 - the served twister awaits the attempt (R7)", updated.CurrentTwisterID)
	}

	// Re-serving the same ID keeps exactly one shown entry (R6.1) but is
	// still the pending attempt.
	if _, err := store.AddShownTwister(ctx, session.SessionID, 7); err != nil {
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
	if _, err := store.AddShownTwister(ctx, session.SessionID, 42); err != nil {
		t.Fatalf("AddShownTwister() failed: %v", err)
	}

	updated, err := store.RecordAttempt(ctx, session.SessionID, true, 30)
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
		if _, err := store.AddShownTwister(ctx, session.SessionID, int64(wantHearts+1)); err != nil {
			t.Fatalf("AddShownTwister() failed: %v", err)
		}
		updated, err := store.RecordAttempt(ctx, session.SessionID, false, 10)
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

	if _, err := store.RecordAttempt(ctx, session.SessionID, false, 0); !errors.Is(err, ErrNoPendingAttempt) {
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
	_, err := deadStore().RecordAttempt(context.Background(), "anything", true, 10)
	if !errors.Is(err, ErrUnavailable) {
		t.Errorf("RecordAttempt() against dead cache = %v, want ErrUnavailable", err)
	}

	store, _ := newTestStore(t)
	_, err = store.RecordAttempt(context.Background(), "00000000-0000-0000-0000-000000000000", true, 10)
	if !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("RecordAttempt() unknown id = %v, want ErrSessionNotFound", err)
	}
}
