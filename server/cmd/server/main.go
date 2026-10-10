// Command server is the LangSpeed HTTP backend ("the Server" in the
// requirements document).
//
// Startup sequence:
//  1. Load and validate configuration from environment variables; if anything
//     required is missing, log every missing name and exit with status 1 (R1.4).
//  2. Wait until the Database and Cache accept TCP connections - up to 5
//     attempts within 30 seconds (R1.2).
//  3. Apply pending database migrations in order; halt on the first failure,
//     naming the migration (R2.1, R2.2).
//  4. Run the Seed_Script that creates the first admin user (R2.8-R2.10).
//  5. Start the HTTP server - only now are incoming requests accepted (R1.2).
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"langspeed/internal/cache"
	"langspeed/internal/config"
	"langspeed/internal/httpserver"
	"langspeed/internal/logging"
	"langspeed/internal/store"
)

// R1.2: health is confirmed by a successful TCP connection within 30 seconds
// and up to 5 retry attempts, per dependency.
const (
	dependencyAttempts = 5
	dependencyTimeout  = 30 * time.Second
	shutdownTimeout    = 10 * time.Second
)

func main() {
	log := logging.New(os.Stdout)

	// --- 1. Configuration (R1.3, R1.4) -------------------------------------
	cfg, err := config.Load()
	if err != nil {
		// The error names every missing variable; exit code is non-zero.
		log.Error("startup aborted: configuration is incomplete", "error", err.Error())
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// --- 2. Dependency health checks (R1.2) --------------------------------
	dbAddr, err := store.TCPAddrFromURL(cfg.DatabaseURL)
	if err != nil {
		// Deliberately generic: DATABASE_URL embeds credentials (R21.4).
		log.Error("startup aborted: configuration is invalid", "variable", "DATABASE_URL", "error", err.Error())
		os.Exit(1)
	}
	if err := store.WaitReady(ctx, log, "database", dbAddr, dependencyAttempts, dependencyTimeout); err != nil {
		log.Error("startup aborted: dependency unavailable", "dependency", "database", "error", err.Error())
		os.Exit(1)
	}
	if err := store.WaitReady(ctx, log, "cache", cfg.RedisAddr, dependencyAttempts, dependencyTimeout); err != nil {
		log.Error("startup aborted: dependency unavailable", "dependency", "cache", "error", err.Error())
		os.Exit(1)
	}

	// --- 3. Migrations (R2.1, R2.2) ----------------------------------------
	db, err := store.Open(cfg.DatabaseURL)
	if err != nil {
		log.Error("startup aborted: database connection could not be established", "error", err.Error())
		os.Exit(1)
	}
	defer db.Close()

	if err := store.Migrate(ctx, db, log); err != nil {
		// The message names the failing migration; later ones are not applied.
		log.Error("startup aborted: database migration failed", "error", err.Error())
		os.Exit(1)
	}

	// --- 4. Seed_Script (R2.8-R2.10) ---------------------------------------
	if err := store.SeedAdmin(ctx, db, log); err != nil {
		log.Error("startup aborted: seed script failed", "error", err.Error())
		os.Exit(1)
	}

	// --- 5. HTTP server ----------------------------------------------------
	// Game_Sessions live in the Cache (glossary); reachability was already
	// confirmed by the TCP wait above (R1.2).
	sessions := cache.New(cfg.RedisAddr)
	defer func() { _ = sessions.Close() }()

	// Tongue-twister selection (R4, R6) runs against the migration-seeded
	// catalogue via the same pool migrations used.
	content := store.NewContent(db)

	api := httpserver.New(cfg, log, sessions, content)
	httpSrv := &http.Server{
		Addr:              ":" + cfg.ServerPort,
		Handler:           api.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	listenErr := make(chan error, 1)
	go func() {
		log.Info("server listening", "addr", httpSrv.Addr, "static_dir", cfg.StaticDir)
		listenErr <- httpSrv.ListenAndServe()
	}()

	select {
	case err := <-listenErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server failed", "addr", httpSrv.Addr, "error", err.Error())
			os.Exit(1)
		}
	case <-ctx.Done():
		log.Info("shutdown signal received")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			log.Error("graceful shutdown failed", "error", fmt.Sprintf("%v", err))
			os.Exit(1)
		}
		log.Info("server stopped")
	}
}
