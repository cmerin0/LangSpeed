package cache

import (
	"context"
	"errors"
	"fmt"
	"math"
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
	// ErrNoPendingAttempt: RecordAttempt was called while no tongue-twister
	// awaits an attempt (CurrentTwisterID is 0) - either none was served yet,
	// or the pending one was already graded. Grading nothing would corrupt
	// Hearts or Score, so the attempt is refused (R7).
	ErrNoPendingAttempt = errors.New("no tongue-twister is awaiting an attempt")
	// ErrAlreadyEnded: the Game_Session already carries an EndedAt stamp, so
	// the game is finished and its Game_Record must not be written a second
	// time (R10) - the caller stands down instead of persisting a duplicate.
	ErrAlreadyEnded = errors.New("the game has already ended")
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

// AddShownTwister appends twisterID to the session's already-shown list,
// marks it as the tongue-twister currently awaiting an attempt
// (CurrentTwisterID, R7) and stamps when it was served (ServedAt, R11's basis
// for time_taken_ms) - the R6.4 bookkeeping that guarantees a tongue-twister
// is recorded before it is ever returned to the player (R6.1 relies on this
// list to exclude repeats).
//
// The session is re-read first so the append starts from the freshest state;
// an ID already present is not appended twice. Read failures surface
// ErrSessionNotFound / ErrUnavailable unchanged; a failed write leaves the
// stored entry as it was, because Save serializes before touching Redis
// (R20.4). Callers map those errors to R6.5 / R6.6 responses.
func (s *Store) AddShownTwister(ctx context.Context, sessionID string, twisterID int64, now time.Time) (*GameSession, error) {
	session, err := s.Get(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	// Re-serving an already-shown ID must not duplicate it in the list
	// (R6.1's basis), but it still becomes the pending attempt.
	alreadyShown := false
	for _, shown := range session.ShownIDs {
		if shown == twisterID {
			alreadyShown = true
			break
		}
	}
	if !alreadyShown {
		session.ShownIDs = append(session.ShownIDs, twisterID)
	}
	session.CurrentTwisterID = twisterID
	session.ServedAt = now.UTC()
	if err := s.Save(ctx, session); err != nil {
		return nil, err
	}
	return session, nil
}

// RecordAttempt grades the tongue-twister the session is currently typing
// (R7-R9) and persists the resulting state in one write:
//
//   - a correct attempt adds points to the Score;
//   - a wrong attempt costs one Heart (never below zero - R7.3 defines the
//     remaining attempts as exactly the 3, 2 or 1 Hearts left);
//   - either way the pending tongue-twister is consumed (CurrentTwisterID
//     and ServedAt cleared), so one served twister can be graded exactly once
//     and a resubmission can never double-score or double-charge;
//   - the graded attempt is appended to Attempts (R11) with how long the
//     player had the twister: now - ServedAt, clamped into the int4 range of
//     attempt_details.time_taken_ms (see clampTimeTakenMS).
//
// `now` is passed in so grading time is injectable in tests; handlers pass
// time.Now().UTC(). Grading without a pending twister returns
// ErrNoPendingAttempt and leaves the stored entry untouched. Read and write
// failures surface like AddShownTwister's, so the HTTP layer can answer
// R6.5 / R6.6-style 503s.
func (s *Store) RecordAttempt(ctx context.Context, sessionID string, correct bool, points int, now time.Time) (*GameSession, error) {
	session, err := s.Get(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if session.CurrentTwisterID == 0 {
		return nil, ErrNoPendingAttempt
	}

	if correct {
		session.Score += points
	} else if session.Hearts > 0 {
		session.Hearts--
	}
	session.Attempts = append(session.Attempts, AttemptRecord{
		TwisterID:   session.CurrentTwisterID,
		TimeTakenMS: timeTakenMS(session.ServedAt, now),
		Correct:     correct,
	})
	session.CurrentTwisterID = 0
	session.ServedAt = time.Time{}

	if err := s.Save(ctx, session); err != nil {
		return nil, err
	}
	return session, nil
}

// maxTimeTakenMS is math.MaxInt32: attempt_details.time_taken_ms is an
// integer (int4) column, so a duration beyond it would make the game-end
// insert fail and lose the whole Game_Record (R10).
const maxTimeTakenMS = int64(math.MaxInt32)

// timeTakenMS measures how long the player had the pending twister: the
// serve-to-grade duration in milliseconds, clamped into [0, MaxInt32] so a
// clock running backwards reports 0 and a pathological gap (a session kept
// alive by /next reads for weeks) can never overflow the int4 column. A
// session served before ServedAt existed reports 0.
func timeTakenMS(servedAt, gradedAt time.Time) int {
	if servedAt.IsZero() {
		return 0
	}
	ms := gradedAt.Sub(servedAt).Milliseconds()
	switch {
	case ms < 0:
		return 0
	case ms > maxTimeTakenMS:
		return int(maxTimeTakenMS)
	default:
		return int(ms)
	}
}

// MarkEnded stamps EndedAt on the Game_Session - and only ever on the first
// call (R10: exactly one Game_Record per finished game). The first caller
// receives the ended session; a later one gets ErrAlreadyEnded, which the
// game-end path treats as "a concurrent request is already persisting this
// game" and turns into a no-op instead of a duplicate record. Unknown or
// expired sessions yield ErrSessionNotFound, an unreachable Cache
// ErrUnavailable (R5.3, R3.6).
func (s *Store) MarkEnded(ctx context.Context, sessionID string, endedAt time.Time) (*GameSession, error) {
	session, err := s.Get(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if !session.EndedAt.IsZero() {
		return nil, ErrAlreadyEnded
	}
	session.EndedAt = endedAt.UTC()
	if err := s.Save(ctx, session); err != nil {
		return nil, err
	}
	return session, nil
}
