// Package httpserver assembles the LangSpeed HTTP API, serves the static
// frontend and implements the structured request logging of Requirement 21.
package httpserver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"langspeed/internal/cache"
	"langspeed/internal/config"
	"langspeed/internal/logging"
	"langspeed/internal/store"
)

// Server owns the HTTP surface of the LangSpeed backend. Endpoints for later
// requirements are registered from routes().
type Server struct {
	cfg      config.Config
	log      *logging.Logger
	sessions *cache.Store
	content  twisterSelector
	mux      *http.ServeMux
}

// twisterSelector hands out the next tongue-twister for a session (R4, R6).
// *store.Content implements it against PostgreSQL; tests substitute fakes so
// the HTTP surface stays testable without a database.
type twisterSelector interface {
	NextTwister(ctx context.Context, difficulty string, shown []int64) (*store.Twister, error)
}

// New builds the server and registers every route known at this stage.
func New(cfg config.Config, log *logging.Logger, sessions *cache.Store, content twisterSelector) *Server {
	s := &Server{cfg: cfg, log: log, sessions: sessions, content: content, mux: http.NewServeMux()}
	s.routes()
	s.registerStatic()
	return s
}

// Handler wraps the router with the Request_Log middleware (R21.1).
func (s *Server) Handler() http.Handler { return s.withRequestLog(s.mux) }

// routes is the single registration point for API endpoints.
func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)

	// Game session creation (R3, R5).
	s.mux.HandleFunc("POST /api/games", s.handleStartGame)
	s.mux.HandleFunc("GET /api/games/{id}", s.handleGetSession)

	// Tongue-twister selection (R4, R6). The more specific pattern wins over
	// the session read above.
	s.mux.HandleFunc("GET /api/games/{id}/next", s.handleNextTwister)
}

// handleHealthz answers liveness probes with a structured JSON body.
func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"status":  "ok",
		"service": "langspeed-server",
	})
}

// registerStatic serves the plain HTML/CSS/JS frontend. During early backend
// steps the directory may not exist yet, in which case "/" returns 404.
func (s *Server) registerStatic() {
	info, err := os.Stat(s.cfg.StaticDir)
	if err != nil || !info.IsDir() {
		s.log.Warn("static frontend directory not found, / will return 404", "dir", s.cfg.StaticDir)
		s.mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
			writeError(w, http.StatusNotFound, "not found")
		})
		return
	}
	s.log.Info("serving static frontend", "dir", s.cfg.StaticDir)
	s.mux.Handle("/", s.staticHandler(s.cfg.StaticDir))
}

// staticHandler serves files from root, but turns missing files into the
// standard JSON 404 so the request log carries a brief reason (R21.3) and the
// client sees a consistent error body (R21.10).
func (s *Server) staticHandler(root string) http.Handler {
	fileServer := http.FileServer(http.Dir(root))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// path.Clean removes any ".." segments, so the join below can never
		// escape root.
		cleaned := path.Clean(r.URL.Path)
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(cleaned))); err != nil {
			writeError(w, http.StatusNotFound, "static asset not found")
			return
		}
		fileServer.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------------------
// Request logging middleware (Requirement 21)
// ---------------------------------------------------------------------------

// withRequestLog emits exactly one structured Request_Log line per processed
// HTTP request, carrying method, path, status code, latency in milliseconds
// and a unique request ID (R21.1).
//
// Severity mapping:
//   - 5xx             -> ERROR, including a sanitized reason when the handler
//     attached one (R21.2);
//   - 401, 403, 404, 409, 503 -> WARN with a brief reason (R21.3, which
//     explicitly lists 503 even though it is a 5xx code);
//   - everything else (including 422 validation errors) -> INFO.
//
// Only client-safe text is ever logged: handlers pass sanitized messages via
// writeError, so raw database errors, SQL strings and stack traces never reach
// a log line (R21.4).
func (s *Server) withRequestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		requestID := strings.TrimSpace(r.Header.Get("X-Request-ID"))
		if requestID == "" {
			requestID = newRequestID()
		}
		w.Header().Set("X-Request-ID", requestID)

		rec := &recorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		latencyMS := float64(time.Since(start).Microseconds()) / 1000
		fields := []any{
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"latency_ms", latencyMS,
			"request_id", requestID,
		}
		if rec.reason != "" {
			fields = append(fields, "reason", rec.reason)
		}
		s.log.Log(severityFor(rec.status), "http request", fields...)
	})
}

// severityFor maps an HTTP status code to its required log level (R21.2, R21.3).
func severityFor(status int) logging.Level {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound,
		http.StatusConflict, http.StatusServiceUnavailable:
		return logging.Warn // R21.3 (explicitly includes 503)
	}
	if status >= 500 {
		return logging.Error // R21.2
	}
	return logging.Info
}

// recorder captures the response status and the sanitized failure reason so
// the request log line can include them.
type recorder struct {
	http.ResponseWriter
	status int
	reason string
	wrote  bool
}

func (rec *recorder) WriteHeader(status int) {
	if rec.wrote {
		return
	}
	rec.wrote = true
	rec.status = status
	rec.ResponseWriter.WriteHeader(status)
}

// ---------------------------------------------------------------------------
// Response helpers
// ---------------------------------------------------------------------------

// writeJSON writes a JSON payload with the given status code.
func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

// writeError writes a JSON error body and records `message` as the log reason.
// Messages MUST be client-safe summaries - never SQL, stack traces or other
// internals (R21.4). The body echoes the request ID for correlation (R21.2);
// it lives in the response headers, set by the logging middleware.
func writeError(w http.ResponseWriter, status int, message string) {
	if rec, ok := w.(*recorder); ok {
		rec.reason = message
	}
	writeJSON(w, status, map[string]string{
		"error":      message,
		"request_id": w.Header().Get("X-Request-ID"),
	})
}

// newRequestID returns a random 16-hex-char identifier for log correlation.
func newRequestID() string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "unknown"
	}
	return hex.EncodeToString(buf[:])
}
