// Package cache implements the Redis-backed "Cache" from the requirements
// glossary: live Game_Sessions (Requirements 3, 5, 7-11 and 20) and the
// Leaderboard (Requirement 12).
package cache

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// InitialHearts is the Heart count every Game_Session starts with (R5.1,
// later enforced by R9.1).
const InitialHearts = 3

// AttemptRecord is one graded attempt, kept in the Game_Session until the
// game ends so R11's Attempt_Details rows (tongue twister, time taken,
// success) can be written together with the Game_Record in one transaction
// (R10). It is deliberately a struct of scalars in grading order: json.Marshal
// emits fields in declaration order and the slice keeps insertion order, so
// serializing the same state twice stays byte-identical (R20.1).
type AttemptRecord struct {
	TwisterID   int64 `json:"tongue_twister_id"`
	TimeTakenMS int   `json:"time_taken_ms"`
	Correct     bool  `json:"correct"`
}

// GameSession is the live, in-progress state of one Game stored in Redis and
// identified by its Session_ID (glossary).
//
// R5.1 mandates every field below: Session_ID, Nickname, Difficulty, current
// Heart count (3), current Score (0), the list of Tongue_Twister IDs already
// shown (empty) and a creation timestamp. The remaining fields are attempt
// state added by later features - CurrentTwisterID (R7), ServedAt/Attempts
// (R11) and EndedAt (R10) - and none of them changes the required initial
// values of R5.1's seven fields.
//
// The struct deliberately contains no maps: json.Marshal emits struct fields
// in declaration order, so serializing the same state twice always produces
// identical byte sequences (R20.1), and every field round-trips losslessly
// (R20.2, R20.3).
type GameSession struct {
	SessionID  string    `json:"session_id"`
	Nickname   string    `json:"nickname"`
	Difficulty string    `json:"difficulty"`
	Hearts     int       `json:"hearts"`
	Score      int       `json:"score"`
	ShownIDs   []int64   `json:"shown_ids"`
	CreatedAt  time.Time `json:"created_at"`
	// CurrentTwisterID is the tongue-twister the player is currently typing:
	// set when /next serves one, cleared when its attempt is graded (R7). It
	// exists so one served twister can only ever be graded once - resubmitting
	// cannot double-score or double-charge hearts. Zero means "no attempt is
	// pending", which the API reports as a 409 rather than guessing.
	CurrentTwisterID int64 `json:"current_twister_id"`
	// ServedAt is when the pending twister was handed out; grading turns the
	// difference to the grading time into attempt_details.time_taken_ms
	// (R11). Zero means no twister is pending, mirroring CurrentTwisterID -
	// and a session created before this field existed simply reports 0 ms.
	ServedAt time.Time `json:"served_at"`
	// Attempts collects every graded attempt of this game in grading order:
	// the in-session source of R11's Attempt_Details rows, written together
	// with the Game_Record (R10) when the game ends. One entry per graded
	// twister, because one attempt per twister is all a session allows
	// (decisions 13 and 6).
	Attempts []AttemptRecord `json:"attempts"`
	// EndedAt is zero while the game runs. Game end (the last Heart gone, or
	// the pool completed with hearts left - decision 10) stamps it exactly
	// once (Store.MarkEnded): it refuses every later request with the Step 4
	// "the game has already ended" 409, makes the Game_Record write
	// idempotent (R10), and gives game_records.ended_at its timestamp.
	EndedAt time.Time `json:"ended_at"`
}

// NewGameSession builds an initialized Game_Session with a server-generated
// Session_ID (R3.5, R5.1). ServedAt and EndedAt start zero (no twister
// pending, game running); Attempts starts as an empty non-nil list so
// serialization is stable from the first write.
func NewGameSession(nickname, difficulty string, now time.Time) *GameSession {
	return &GameSession{
		SessionID:  uuid.NewString(),
		Nickname:   nickname,
		Difficulty: difficulty,
		Hearts:     InitialHearts,
		Score:      0,
		ShownIDs:   []int64{},
		CreatedAt:  now.UTC(),
		Attempts:   []AttemptRecord{},
	}
}

// Serialize encodes a complete Game_Session. On failure it returns no bytes
// at all, so callers can never write partial state to the Cache (R20.4).
func Serialize(session *GameSession) ([]byte, error) {
	if session == nil {
		return nil, errors.New("game session could not be serialized: session is nil")
	}
	data, err := json.Marshal(session)
	if err != nil {
		return nil, fmt.Errorf("game session could not be serialized: %w", err)
	}
	return data, nil
}

// Deserialize decodes stored bytes into a complete Game_Session. On failure
// it returns a nil session and the error - never a partially populated
// object (R20.5).
func Deserialize(data []byte) (*GameSession, error) {
	var session GameSession
	if err := json.Unmarshal(data, &session); err != nil {
		return nil, fmt.Errorf("game session could not be deserialized: %w", err)
	}
	return &session, nil
}
