// Package store owns everything that talks to PostgreSQL: dependency health
// checks (R1.2), connection management, schema migrations (R2.1-R2.6) and the
// Seed_Script (R2.8-R2.10).
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/url"
	"time"

	"github.com/lib/pq" // PostgreSQL driver (also used by dbFailure below)

	"langspeed/internal/logging"
)

// TCPAddrFromURL extracts host:port from a postgres DSN so the caller can
// confirm reachability with a plain TCP connection (R1.2).
//
// Errors are deliberately generic: DATABASE_URL embeds credentials and must
// never be echoed into logs (R21.4).
func TCPAddrFromURL(rawURL string) (string, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") || parsed.Host == "" {
		return "", errors.New("value must be a postgres:// or postgresql:// URL including a host")
	}
	host := parsed.Hostname()
	if host == "" {
		return "", errors.New("value must include a host")
	}
	port := parsed.Port()
	if port == "" {
		port = "5432"
	}
	return net.JoinHostPort(host, port), nil
}

// WaitReady dials addr over TCP until it succeeds, giving up after `attempts`
// tries or `total`, whichever comes first (R1.2: "a successful TCP connection
// to each service within 30 seconds and up to 5 retry attempts").
// ctx cancellation aborts the wait immediately.
func WaitReady(ctx context.Context, log *logging.Logger, name, addr string, attempts int, total time.Duration) error {
	if attempts < 1 {
		attempts = 1
	}
	deadline := time.Now().Add(total)
	var lastErr error

	for attempt := 1; attempt <= attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("%s check cancelled: %w", name, err)
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		// Split the remaining time budget across the remaining attempts so
		// the whole loop is guaranteed to finish within `total`.
		budget := remaining / time.Duration(attempts-attempt+1)

		conn, err := net.DialTimeout("tcp", addr, budget)
		if err == nil {
			_ = conn.Close()
			log.Info("dependency is reachable", "dependency", name, "addr", addr, "attempt", attempt)
			return nil
		}
		lastErr = err
		log.Warn("waiting for dependency",
			"dependency", name, "addr", addr,
			"attempt", attempt, "max_attempts", attempts,
			"error", err.Error(),
		)

		if attempt < attempts {
			sleep := budget / 2
			if sleep > remaining {
				sleep = remaining
			}
			if sleep > 0 {
				select {
				case <-ctx.Done():
					return fmt.Errorf("%s check cancelled: %w", name, ctx.Err())
				case <-time.After(sleep):
				}
			}
		}
	}

	if lastErr == nil {
		lastErr = errors.New("deadline exceeded")
	}
	return fmt.Errorf("%s at %s was not reachable within %s after up to %d attempts: %w",
		name, addr, total, attempts, lastErr)
}

// Open creates the connection pool. It does not block: callers have already
// confirmed TCP reachability via WaitReady.
func Open(databaseURL string) (*sql.DB, error) {
	db, err := sql.Open("postgres", databaseURL)
	if err != nil {
		// sql.Open surfaces DSN parse errors - never echo the DSN itself.
		return nil, errors.New("DATABASE_URL was rejected by the postgres driver")
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)
	return db, nil
}

// dbFailure renders a database error safely for logs (R21.4).
//
// lib/pq never includes the SQL text, bound parameters or a stack trace in
// its error type - only the server-provided message and the SQLSTATE code -
// so this summary stays free of internals. Request-path logging must go one
// step further and use only client-safe messages.
func dbFailure(err error) string {
	if err == nil {
		return ""
	}
	var pqErr *pq.Error
	if errors.As(err, &pqErr) {
		return fmt.Sprintf("%s (SQLSTATE %s)", pqErr.Message, pqErr.Code)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "database operation timed out"
	}
	return "database operation failed"
}
