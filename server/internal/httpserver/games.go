package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"langspeed/internal/cache"
)

// requestTimeout bounds one Cache round-trip so a struggling Cache cannot
// hold a request open past the frontend's 10s budget (R18.5).
const requestTimeout = 3 * time.Second

// maxBodyBytes caps request bodies; the start-game payload is tiny.
const maxBodyBytes = 1 << 20 // 1 MiB

// maxNicknameLength is the Nickname limit (R3.1, R3.3).
const maxNicknameLength = 32

// validDifficulties is the fixed Difficulty set (glossary, R3.4).
var validDifficulties = []string{"easy", "medium", "hard"}

// startGameRequest is the POST /api/games body. Fields are pointers so an
// absent field (R5.6) can be told apart from an explicitly empty one (R3.2).
type startGameRequest struct {
	Nickname   *string `json:"nickname"`
	Difficulty *string `json:"difficulty"`
}

// gameStateResponse is the Game_Session state returned to clients (R5.5).
// Only R5.5's Session_ID is mandated; the remaining fields give the frontend
// everything R18 needs to render the HUD right away.
type gameStateResponse struct {
	SessionID  string `json:"session_id"`
	Nickname   string `json:"nickname"`
	Difficulty string `json:"difficulty"`
	Hearts     int    `json:"hearts"`
	Score      int    `json:"score"`
}

func toStateResponse(session *cache.GameSession) gameStateResponse {
	return gameStateResponse{
		SessionID:  session.SessionID,
		Nickname:   session.Nickname,
		Difficulty: session.Difficulty,
		Hearts:     session.Hearts,
		Score:      session.Score,
	}
}

// handleStartGame implements the start-game endpoint (R3) combined with
// Game_Session initialization (R5):
//
//	POST /api/games  {"nickname": "...", "difficulty": "easy"}
//	  -> 201 with the Session_ID (R3.5, R5.5)
//	  -> 422 naming every invalid or missing field (R3.2-R3.4, R5.6)
//	  -> 400 for a malformed body
//	  -> 503 when the Cache is unavailable (R3.6)
func (s *Server) handleStartGame(w http.ResponseWriter, r *http.Request) {
	var req startGameRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxBodyBytes)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	if problem := validateStartRequest(&req); problem != "" {
		writeError(w, http.StatusUnprocessableEntity, problem)
		return
	}
	nickname := strings.TrimSpace(*req.Nickname)
	difficulty := *req.Difficulty

	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	session := cache.NewGameSession(nickname, difficulty, time.Now().UTC())
	if err := s.sessions.Create(ctx, session); err != nil {
		if errors.Is(err, cache.ErrUnavailable) {
			// Operators get the internal cause; clients only the summary (R21.4).
			s.log.Warn("game session could not be created", "error", err.Error())
			writeError(w, http.StatusServiceUnavailable, "the service is temporarily unavailable")
			return
		}
		s.log.Error("game session could not be created", "error", err.Error())
		writeError(w, http.StatusInternalServerError, "the game session could not be created")
		return
	}

	// R21.5: Session_ID and Difficulty only - the player-chosen Nickname must
	// never appear in a log line.
	s.log.Info("game session created", "session_id", session.SessionID, "difficulty", session.Difficulty)

	writeJSON(w, http.StatusCreated, toStateResponse(session))
}

// handleGetSession returns the live state for a Session_ID (glossary lookup).
// An unknown or expired Session_ID produces the error response required by
// R5.3; a successful read also refreshes the 2h inactivity window (R5.2),
// which happens inside the Store.
func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	session, err := s.sessions.Get(ctx, r.PathValue("id"))
	switch {
	case errors.Is(err, cache.ErrSessionNotFound):
		writeError(w, http.StatusNotFound, "session not found or has expired")
	case errors.Is(err, cache.ErrUnavailable):
		s.log.Warn("game session could not be read", "error", err.Error())
		writeError(w, http.StatusServiceUnavailable, "the service is temporarily unavailable")
	case err != nil:
		// Stored bytes failed to deserialize; the R20.5 error propagated as-is.
		s.log.Error("game session could not be read", "error", err.Error())
		writeError(w, http.StatusInternalServerError, "the game session could not be read")
	default:
		writeJSON(w, http.StatusOK, toStateResponse(session))
	}
}

// validateStartRequest returns one client-safe message listing every problem
// with the request, or "" when it is valid:
//
//   - an absent/null field        -> "missing required field(s): ..." (R5.6)
//   - empty/whitespace nickname   -> "nickname is required" (R3.2)
//   - nickname over 32 characters -> message naming the limit (R3.3)
//   - unknown difficulty          -> message listing all three values (R3.4)
//
// When several fields are wrong, all problems appear in one 422 response.
func validateStartRequest(req *startGameRequest) string {
	var missing []string
	if req.Nickname == nil {
		missing = append(missing, "nickname")
	}
	if req.Difficulty == nil {
		missing = append(missing, "difficulty")
	}
	if len(missing) > 0 {
		return "missing required field(s): " + strings.Join(missing, ", ")
	}

	var problems []string
	nickname := strings.TrimSpace(*req.Nickname)
	switch {
	case nickname == "":
		problems = append(problems, "nickname is required")
	case utf8.RuneCountInString(nickname) > maxNicknameLength:
		problems = append(problems,
			fmt.Sprintf("nickname must be at most %d characters", maxNicknameLength))
	}
	if !isValidDifficulty(*req.Difficulty) {
		problems = append(problems,
			"difficulty must be one of: "+strings.Join(validDifficulties, ", "))
	}
	return strings.Join(problems, "; ")
}

// isValidDifficulty reports whether difficulty is one of the three accepted
// values (glossary: easy, medium, hard).
func isValidDifficulty(difficulty string) bool {
	for _, valid := range validDifficulties {
		if difficulty == valid {
			return true
		}
	}
	return false
}
