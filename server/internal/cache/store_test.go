package cache

import (
	"context"
	"errors"
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
