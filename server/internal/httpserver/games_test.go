package httpserver

import (
	"bytes"
	"encoding/json"
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
)

// testServer bundles a Server, its captured log output and the miniredis it
// talks to.
type testServer struct {
	server *Server
	logs   *bytes.Buffer
	mr     *miniredis.Miniredis
}

// newTestServer builds a Server backed by an in-process Redis.
func newTestServer(t *testing.T) *testServer {
	t.Helper()
	mr := miniredis.RunT(t)
	logs := &bytes.Buffer{}
	server := New(
		config.Config{ServerPort: "0", StaticDir: t.TempDir()},
		logging.New(logs),
		cache.New(mr.Addr()),
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
	)
	return &testServer{server: server, logs: logs}
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
