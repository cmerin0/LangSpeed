package config

import (
	"strings"
	"testing"
)

// TestLoadReportsEveryMissingVariable covers R1.4: when required environment
// variables are absent, the error must name each missing variable.
func TestLoadReportsEveryMissingVariable(t *testing.T) {
	for _, name := range requiredVars {
		t.Setenv(name, "")
	}

	_, err := Load()
	if err == nil {
		t.Fatal("Load() with no environment set should fail")
	}
	for _, name := range requiredVars {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error does not name missing variable %s: %v", name, err)
		}
	}
}

// TestLoadReportsPartialMissingVariables: a single missing variable is enough
// to abort, and it must be identified (R1.4).
func TestLoadReportsPartialMissingVariables(t *testing.T) {
	t.Setenv("SERVER_PORT", "3000")
	t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/db")
	t.Setenv("REDIS_ADDR", "localhost:6379")
	t.Setenv("JWT_SECRET", "")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() with JWT_SECRET missing should fail")
	}
	if !strings.Contains(err.Error(), "JWT_SECRET") {
		t.Errorf("error does not name JWT_SECRET: %v", err)
	}
}

// TestLoadReturnsConfiguredValues: with every variable present the config is
// returned intact (R1.3).
func TestLoadReturnsConfiguredValues(t *testing.T) {
	t.Setenv("SERVER_PORT", "4321")
	t.Setenv("DATABASE_URL", "postgres://user:pass@db:5432/langspeed")
	t.Setenv("REDIS_ADDR", "cache:6379")
	t.Setenv("JWT_SECRET", "secret")
	t.Setenv("STATIC_DIR", "/tmp/web")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}
	if cfg.ServerPort != "4321" {
		t.Errorf("ServerPort = %q, want 4321", cfg.ServerPort)
	}
	if cfg.DatabaseURL != "postgres://user:pass@db:5432/langspeed" {
		t.Errorf("DatabaseURL = %q", cfg.DatabaseURL)
	}
	if cfg.RedisAddr != "cache:6379" {
		t.Errorf("RedisAddr = %q", cfg.RedisAddr)
	}
	if cfg.JWTSecret != "secret" {
		t.Errorf("JWTSecret = %q", cfg.JWTSecret)
	}
	if cfg.StaticDir != "/tmp/web" {
		t.Errorf("StaticDir = %q, want /tmp/web", cfg.StaticDir)
	}
}

// TestLoadRejectsNonNumericPort: SERVER_PORT must be usable as a listen port.
func TestLoadRejectsNonNumericPort(t *testing.T) {
	t.Setenv("SERVER_PORT", "not-a-port")
	t.Setenv("DATABASE_URL", "postgres://localhost/db")
	t.Setenv("REDIS_ADDR", "localhost:6379")
	t.Setenv("JWT_SECRET", "secret")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() with a non-numeric SERVER_PORT should fail")
	}
	if !strings.Contains(err.Error(), "SERVER_PORT") {
		t.Errorf("error does not mention SERVER_PORT: %v", err)
	}
}
