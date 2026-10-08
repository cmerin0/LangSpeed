// Package cache implements the Redis-backed "Cache" from the requirements
// glossary: live Game_Sessions (Requirements 3, 5 and 20) and - in later
// features - the Leaderboard (Requirement 12).
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

// GameSession is the live, in-progress state of one Game stored in Redis and
// identified by its Session_ID (glossary).
//
// R5.1 mandates every field below: Session_ID, Nickname, Difficulty, current
// Heart count (3), current Score (0), the list of Tongue_Twister IDs already
// shown (empty) and a creation timestamp.
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
}

// NewGameSession builds an initialized Game_Session with a server-generated
// Session_ID (R3.5, R5.1).
func NewGameSession(nickname, difficulty string, now time.Time) *GameSession {
	return &GameSession{
		SessionID:  uuid.NewString(),
		Nickname:   nickname,
		Difficulty: difficulty,
		Hearts:     InitialHearts,
		Score:      0,
		ShownIDs:   []int64{},
		CreatedAt:  now.UTC(),
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
