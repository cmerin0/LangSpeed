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
// down to selection (R4.5, R6.1).
type fakeContent struct {
	twister *store.Twister   // returned when err is nil; nil echoes the difficulty
	err     error            // error to return instead of a twister
	onCall  func()           // hook run before answering (e.g. kill the Cache)

	difficulty string // difficulty of the last call
	shown      []int64 // already-shown list of the last call
	calls      int
}

func (f *fakeContent) NextTwister(_ context.Context, difficulty string, shown []int64) (*store.Twister, error) {
	f.calls++
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

	// The second request must already carry the first ID (R6.4 -> R6.1).
	ts.request("GET", "/api/games/"+id+"/next", "")
	if len(fake.shown) != 1 || fake.shown[0] != got.TongueTwisterID {
		t.Errorf("second selection shown = %v, want [%d]", fake.shown, got.TongueTwisterID)
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
	if fake.calls != 0 {
		t.Errorf("selector was called %d times for an unknown session, want 0", fake.calls)
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
