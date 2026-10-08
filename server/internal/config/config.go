// Package config loads and validates the server's runtime configuration.
//
// Requirement 1.3: all runtime configuration (database connection string,
// cache address, JWT secret, server port) is read from environment variables.
// Requirement 1.4: if a required variable is absent, the server logs the name
// of every missing variable and exits with a non-zero status code.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config is the fully validated runtime configuration.
type Config struct {
	ServerPort  string // SERVER_PORT  - HTTP listen port (R1.5: defaults to 3000)
	DatabaseURL string // DATABASE_URL - PostgreSQL connection string
	RedisAddr   string // REDIS_ADDR   - host:port of the Cache
	JWTSecret   string // JWT_SECRET   - signing secret for admin JWTs (R13.5)
	StaticDir   string // STATIC_DIR   - optional frontend directory (defaults to ./web, then ../web)
}

// requiredVars lists the variables the server cannot start without (R1.4).
// Order defines the order in which missing names appear in the error message.
var requiredVars = []string{"SERVER_PORT", "DATABASE_URL", "REDIS_ADDR", "JWT_SECRET"}

// Load reads configuration from the environment. When variables are missing
// the returned error names *all* of them in a single message, so one log line
// satisfies R1.4 ("identifying the name of each missing variable").
func Load() (Config, error) {
	values := make(map[string]string, len(requiredVars))
	var missing []string
	for _, name := range requiredVars {
		value := strings.TrimSpace(os.Getenv(name))
		if value == "" {
			missing = append(missing, name)
			continue
		}
		values[name] = value
	}
	if len(missing) > 0 {
		return Config{}, fmt.Errorf("missing required environment variable(s): %s", strings.Join(missing, ", "))
	}

	port := values["SERVER_PORT"]
	if _, err := strconv.Atoi(port); err != nil {
		return Config{}, fmt.Errorf("SERVER_PORT must be a number, got %q", port)
	}

	// The frontend directory is optional (it does not exist in early steps);
	// resolve it relative to the working directory so `go run ./cmd/server`
	// works both from server/ and from the repository root.
	staticDir := strings.TrimSpace(os.Getenv("STATIC_DIR"))
	if staticDir == "" {
		staticDir = firstDir("./web", "../web")
	}

	return Config{
		ServerPort:  port,
		DatabaseURL: values["DATABASE_URL"],
		RedisAddr:   values["REDIS_ADDR"],
		JWTSecret:   values["JWT_SECRET"],
		StaticDir:   staticDir,
	}, nil
}

// firstDir returns the first candidate that is an existing directory.
func firstDir(candidates ...string) string {
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate
		}
	}
	return candidates[0]
}
