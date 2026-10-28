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
	"langspeed/internal/store"
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
// everything R18 needs to render the HUD right away. Game_over (Step 5)
// tells a reloading client that the game is finished even when Hearts remain
// - the win half of decision 10 - so no further attempt is attempted.
type gameStateResponse struct {
	SessionID  string `json:"session_id"`
	Nickname   string `json:"nickname"`
	Difficulty string `json:"difficulty"`
	Hearts     int    `json:"hearts"`
	Score      int    `json:"score"`
	GameOver   bool   `json:"game_over"`
}

func toStateResponse(session *cache.GameSession) gameStateResponse {
	return gameStateResponse{
		SessionID:  session.SessionID,
		Nickname:   session.Nickname,
		Difficulty: session.Difficulty,
		Hearts:     session.Hearts,
		Score:      session.Score,
		// EndedAt marks both outcomes; the Hearts check also covers a lost
		// session whose end stamp could not be written (best-effort, see
		// endGame) - it is over either way.
		GameOver: !session.EndedAt.IsZero() || session.Hearts <= 0,
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

// nextTwisterResponse is a successful next-twister payload. Hearts are
// mandatory: the request modified Game_Session state (the already-shown list),
// and R9.5 requires the current Heart count in every such response. Score
// rides along so the HUD stays in sync without an extra read.
type nextTwisterResponse struct {
	TongueTwisterID int64  `json:"tongue_twister_id"`
	Text            string `json:"text"`
	Difficulty      string `json:"difficulty"`
	Hearts          int    `json:"hearts"`
	Score           int    `json:"score"`
}

// completedResponse is the R6.3 notification that the session has seen every
// tongue-twister for its difficulty - and, with Hearts left, the moment the
// player has won (decision 10): the same request writes the Game_Record,
// its attempt details and the leaderboard entry (R10-R12, decision 7's
// teardown), well inside R6.3's 3-second budget. Game_over, Hearts and Score
// ride along so the frontend can finish the game without a second request.
type completedResponse struct {
	Completed bool   `json:"completed"`
	Message   string `json:"message"`
	GameOver  bool   `json:"game_over"`
	Hearts    int    `json:"hearts"`
	Score     int    `json:"score"`
}

// handleNextTwister implements tongue-twister selection:
//
//	GET /api/games/{id}/next
//	  -> 200 with the next tongue-twister (R6.2), recorded as shown first
//	     (R6.4) so a repeat can never be served (R6.1)
//	  -> 200 {"completed": true, ...} when the pool is exhausted (R6.3) -
//	     the win half of decision 10: this request also persists the
//	     finished game (R10-R12), so a later call answers 409 instead
//	  -> 404 when the difficulty has no active content (R4.6)
//	  -> 404 for an unknown or expired Session_ID (R5.3)
//	  -> 409 when the game has already ended (lost, or completed already)
//	  -> 503 when the Cache cannot be read (R6.5) or written (R6.6) - the
//	     R6.6 response never carries a tongue-twister
func (s *Server) handleNextTwister(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	sessionID := r.PathValue("id")
	session, err := s.sessions.Get(ctx, sessionID)
	switch {
	case errors.Is(err, cache.ErrSessionNotFound):
		writeError(w, http.StatusNotFound, "session not found or has expired")
		return
	case errors.Is(err, cache.ErrUnavailable):
		// R6.5: the already-shown list could not be read, so no selection can
		// be trusted; state is preserved because the read changed nothing.
		s.log.Warn("game session could not be read", "error", err.Error())
		writeError(w, http.StatusServiceUnavailable, "the session state could not be retrieved")
		return
	case err != nil:
		s.log.Error("game session could not be read", "error", err.Error())
		writeError(w, http.StatusInternalServerError, "the game session could not be read")
		return
	}

	// A finished game keeps its final state: a lost game has no Hearts left
	// (R9.6) and a finished game carries its end stamp (R10) - either way no
	// further twister may be served, and the result was already persisted
	// when the game ended (game-end handling of Step 5).
	if session.Hearts <= 0 || !session.EndedAt.IsZero() {
		writeError(w, http.StatusConflict, "the game has already ended")
		return
	}

	// The tongue-twister already being typed is returned again rather than
	// replaced: swapping it would let a player skip everything hard for
	// free, and it makes a page reload (repeated /next) show the same text.
	// Only when the pending twister has vanished from the catalogue does
	// selection run and hand out a fresh one.
	if session.CurrentTwisterID != 0 {
		pending, err := s.content.TwisterByID(ctx, session.CurrentTwisterID)
		switch {
		case err == nil:
			writeJSON(w, http.StatusOK, nextTwisterResponse{
				TongueTwisterID: pending.ID,
				Text:            pending.Text,
				Difficulty:      pending.Difficulty,
				Hearts:          session.Hearts,
				Score:           session.Score,
			})
			return
		case errors.Is(err, store.ErrTwisterNotFound):
			// Deleted mid-game: fall through to selection for a fresh one.
		default:
			// err carries only dbFailure's sanitized text (R21.4).
			s.log.Error("pending tongue-twister could not be loaded", "error", err.Error())
			writeError(w, http.StatusInternalServerError, "the pending tongue-twister could not be loaded")
			return
		}
	}

	twister, err := s.content.NextTwister(ctx, session.Difficulty, session.ShownIDs)
	switch {
	case errors.Is(err, store.ErrAllShown):
		// R6.3's notification, and - with Hearts left - the win of decision
		// 10: this request performs decision 7's deferred teardown, writing
		// the Game_Record, its attempt details and the leaderboard entry
		// (R10-R12) before answering. A repeat call is stopped by the
		// end-stamp guard above; a request racing across the finish line is
		// stopped inside endGame by MarkEnded, so the record is written
		// exactly once.
		s.endGame(ctx, sessionID)
		writeJSON(w, http.StatusOK, completedResponse{
			Completed: true,
			Message:   "all tongue-twisters for the current difficulty have been completed",
			GameOver:  true,
			Hearts:    session.Hearts,
			Score:     session.Score,
		})
		return
	case errors.Is(err, store.ErrNoContent):
		// R4.6: nothing active exists for this difficulty.
		writeError(w, http.StatusNotFound, "no content is available for the selected difficulty")
		return
	case err != nil:
		// err carries only dbFailure's sanitized text (R21.4).
		s.log.Error("next tongue-twister could not be selected", "error", err.Error())
		writeError(w, http.StatusInternalServerError, "the next tongue-twister could not be selected")
		return
	}

	// R6.4: record the ID before the twister leaves the Server, so the very
	// next selection already excludes it. The serve time starts R11's clock
	// for the attempt this response asks for.
	if _, err := s.sessions.AddShownTwister(ctx, sessionID, twister.ID, time.Now().UTC()); err != nil {
		switch {
		case errors.Is(err, cache.ErrSessionNotFound):
			writeError(w, http.StatusNotFound, "session not found or has expired")
		case errors.Is(err, cache.ErrUnavailable):
			// R6.6: state could not be updated - report it and withhold the
			// tongue-twister entirely.
			s.log.Warn("game session could not be updated", "error", err.Error())
			writeError(w, http.StatusServiceUnavailable, "the session state could not be updated")
		default:
			s.log.Error("game session could not be updated", "error", err.Error())
			writeError(w, http.StatusInternalServerError, "the game session could not be updated")
		}
		return
	}

	writeJSON(w, http.StatusOK, nextTwisterResponse{
		TongueTwisterID: twister.ID,
		Text:            twister.Text,
		Difficulty:      twister.Difficulty,
		Hearts:          session.Hearts,
		Score:           session.Score,
	})
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

// ---------------------------------------------------------------------------
// Typing attempts, scoring and hearts (Requirements 7-9)
// ---------------------------------------------------------------------------

// attemptRequest is the POST /api/games/{id}/attempts body: the text the
// player typed for the tongue-twister the session is currently typing. The
// field is a pointer so an absent/null value (R5.6 style) can be told apart
// from an explicitly empty one.
type attemptRequest struct {
	Text *string `json:"text"`
}

// attemptResponse is the result of grading one attempt. Hearts are mandatory
// (R9.5); Score and Game_over ride along so the HUD reacts to a finished game
// without an extra read.
type attemptResponse struct {
	SessionID       string `json:"session_id"`
	TongueTwisterID int64  `json:"tongue_twister_id"`
	Correct         bool   `json:"correct"`
	Hearts          int    `json:"hearts"`
	Score           int    `json:"score"`
	GameOver        bool   `json:"game_over"`
}

// scoreFor is the points awarded for one correct attempt at the given
// difficulty (R8). Difficulty is the only signal the Game_Session carries, so
// harder content is worth more; a speed bonus can be layered on later if the
// requirements ask for one. An unknown difficulty (impossible - sessions are
// validated at creation, R3.4) scores like easy.
func scoreFor(difficulty string) int {
	switch difficulty {
	case "medium":
		return 20
	case "hard":
		return 30
	default:
		return 10
	}
}

// normalizeForComparison makes two texts comparable for grading: surrounding
// whitespace is irrelevant, internal whitespace runs count as one separator,
// and letter case does not matter. Everything else - every character, every
// punctuation mark - must match exactly.
func normalizeForComparison(text string) string {
	return strings.ToLower(strings.Join(strings.Fields(text), " "))
}

// handleAttempt grades one typing attempt (R7), updates Score (R8) and Hearts
// (R9) and reports the result:
//
//	POST /api/games/{id}/attempts  {"text": "..."}
//	  -> 200 with correct, hearts, score and game_over; when this attempt
//	     takes the last Heart, the same request persists the finished game -
//	     Game_Record, attempt details, leaderboard (R10-R12) - so the result
//	     exists before the client is told the game is over
//	  -> 422 when the text is missing, empty or whitespace-only
//	  -> 400 for a malformed body
//	  -> 404 for an unknown or expired Session_ID
//	  -> 409 when the game has already ended (0 Hearts or completed already),
//	     when no twister is awaiting an attempt (call /next first), when that
//	     twister vanished from the catalogue, or when a concurrent request
//	     graded it first
//	  -> 503 when the Cache cannot be read or written
func (s *Server) handleAttempt(w http.ResponseWriter, r *http.Request) {
	var req attemptRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxBodyBytes)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if problem := validateAttemptRequest(&req); problem != "" {
		writeError(w, http.StatusUnprocessableEntity, problem)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	sessionID := r.PathValue("id")
	session, err := s.sessions.Get(ctx, sessionID)
	switch {
	case errors.Is(err, cache.ErrSessionNotFound):
		writeError(w, http.StatusNotFound, "session not found or has expired")
		return
	case errors.Is(err, cache.ErrUnavailable):
		s.log.Warn("game session could not be read", "error", err.Error())
		writeError(w, http.StatusServiceUnavailable, "the session state could not be retrieved")
		return
	case err != nil:
		s.log.Error("game session could not be read", "error", err.Error())
		writeError(w, http.StatusInternalServerError, "the game session could not be read")
		return
	}

	// R9 defines the game as "keep 3 hearts alive": once the last Heart is
	// gone the game is over (R9.6) and nothing further can be graded. A
	// finished game - lost or completed - carries its end stamp (R10) and is
	// equally final; its result was persisted when it ended.
	if session.Hearts <= 0 || !session.EndedAt.IsZero() {
		writeError(w, http.StatusConflict, "the game has already ended")
		return
	}
	// Grading needs a pending twister: without one there is nothing to
	// compare against, and accepting the attempt would corrupt Score/Hearts.
	if session.CurrentTwisterID == 0 {
		writeError(w, http.StatusConflict, "no tongue-twister is awaiting an attempt")
		return
	}

	twister, err := s.content.TwisterByID(ctx, session.CurrentTwisterID)
	switch {
	case errors.Is(err, store.ErrTwisterNotFound):
		// The twister was deactivated/deleted after being served; /next can
		// move the session past it, so the client is told to do exactly that.
		writeError(w, http.StatusConflict, "the tongue-twister awaiting the attempt is no longer available")
		return
	case err != nil:
		// err carries only dbFailure's sanitized text (R21.4).
		s.log.Error("tongue-twister for the pending attempt could not be loaded", "error", err.Error())
		writeError(w, http.StatusInternalServerError, "the attempt could not be graded")
		return
	}

	correct := normalizeForComparison(*req.Text) == normalizeForComparison(twister.Text)
	points := 0
	if correct {
		points = scoreFor(session.Difficulty)
	}

	updated, err := s.sessions.RecordAttempt(ctx, sessionID, correct, points, time.Now().UTC())
	switch {
	case errors.Is(err, cache.ErrNoPendingAttempt):
		// A concurrent request graded the same twister between our read and
		// our write - only one of them may count.
		writeError(w, http.StatusConflict, "no tongue-twister is awaiting an attempt")
	case errors.Is(err, cache.ErrSessionNotFound):
		writeError(w, http.StatusNotFound, "session not found or has expired")
	case errors.Is(err, cache.ErrUnavailable):
		s.log.Warn("game session could not be updated", "error", err.Error())
		writeError(w, http.StatusServiceUnavailable, "the session state could not be updated")
	default:
		if err != nil {
			s.log.Error("game session could not be updated", "error", err.Error())
			writeError(w, http.StatusInternalServerError, "the game session could not be updated")
			return
		}
		// R21.5: Session_ID and the outcome only - never the Nickname, and
		// never the player-typed text either.
		s.log.Info("attempt graded",
			"session_id", updated.SessionID,
			"tongue_twister_id", twister.ID,
			"correct", correct,
			"hearts", updated.Hearts,
			"score", updated.Score)

		// The last Heart is gone: this attempt is the loss of decision 10.
		// Persist the finished game (R10-R12) before the response reports
		// game_over, so the result can never be stranded behind the "the
		// game has already ended" 409 that follows.
		if updated.Hearts == 0 {
			s.endGame(ctx, sessionID)
		}

		writeJSON(w, http.StatusOK, attemptResponse{
			SessionID:       updated.SessionID,
			TongueTwisterID: twister.ID,
			Correct:         correct,
			Hearts:          updated.Hearts,
			Score:           updated.Score,
			GameOver:        updated.Hearts == 0,
		})
	}
}

// validateAttemptRequest returns one client-safe message listing every problem
// with the request, or "" when it is valid:
//
//   - an absent/null field -> "missing required field(s): text" (R5.6 style)
//   - empty/whitespace     -> "text is required"
//
// No length limit is enforced: a body over 1 GiB never reaches here (the
// reader is capped at maxBodyBytes), and text longer than any catalogue row
// simply grades as incorrect - which is exactly the right answer for a typo
// run wild.
func validateAttemptRequest(req *attemptRequest) string {
	if req.Text == nil {
		return "missing required field(s): text"
	}
	if strings.TrimSpace(*req.Text) == "" {
		return "text is required"
	}
	return ""
}

// ---------------------------------------------------------------------------
// Game end: Game_Record, attempt details, leaderboard (Requirements 10-12)
// ---------------------------------------------------------------------------

// endGame persists a finished game exactly once (R10-R12), in three steps:
//
//  1. the Game_Session is stamped with EndedAt in the Cache - the flag every
//     game-end guard checks (409 "the game has already ended"). The stamp is
//     write-once: a concurrent duplicate (an attempt and a /next racing over
//     the finish line) gets cache.ErrAlreadyEnded and stands down, so at most
//     one Game_Record per game is ever inserted;
//  2. the Game_Record (R10) and its Attempt_Details (R11) are inserted into
//     PostgreSQL in one transaction, built from the attempts the session
//     collected while the game ran;
//  3. the final Score is folded into the difficulty's Redis leaderboard
//     (R12), keeping only the nickname's highest score.
//
// The session's end state is authoritative for the API no matter what
// happens below: guards refuse play from step 1 on, so steps 2 and 3 are
// best effort. Their failures are logged for the operator - session ID and
// sanitized cause only, never the nickname (R21.5 / R21.4) - while the
// player still receives the game-over they earned. On the win path a failed
// step 1 even heals itself: the session stays un-ended, so the next /next
// finds the exhausted pool again and retries.
func (s *Server) endGame(ctx context.Context, sessionID string) {
	ended, err := s.sessions.MarkEnded(ctx, sessionID, time.Now().UTC())
	if errors.Is(err, cache.ErrAlreadyEnded) {
		return // another request is already persisting this game
	}
	if err != nil {
		// err is the cache sentinel text only (R21.4); the Nickname stays
		// out of the line (R21.5).
		s.log.Error("game session could not be marked ended", "session_id", sessionID, "error", err.Error())
		return
	}

	// R21.5: Session_ID, Difficulty and the outcome - never the Nickname.
	s.log.Info("game ended",
		"session_id", ended.SessionID,
		"difficulty", ended.Difficulty,
		"score", ended.Score,
		"hearts", ended.Hearts)

	result := store.GameResult{
		Nickname:   ended.Nickname,
		FinalScore: ended.Score,
		Difficulty: ended.Difficulty,
		StartedAt:  ended.CreatedAt,
		EndedAt:    ended.EndedAt,
		Attempts:   make([]store.AttemptResult, 0, len(ended.Attempts)),
	}
	for _, attempt := range ended.Attempts {
		result.Attempts = append(result.Attempts, store.AttemptResult{
			TwisterID:   attempt.TwisterID,
			TimeTakenMS: attempt.TimeTakenMS,
			Success:     attempt.Correct,
		})
	}
	if err := s.results.SaveGameResult(ctx, result); err != nil {
		// err carries only dbFailure's sanitized text (R21.4).
		s.log.Error("game result could not be persisted", "session_id", ended.SessionID, "error", err.Error())
	}
	if err := s.sessions.SaveLeaderboardScore(ctx, ended.Difficulty, ended.Nickname, ended.Score); err != nil {
		s.log.Error("leaderboard could not be updated", "session_id", ended.SessionID, "error", err.Error())
	}
}
