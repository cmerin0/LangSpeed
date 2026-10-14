package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"langspeed/internal/cache"
	"langspeed/internal/config"
	"langspeed/internal/logging"
	"langspeed/internal/store"
)

// testServer bundles a Server, its captured log output and the miniredis it
// talks to.
type testServer struct {
	server *Server
	logs   *bytes.Buffer
	mr     *miniredis.Miniredis
}

// newTestServer builds a Server backed by an in-process Redis. The default
// fake selector echoes the requested difficulty; /next tests override
// ts.server.content when they need a different outcome.
func newTestServer(t *testing.T) *testServer {
	t.Helper()
	mr := miniredis.RunT(t)
	logs := &bytes.Buffer{}
	server := New(
		config.Config{ServerPort: "0", StaticDir: t.TempDir()},
		logging.New(logs),
		cache.New(mr.Addr()),
		&fakeContent{},
	)
	return &testServer{server: server, logs: logs, mr: mr}
}

// newDeadCacheServer builds a Server whose Cache is unreachable (R3.6).
func newDeadCacheServer(t *testing.T) *testServer {
	t.Helper()
	logs := &bytes.Buffer{}
	dead := cache.NewWithClient(redis.NewClient(&redis.Options{
		Addr:         "127.0.0.1:1",
		DialTimeout:  100 * time.Millisecond,
		ReadTimeout:  100 * time.Millisecond,
		WriteTimeout: 100 * time.Millisecond,
	}))
	server := New(
		config.Config{ServerPort: "0", StaticDir: t.TempDir()},
		logging.New(logs),
		dead,
		&fakeContent{},
	)
	return &testServer{server: server, logs: logs}
}

// fakeContent substitutes for the PostgreSQL-backed selector so the HTTP
// surface can be tested without a database. It records what the handler asked
// for, which lets tests assert the difficulty and already-shown list passed
// down to selection (R4.5, R6.1), and which ID an attempt was graded against
// (R7).
type fakeContent struct {
	twister *store.Twister // returned when err is nil; nil echoes the difficulty
	err     error          // error to return instead of a twister
	onCall  func()         // hook run before answering (e.g. kill the Cache)

	// byID is the catalogue lookup used by attempts (R7) and by /next for a
	// pending twister. Texts default to the same sentence as the selector.
	byID    map[int64]*store.Twister
	byIDErr error

	difficulty   string  // difficulty of the last selection call
	shown        []int64 // already-shown list of the last selection call
	lastGradedID int64   // ID of the last TwisterByID call
	selections   int
	lookups      int
}

func (f *fakeContent) NextTwister(_ context.Context, difficulty string, shown []int64) (*store.Twister, error) {
	f.selections++
	f.difficulty = difficulty
	f.shown = append([]int64(nil), shown...)
	if f.onCall != nil {
		f.onCall()
	}
	if f.err != nil {
		return nil, f.err
	}
	if f.twister != nil {
		return f.twister, nil
	}
	return &store.Twister{
		ID:         42,
		Text:       "She sells seashells by the seashore.",
		Difficulty: difficulty,
	}, nil
}

func (f *fakeContent) TwisterByID(_ context.Context, id int64) (*store.Twister, error) {
	f.lookups++
	f.lastGradedID = id
	if f.onCall != nil {
		f.onCall()
	}
	if f.byIDErr != nil {
		return nil, f.byIDErr
	}
	if twister, ok := f.byID[id]; ok {
		return twister, nil
	}
	return &store.Twister{
		ID:         id,
		Text:       "She sells seashells by the seashore.",
		Difficulty: "easy",
	}, nil
}

// request performs a request against the full handler stack (including the
// Request_Log middleware) and returns the recorder.
func (ts *testServer) request(method, target, body string) *httptest.ResponseRecorder {
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, reader)
	rec := httptest.NewRecorder()
	ts.server.Handler().ServeHTTP(rec, req)
	return rec
}

// errorBody extracts the {"error": ...} payload.
func errorBody(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var payload struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response is not JSON: %v (%s)", err, rec.Body.String())
	}
	if payload.Error == "" {
		t.Fatalf("response has no error message: %s", rec.Body.String())
	}
	return payload.Error
}

