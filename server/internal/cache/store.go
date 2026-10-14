package cache

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// SessionTTL is how long a Game_Session survives without activity: exactly 2
// hours, refreshed on every read and write (R5.2, where inactivity means "no
// read or write operation on that Cache entry").
const SessionTTL = 2 * time.Hour

// maxCreateAttempts bounds the retry loop that guarantees at most one
// Game_Session exists per Session_ID (R5.7). Collisions are UUID collisions -
// practically impossible, but the guarantee must hold regardless.
const maxCreateAttempts = 3

// Errors surfaced to callers; the HTTP layer maps them to status codes.
var (
	// ErrSessionNotFound: the Session_ID has no entry in the Cache - it never
	// existed or the 2h window elapsed (R5.3).
	ErrSessionNotFound = errors.New("session not found or has expired")
	// ErrUnavailable: the Cache cannot be reached (R3.6).
	ErrUnavailable = errors.New("cache is temporarily unavailable")
)

// Store persists Game_Sessions in Redis under the key game_session:{id}.
type Store struct {
	client *redis.Client
	ttl    time.Duration
}

// New connects to addr. Timeouts are deliberately short so a downed Cache
// surfaces as ErrUnavailable well inside the frontend's 10s request budget
// (R18.5), letting the API answer 503 quickly (R3.6).
func New(addr string) *Store {
	return NewWithClient(redis.NewClient(&redis.Options{
		Addr:         addr,
		DialTimeout:  2 * time.Second,
		ReadTimeout:  1 * time.Second,
		WriteTimeout: 1 * time.Second,
	}))
}

// NewWithClient builds a Store around an existing client (used by tests).
func NewWithClient(client *redis.Client) *Store {
	return &Store{client: client, ttl: SessionTTL}
}

// Close releases the underlying connection pool.
func (s *Store) Close() error { return s.client.Close() }

// key returns the Redis key for one Game_Session.
func key(sessionID string) string { return "game_session:" + sessionID }

// Create stores a new Game_Session with a 2h expiry (R5.2).
//
// The write uses SET NX, so two concurrent creations can never both win for
// the same Session_ID - on the (astronomically unlikely) collision a fresh
// Session_ID is generated and the attempt retried (R5.7). Nothing reaches the
// Cache unless serialization of the full object succeeded (R20.4), so no
// partial Game_Session can ever exist (R5.6).
func (s *Store) Create(ctx context.Context, session *GameSession) error {
	for attempt := 1; attempt <= maxCreateAttempts; attempt++ {
		data, err := Serialize(session)
		if err != nil {
			return err
		}
		created, err := s.client.SetNX(ctx, key(session.SessionID), data, s.ttl).Result()
		if err != nil {
			return fmt.Errorf("%w: %v", ErrUnavailable, err)
		}
		if created {
			return nil
		}
		session.SessionID = uuid.NewString() // R5.7: retry under a unique ID
	}
	return errors.New("no unique session id could be allocated")
}

// Get returns the Game_Session for sessionID and refreshes the inactivity
// window in the same atomic operation (GETEX), so every read keeps the entry
// alive for another SessionTTL (R5.2).
//
// A missing key yields ErrSessionNotFound (R5.3), an unreachable Cache
// yields ErrUnavailable (R3.6), and corrupt stored bytes propagate the
// deserialization error with no partial object (R20.5).
func (s *Store) Get(ctx context.Context, sessionID string) (*GameSession, error) {
	data, err := s.client.GetEx(ctx, key(sessionID), s.ttl).Result()
	if errors.Is(err, redis.Nil) {
		return nil, ErrSessionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return Deserialize([]byte(data))
}

// Save writes a complete Game_Session back to the Cache and resets the
// inactivity window - any write counts as activity (R5.2). Serialization
// failures leave the stored entry untouched (R20.4).
func (s *Store) Save(ctx context.Context, session *GameSession) error {
	data, err := Serialize(session)
	if err != nil {
		return err
	}
	if _, err := s.client.Set(ctx, key(session.SessionID), data, s.ttl).Result(); err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return nil
}

// AddShownTwister appends twisterID to the session's already-shown list and
// persists the update - the R6.4 bookkeeping that guarantees a tongue-twister
// is recorded before it is ever returned to the player (R6.1 relies on this
// list to exclude repeats).
//
// The session is re-read first so the append starts from the freshest state;
// an ID already present is not appended twice. Read failures surface
// ErrSessionNotFound / ErrUnavailable unchanged; a failed write leaves the
// stored entry as it was, because Save serializes before touching Redis
// (R20.4). Callers map those errors to R6.5 / R6.6 responses.
func (s *Store) AddShownTwister(ctx context.Context, sessionID string, twisterID int64) (*GameSession, error) {
	session, err := s.Get(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	for _, shown := range session.ShownIDs {
		if shown == twisterID {
			return session, nil
		}
	}
	session.ShownIDs = append(session.ShownIDs, twisterID)
	if err := s.Save(ctx, session); err != nil {
		return nil, err
	}
	return session, nil
}