// TestStartGameCreatesSession covers R3.5, R5.1, R5.5: a valid request
// returns 201 with a Session_ID and a fully initialized Game_Session.
func TestStartGameCreatesSession(t *testing.T) {
	ts := newTestServer(t)

	rec := ts.request("POST", "/api/games", `{"nickname":"  carlo  ","difficulty":"medium"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (%s)", rec.Code, rec.Body.String())
	}

	var state gameStateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &state); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if state.SessionID == "" {
		t.Fatal("response does not contain a Session_ID")
	}
	if state.Nickname != "carlo" {
		t.Errorf("Nickname = %q, want trimmed %q", state.Nickname, "carlo")
	}
	if state.Difficulty != "medium" {
		t.Errorf("Difficulty = %q, want medium", state.Difficulty)
	}
	if state.Hearts != 3 {
		t.Errorf("Hearts = %d, want 3", state.Hearts)
	}
	if state.Score != 0 {
		t.Errorf("Score = %d, want 0", state.Score)
	}
	if !ts.mr.Exists("game_session:" + state.SessionID) {
		t.Error("Game_Session was not stored in the Cache")
	}
}

// TestStartGameValidation covers R3.2, R3.3, R3.4 and R5.6: every invalid
// payload yields 422 with a message identifying the problem, and R21.5: the
// session-created log never contains the nickname.
func TestStartGameValidation(t *testing.T) {
	longNickname := strings.Repeat("a", 33)

	cases := []struct {
		name    string
		body    string
		wantSub string
	}{
		{"missing both fields", `{}`, "missing required field(s): nickname, difficulty"},
		{"missing nickname", `{"difficulty":"easy"}`, "missing required field(s): nickname"},
		{"missing difficulty", `{"nickname":"carlo"}`, "missing required field(s): difficulty"},
		{"null nickname", `{"nickname":null,"difficulty":"easy"}`, "missing required field(s): nickname"},
		{"empty nickname", `{"nickname":"","difficulty":"easy"}`, "nickname is required"},
		{"whitespace nickname", `{"nickname":"   ","difficulty":"easy"}`, "nickname is required"},
		{"nickname too long", `{"nickname":"` + longNickname + `","difficulty":"easy"}`, "at most 32 characters"},
		{"bad difficulty", `{"nickname":"carlo","difficulty":"insane"}`, "difficulty must be one of: easy, medium, hard"},
		{
			"multiple problems at once",
			`{"nickname":"","difficulty":"insane"}`,
			"nickname is required; difficulty must be one of: easy, medium, hard",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := newTestServer(t)
			rec := ts.request("POST", "/api/games", tc.body)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422 (%s)", rec.Code, rec.Body.String())
			}
			message := errorBody(t, rec)
			if !strings.Contains(message, tc.wantSub) {
				t.Errorf("error = %q, want it to contain %q", message, tc.wantSub)
			}
			if len(ts.mr.Keys()) != 0 {
				t.Errorf("invalid request created %d Cache entries, want 0", len(ts.mr.Keys()))
			}
		})
	}
}

// TestStartGameMalformedJSON: a syntactically broken body is a 400.
func TestStartGameMalformedJSON(t *testing.T) {
	ts := newTestServer(t)
	rec := ts.request("POST", "/api/games", `{"nickname": "carlo"`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", rec.Code, rec.Body.String())
	}
	if msg := errorBody(t, rec); !strings.Contains(msg, "invalid JSON") {
		t.Errorf("error = %q, want invalid JSON message", msg)
	}
}

// TestStartGameCacheUnavailable covers R3.6: 503 with a message saying the
// service is temporarily unavailable.
func TestStartGameCacheUnavailable(t *testing.T) {
	ts := newDeadCacheServer(t)
	rec := ts.request("POST", "/api/games", `{"nickname":"carlo","difficulty":"easy"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (%s)", rec.Code, rec.Body.String())
	}
	if msg := errorBody(t, rec); !strings.Contains(msg, "temporarily unavailable") {
		t.Errorf("error = %q, want temporary-unavailability message", msg)
	}
}

// TestGetSessionReturnsState covers R5.5's read path and R5.3's 404 for an
// unknown Session_ID.
func TestGetSessionReturnsState(t *testing.T) {
	ts := newTestServer(t)

	created := ts.request("POST", "/api/games", `{"nickname":"carlo","difficulty":"hard"}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", created.Code)
	}
	var state gameStateResponse
	if err := json.Unmarshal(created.Body.Bytes(), &state); err != nil {
		t.Fatalf("create response is not JSON: %v", err)
	}

	rec := ts.request("GET", "/api/games/"+state.SessionID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var got gameStateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("get response is not JSON: %v", err)
	}
	if got != state {
		t.Errorf("GET state = %+v, want %+v", got, state)
	}
}

func TestGetSessionUnknownID(t *testing.T) {
	ts := newTestServer(t)
	rec := ts.request("GET", "/api/games/00000000-0000-0000-0000-000000000000", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (%s)", rec.Code, rec.Body.String())
	}
	if msg := errorBody(t, rec); !strings.Contains(msg, "not found or has expired") {
		t.Errorf("error = %q, want not-found message", msg)
	}
}

// TestGetSessionCacheUnavailable: reads fail with 503 as well.
func TestGetSessionCacheUnavailable(t *testing.T) {
	ts := newDeadCacheServer(t)
	rec := ts.request("GET", "/api/games/anything", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (%s)", rec.Code, rec.Body.String())
	}
}

// TestSessionCreatedLogOmitsNickname covers R21.5: the INFO log carries the
// Session_ID and Difficulty but never the player-chosen Nickname.
func TestSessionCreatedLogOmitsNickname(t *testing.T) {
	ts := newTestServer(t)

	const nickname = "super-secret-player"
	rec := ts.request("POST", "/api/games", `{"nickname":"`+nickname+`","difficulty":"hard"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", rec.Code)
	}

	logs := ts.logs.String()
	if !strings.Contains(logs, `"msg":"game session created"`) {
		t.Errorf("missing session-created log line:\n%s", logs)
	}
	if !strings.Contains(logs, `"difficulty":"hard"`) {
		t.Errorf("session-created log line does not carry the difficulty:\n%s", logs)
	}
	if strings.Contains(logs, nickname) {
		t.Errorf("nickname leaked into the logs:\n%s", logs)
	}
}

// createSession starts a game and returns its Session_ID. The nickname is a
// deliberate canary: it must never appear in a log line (R21.5) and is
// chosen so no substring of it can occur in file paths by accident.
func createSession(t *testing.T, ts *testServer, difficulty string) string {
	t.Helper()
	rec := ts.request("POST", "/api/games", `{"nickname":"canary-player","difficulty":"`+difficulty+`"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201 (%s)", rec.Code, rec.Body.String())
	}
	var state gameStateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &state); err != nil {
		t.Fatalf("create response is not JSON: %v", err)
	}
	return state.SessionID
}

// TestNextTwisterReturnsAndMarksShown covers the happy path of R6: the
// session's difficulty is passed to selection (R4.5), the twister comes back
// with the R9.5-mandated Heart count, the ID is recorded before it is
// returned (R6.4) so the following call sees it as shown (R6.1), and no
// nickname reaches any log line (R21.5).
func TestNextTwisterReturnsAndMarksShown(t *testing.T) {
	ts := newTestServer(t)
	fake := ts.server.content.(*fakeContent)
	id := createSession(t, ts, "medium")

	rec := ts.request("GET", "/api/games/"+id+"/next", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("next status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var got nextTwisterResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("next response is not JSON: %v", err)
	}
	if got.TongueTwisterID == 0 {
		t.Error("response does not contain a tongue_twister_id")
	}
	if got.Difficulty != "medium" {
		t.Errorf("Difficulty = %q, want the session's difficulty medium", got.Difficulty)
	}
	if got.Hearts != 3 {
		t.Errorf("Hearts = %d, want the current count 3 (R9.5)", got.Hearts)
	}
	if got.Score != 0 {
		t.Errorf("Score = %d, want 0", got.Score)
	}
	if fake.difficulty != "medium" {
		t.Errorf("selection difficulty = %q, want session difficulty medium (R4.5)", fake.difficulty)
	}
	if len(fake.shown) != 0 {
		t.Errorf("first selection shown list = %v, want empty (R5.1)", fake.shown)
	}

	// While the first twister is still awaiting its attempt, /next returns
	// the very same one again instead of selecting - skipping would
	// otherwise be free, and a page reload must not lose the text.
	rec = ts.request("GET", "/api/games/"+id+"/next", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("repeat next status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var again nextTwisterResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &again); err != nil {
		t.Fatalf("repeat next response is not JSON: %v", err)
	}
	if again.TongueTwisterID != got.TongueTwisterID || again.Text != got.Text {
		t.Errorf("repeat next = %+v, want the pending %+v", again, got)
	}
	if fake.selections != 1 {
		t.Errorf("selections = %d, want 1 (repeat /next must not re-select)", fake.selections)
	}

	// Grade the pending attempt, then /next must run a fresh selection that
	// already carries the first ID (R6.4 -> R6.1).
	if rec := ts.request("POST", "/api/games/"+id+"/attempts", `{"text":"`+got.Text+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("attempt status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	ts.request("GET", "/api/games/"+id+"/next", "")
	if len(fake.shown) != 1 || fake.shown[0] != got.TongueTwisterID {
		t.Errorf("selection after the attempt shown = %v, want [%d]", fake.shown, got.TongueTwisterID)
	}

	if strings.Contains(ts.logs.String(), "canary-player") {
		t.Errorf("nickname leaked into the logs:\n%s", ts.logs.String())
	}
}

// TestNextTwisterCompletedNotifiesPlayer covers R6.3's notification: pool
// exhaustion is a 200 with a message, not an error.
func TestNextTwisterCompletedNotifiesPlayer(t *testing.T) {
	ts := newTestServer(t)
	ts.server.content = &fakeContent{err: store.ErrAllShown}
	id := createSession(t, ts, "easy")

	rec := ts.request("GET", "/api/games/"+id+"/next", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("next status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var payload struct {
		Completed bool   `json:"completed"`
		Message   string `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if !payload.Completed {
		t.Errorf("completed = false, want true: %s", rec.Body.String())
	}
	if !strings.Contains(payload.Message, "all tongue-twisters") || !strings.Contains(payload.Message, "completed") {
		t.Errorf("message = %q, want the R6.3 completion notification", payload.Message)
	}
}

// TestNextTwisterNoContent covers R4.6: an empty catalogue for the difficulty
// is a 404 saying so.
func TestNextTwisterNoContent(t *testing.T) {
	ts := newTestServer(t)
	ts.server.content = &fakeContent{err: store.ErrNoContent}
	id := createSession(t, ts, "hard")

	rec := ts.request("GET", "/api/games/"+id+"/next", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("next status = %d, want 404 (%s)", rec.Code, rec.Body.String())
	}
	if msg := errorBody(t, rec); !strings.Contains(msg, "no content is available for the selected difficulty") {
		t.Errorf("error = %q, want the R4.6 no-content message", msg)
	}
}

// TestNextTwisterUnknownSession: an unknown or expired Session_ID cannot have
// an already-shown list, so selection is never consulted (R5.3).
func TestNextTwisterUnknownSession(t *testing.T) {
	ts := newTestServer(t)
	fake := ts.server.content.(*fakeContent)

	rec := ts.request("GET", "/api/games/00000000-0000-0000-0000-000000000000/next", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("next status = %d, want 404 (%s)", rec.Code, rec.Body.String())
	}
	if msg := errorBody(t, rec); !strings.Contains(msg, "not found or has expired") {
		t.Errorf("error = %q, want not-found message", msg)
	}
	if fake.selections != 0 {
		t.Errorf("selector was called %d times for an unknown session, want 0", fake.selections)
	}
}

// TestNextTwisterCacheUnavailableOnRead covers R6.5: when the already-shown
// list cannot be read, the response says the session state could not be
// retrieved - and state is untouched because nothing was written.
func TestNextTwisterCacheUnavailableOnRead(t *testing.T) {
	ts := newDeadCacheServer(t)

	rec := ts.request("GET", "/api/games/anything/next", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("next status = %d, want 503 (%s)", rec.Code, rec.Body.String())
	}
	if msg := errorBody(t, rec); !strings.Contains(msg, "session state could not be retrieved") {
		t.Errorf("error = %q, want the R6.5 read-failure message", msg)
	}
}

// TestNextTwisterCacheUnavailableOnWrite covers R6.6: if the shown-list
// update fails after selection, the answer is 503 "session state could not
// be updated" and carries no tongue-twister at all. The Cache dies between
// the handler's read and its write to force exactly that path.
func TestNextTwisterCacheUnavailableOnWrite(t *testing.T) {
	ts := newTestServer(t)
	fake := ts.server.content.(*fakeContent)
	fake.onCall = func() { ts.mr.Close() } // Cache unreachable from here on
	id := createSession(t, ts, "easy")

	rec := ts.request("GET", "/api/games/"+id+"/next", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("next status = %d, want 503 (%s)", rec.Code, rec.Body.String())
	}
	if msg := errorBody(t, rec); !strings.Contains(msg, "session state could not be updated") {
		t.Errorf("error = %q, want the R6.6 write-failure message", msg)
	}
	if strings.Contains(rec.Body.String(), "seashells") {
		t.Errorf("R6.6 response leaked the tongue-twister: %s", rec.Body.String())
	}
}

// TestNextTwisterSelectionFailure: an unexpected database failure is a
// sanitized 500, never an internal message.
func TestNextTwisterSelectionFailure(t *testing.T) {
	ts := newTestServer(t)
	ts.server.content = &fakeContent{err: errors.New(`pq: relation "tongue_twisters" does not exist`)}
	id := createSession(t, ts, "easy")

	rec := ts.request("GET", "/api/games/"+id+"/next", "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("next status = %d, want 500 (%s)", rec.Code, rec.Body.String())
	}
	msg := errorBody(t, rec)
	if !strings.Contains(msg, "could not be selected") {
		t.Errorf("error = %q, want the selection-failure message", msg)
	}
	if strings.Contains(msg, "pq:") || strings.Contains(msg, "relation") {
		t.Errorf("error leaked database internals (R21.4): %q", msg)
	}
}

// ---------------------------------------------------------------------------
// Typing attempts, scoring and hearts (Requirements 7-9)
// ---------------------------------------------------------------------------

// startAndServe creates a game and fetches its first tongue-twister, the pair
// every attempt test starts from.
func startAndServe(t *testing.T, ts *testServer, difficulty string) (sessionID string, served nextTwisterResponse) {
	t.Helper()
	sessionID = createSession(t, ts, difficulty)
	rec := ts.request("GET", "/api/games/"+sessionID+"/next", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("next status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &served); err != nil {
		t.Fatalf("next response is not JSON: %v", err)
	}
	return sessionID, served
}

// TestAttemptCorrectScoresAndAdvances covers the happy path of R7/R8: the
// typed text is graded against the pending twister, the Score grows by the
// difficulty's points, Hearts stay untouched, the pending twister is consumed
// and the R9.5-mandated Heart count rides in the response.
func TestAttemptCorrectScoresAndAdvances(t *testing.T) {
	ts := newTestServer(t)
	sessionID, served := startAndServe(t, ts, "medium")

	rec := ts.request("POST", "/api/games/"+sessionID+"/attempts",
		`{"text":"`+served.Text+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("attempt status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var got attemptResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("attempt response is not JSON: %v", err)
	}
	if !got.Correct {
		t.Errorf("correct = false, want true for the exact text")
	}
	if got.Hearts != 3 {
		t.Errorf("Hearts = %d, want 3 (R9.5)", got.Hearts)
	}
	if want := scoreFor("medium"); got.Score != want {
		t.Errorf("Score = %d, want %d (R8)", got.Score, want)
	}
	if got.GameOver {
		t.Error("game_over = true with 3 Hearts left")
	}
	if got.SessionID != sessionID {
		t.Errorf("SessionID = %q, want %q", got.SessionID, sessionID)
	}

	// The pending twister is consumed: the stored session no longer has one.
	stored, err := ts.server.sessions.Get(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("stored session could not be read: %v", err)
	}
	if stored.CurrentTwisterID != 0 {
		t.Errorf("CurrentTwisterID = %d, want 0 (consumed)", stored.CurrentTwisterID)
	}
	if stored.Score != got.Score {
		t.Errorf("stored Score = %d, want %d", stored.Score, got.Score)
	}
}

// TestAttemptWrongLosesHeart covers the R9 heart rule: a wrong attempt costs
// exactly one Heart, awards nothing, and consumes the twister so the player
// must fetch the next one.
func TestAttemptWrongLosesHeart(t *testing.T) {
	ts := newTestServer(t)
	sessionID, _ := startAndServe(t, ts, "easy")

	rec := ts.request("POST", "/api/games/"+sessionID+"/attempts",
		`{"text":"complete nonsense"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("attempt status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var got attemptResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("attempt response is not JSON: %v", err)
	}
	if got.Correct {
		t.Error("correct = true for wrong text")
	}
	if got.Hearts != 2 {
		t.Errorf("Hearts = %d, want 2 after one wrong attempt (R9)", got.Hearts)
	}
	if got.Score != 0 {
		t.Errorf("Score = %d, want 0 for a wrong attempt (R8)", got.Score)
	}
	if got.GameOver {
		t.Error("game_over = true with 2 Hearts left")
	}
}

// TestAttemptThreeWrongEndsGame covers the user-defined game-over rule: the
// remaining attempts are exactly the Hearts (3, 2 or 1 - R7.3); the third
// wrong attempt drops the count to 0, flags game_over, and both further
// attempts and /next are refused with 409 - a finished game keeps its state.
func TestAttemptThreeWrongEndsGame(t *testing.T) {
	ts := newTestServer(t)
	sessionID, _ := startAndServe(t, ts, "hard")

	for attempt := 1; attempt <= 3; attempt++ {
		// Each wrong attempt consumes the pending twister, so serve a new one.
		if attempt > 1 {
			rec := ts.request("GET", "/api/games/"+sessionID+"/next", "")
			if rec.Code != http.StatusOK {
				t.Fatalf("next %d status = %d, want 200 (%s)", attempt, rec.Code, rec.Body.String())
			}
		}
		rec := ts.request("POST", "/api/games/"+sessionID+"/attempts", `{"text":"wrong"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("attempt %d status = %d, want 200 (%s)", attempt, rec.Code, rec.Body.String())
		}
		var got attemptResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("attempt %d response is not JSON: %v", attempt, err)
		}
		wantHearts := 3 - attempt
		if got.Hearts != wantHearts {
			t.Errorf("attempt %d Hearts = %d, want %d", attempt, got.Hearts, wantHearts)
		}
		if got.GameOver != (wantHearts == 0) {
			t.Errorf("attempt %d game_over = %t, want %t", attempt, got.GameOver, wantHearts == 0)
		}
	}

	// A fourth attempt: the game is over, nothing more is graded (R7.3).
	rec := ts.request("POST", "/api/games/"+sessionID+"/attempts", `{"text":"anything"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("attempt after game over = %d, want 409 (%s)", rec.Code, rec.Body.String())
	}
	if msg := errorBody(t, rec); !strings.Contains(msg, "the game has already ended") {
		t.Errorf("error = %q, want the game-over message", msg)
	}

	// /next refuses too - the lost game's state is final (until R10 lands).
	rec = ts.request("GET", "/api/games/"+sessionID+"/next", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("next after game over = %d, want 409 (%s)", rec.Code, rec.Body.String())
	}
}

// TestAttemptGradingIsForgivingWhereItShouldBe: letter case and surrounding
// or doubled whitespace never decide a game - every other character does.
func TestAttemptGradingIsForgivingWhereItShouldBe(t *testing.T) {
	cases := []struct {
		name  string
		typed string
		want  bool
	}{
		{"exact", "She sells seashells by the seashore.", true},
		{"upper case", "SHE SELLS SEASHELLS BY THE SEASHORE.", true},
		{"mixed case", "she SELLS Seashells by the SEASHORE.", true},
		{"surrounding whitespace", "   She sells seashells by the seashore.  ", true},
		{"doubled inner spaces", "She  sells seashells by the seashore.", true},
		{"tabs and newlines", "She sells\tseashells\nby the seashore.", true},
		{"missing period", "She sells seashells by the seashore", false},
		{"one wrong letter", "She sells seashells by the seash0re.", false},
		{"extra word", "She sells red seashells by the seashore.", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := newTestServer(t)
			sessionID, _ := startAndServe(t, ts, "easy")

			body, err := json.Marshal(attemptRequest{Text: &tc.typed})
			if err != nil {
				t.Fatalf("request could not be encoded: %v", err)
			}
			rec := ts.request("POST", "/api/games/"+sessionID+"/attempts", string(body))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
			}
			var got attemptResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("response is not JSON: %v", err)
			}
			if got.Correct != tc.want {
				t.Errorf("correct = %t for %q, want %t", got.Correct, tc.typed, tc.want)
			}
		})
	}
}

// TestAttemptScoreGrowsWithDifficulty covers R8's difficulty weighting: hard
// content is worth more than easy content, correct only.
func TestAttemptScoreGrowsWithDifficulty(t *testing.T) {
	for _, difficulty := range []string{"easy", "medium", "hard"} {
		t.Run(difficulty, func(t *testing.T) {
			ts := newTestServer(t)
			sessionID, served := startAndServe(t, ts, difficulty)

			rec := ts.request("POST", "/api/games/"+sessionID+"/attempts",
				`{"text":"`+served.Text+`"}`)
			var got attemptResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("response is not JSON: %v", err)
			}
			if want := scoreFor(difficulty); got.Score != want {
				t.Errorf("%s Score = %d, want %d", difficulty, got.Score, want)
			}
			if scoreFor("easy") >= scoreFor("medium") || scoreFor("medium") >= scoreFor("hard") {
				t.Errorf("points must grow with difficulty: easy=%d medium=%d hard=%d",
					scoreFor("easy"), scoreFor("medium"), scoreFor("hard"))
			}
		})
	}
}

// TestAttemptCannotGradeTheSameTwisterTwice: consuming the pending twister is
// what makes "one attempt per twister" hold - a resubmission is a 409 and can
// neither re-score nor re-charge.
func TestAttemptCannotGradeTheSameTwisterTwice(t *testing.T) {
	ts := newTestServer(t)
	sessionID, served := startAndServe(t, ts, "hard")

	first := ts.request("POST", "/api/games/"+sessionID+"/attempts",
		`{"text":"`+served.Text+`"}`)
	if first.Code != http.StatusOK {
		t.Fatalf("first attempt = %d, want 200 (%s)", first.Code, first.Body.String())
	}

	second := ts.request("POST", "/api/games/"+sessionID+"/attempts",
		`{"text":"`+served.Text+`"}`)
	if second.Code != http.StatusConflict {
		t.Fatalf("second attempt = %d, want 409 (%s)", second.Code, second.Body.String())
	}
	if msg := errorBody(t, second); !strings.Contains(msg, "no tongue-twister is awaiting an attempt") {
		t.Errorf("error = %q, want the no-pending-attempt message", msg)
	}

	stored, err := ts.server.sessions.Get(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("stored session could not be read: %v", err)
	}
	if want := scoreFor("hard"); stored.Score != want {
		t.Errorf("stored Score = %d, want %d (the resubmission must not re-score)", stored.Score, want)
	}
	if stored.Hearts != 3 {
		t.Errorf("stored Hearts = %d, want 3", stored.Hearts)
	}
}

// TestAttemptWithoutPendingTwister: attempting before /next has ever served
// anything has no text to grade against - 409, not a guess.
func TestAttemptWithoutPendingTwister(t *testing.T) {
	ts := newTestServer(t)
	sessionID := createSession(t, ts, "easy")

	rec := ts.request("POST", "/api/games/"+sessionID+"/attempts", `{"text":"anything"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (%s)", rec.Code, rec.Body.String())
	}
	if msg := errorBody(t, rec); !strings.Contains(msg, "no tongue-twister is awaiting an attempt") {
		t.Errorf("error = %q, want the no-pending-attempt message", msg)
	}
}

// TestAttemptValidation: absent, empty and whitespace-only text are 422s
// naming the problem (R5.6-style), a broken body is a 400, and a rejected
// request never mutates the session.
func TestAttemptValidation(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantSub string
	}{
		{"missing text", `{}`, "missing required field(s): text"},
		{"null text", `{"text":null}`, "missing required field(s): text"},
		{"empty text", `{"text":""}`, "text is required"},
		{"whitespace text", `{"text":"   "}`, "text is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := newTestServer(t)
			sessionID, _ := startAndServe(t, ts, "easy")

			rec := ts.request("POST", "/api/games/"+sessionID+"/attempts", tc.body)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422 (%s)", rec.Code, rec.Body.String())
			}
			if msg := errorBody(t, rec); !strings.Contains(msg, tc.wantSub) {
				t.Errorf("error = %q, want it to contain %q", msg, tc.wantSub)
			}

			// Rejected before any write: the pending twister is still there.
			stored, err := ts.server.sessions.Get(context.Background(), sessionID)
			if err != nil {
				t.Fatalf("stored session could not be read: %v", err)
			}
			if stored.CurrentTwisterID == 0 {
				t.Error("rejected request consumed the pending twister")
			}
		})
	}

	t.Run("malformed JSON", func(t *testing.T) {
		ts := newTestServer(t)
		sessionID, _ := startAndServe(t, ts, "easy")
		rec := ts.request("POST", "/api/games/"+sessionID+"/attempts", `{"text": "oops"`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (%s)", rec.Code, rec.Body.String())
		}
	})
}

// TestAttemptUnknownSession: R5.3's 404 on the attempt path.
func TestAttemptUnknownSession(t *testing.T) {
	ts := newTestServer(t)
	rec := ts.request("POST", "/api/games/00000000-0000-0000-0000-000000000000/attempts",
		`{"text":"anything"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (%s)", rec.Code, rec.Body.String())
	}
	if msg := errorBody(t, rec); !strings.Contains(msg, "not found or has expired") {
		t.Errorf("error = %q, want not-found message", msg)
	}
}

// TestAttemptTwisterDeletedMidGame: a twister removed from the catalogue
// after being served cannot be graded - 409 pointing the client back to /next,
// which hands out a fresh one.
func TestAttemptTwisterDeletedMidGame(t *testing.T) {
	ts := newTestServer(t)
	fake := ts.server.content.(*fakeContent)
	sessionID, _ := startAndServe(t, ts, "easy")
	fake.byIDErr = store.ErrTwisterNotFound

	rec := ts.request("POST", "/api/games/"+sessionID+"/attempts", `{"text":"anything"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (%s)", rec.Code, rec.Body.String())
	}
	if msg := errorBody(t, rec); !strings.Contains(msg, "no longer available") {
		t.Errorf("error = %q, want the deleted-twister message", msg)
	}
}

// TestAttemptDatabaseFailureIsSanitized: an unexpected catalogue failure is a
// 500 whose message never leaks SQL (R21.4).
func TestAttemptDatabaseFailureIsSanitized(t *testing.T) {
	ts := newTestServer(t)
	fake := ts.server.content.(*fakeContent)
	sessionID, _ := startAndServe(t, ts, "easy")
	fake.byIDErr = errors.New(`pq: relation "tongue_twisters" does not exist`)

	rec := ts.request("POST", "/api/games/"+sessionID+"/attempts", `{"text":"anything"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (%s)", rec.Code, rec.Body.String())
	}
	msg := errorBody(t, rec)
	if !strings.Contains(msg, "could not be graded") {
		t.Errorf("error = %q, want the grading-failure message", msg)
	}
	if strings.Contains(msg, "pq:") || strings.Contains(msg, "relation") {
		t.Errorf("error leaked database internals (R21.4): %q", msg)
	}
}

// TestAttemptCacheUnavailable: a dead Cache answers 503 on read and write
// with distinct messages, and a failed write changes nothing.
func TestAttemptCacheUnavailable(t *testing.T) {
	t.Run("read", func(t *testing.T) {
		ts := newDeadCacheServer(t)
		rec := ts.request("POST", "/api/games/anything/attempts", `{"text":"x"}`)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503 (%s)", rec.Code, rec.Body.String())
		}
		if msg := errorBody(t, rec); !strings.Contains(msg, "session state could not be retrieved") {
			t.Errorf("error = %q, want the read-failure message", msg)
		}
	})

	t.Run("write", func(t *testing.T) {
		ts := newTestServer(t)
		fake := ts.server.content.(*fakeContent)
		sessionID, served := startAndServe(t, ts, "easy")
		// The Cache dies in the window between the handler's read and its
		// write: TwisterByID runs after the session was loaded.
		fake.onCall = func() { ts.mr.Close() }

		rec := ts.request("POST", "/api/games/"+sessionID+"/attempts",
			`{"text":"`+served.Text+`"}`)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503 (%s)", rec.Code, rec.Body.String())
		}
		if msg := errorBody(t, rec); !strings.Contains(msg, "session state could not be updated") {
			t.Errorf("error = %q, want the write-failure message", msg)
		}
	})
}

// TestAttemptLogOmitsNicknameAndTypedText covers R21.5 on the new endpoint:
// the graded log line carries the Session_ID and outcome, never the
// player-chosen Nickname nor the player-typed text.
func TestAttemptLogOmitsNicknameAndTypedText(t *testing.T) {
	ts := newTestServer(t)
	const typedText = "She sells seashells by the seashore."
	sessionID := createSession(t, ts, "easy")

	ts.request("GET", "/api/games/"+sessionID+"/next", "")
	rec := ts.request("POST", "/api/games/"+sessionID+"/attempts",
		`{"text":"`+typedText+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("attempt status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}

	logs := ts.logs.String()
	if !strings.Contains(logs, `"msg":"attempt graded"`) {
		t.Errorf("missing the graded-attempt log line:\n%s", logs)
	}
	if strings.Contains(logs, "canary-player") {
		t.Errorf("nickname leaked into the logs:\n%s", logs)
	}
	if strings.Contains(logs, "seashells") {
		t.Errorf("player-typed text leaked into the logs:\n%s", logs)
	}
}
