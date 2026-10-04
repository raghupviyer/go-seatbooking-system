package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/raghupviyer/go-ticketbooking-system/internal/api"
	"github.com/raghupviyer/go-ticketbooking-system/internal/auth"
	"github.com/raghupviyer/go-ticketbooking-system/internal/config"
	"github.com/raghupviyer/go-ticketbooking-system/internal/store"
)

func main() {
	cfg, err := config.Load()
	// Logs go to stdout as JSON so the platform's log viewer can index them.
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel})))
	if err != nil {
		fatal("config", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		fatal("postgres", err)
	}
	defer db.Close()

	if err := store.Migrate(ctx, db); err != nil {
		fatal("migrate", err)
	}

	srv := &http.Server{
		Addr:    ":" + cfg.Port,
		Handler: api.New(db, cfg, auth.NewTokens(cfg.JWTSecret)).Routes(),
		// Generous enough that a connection queued behind a burst of thousands is
		// still served, while still cutting off clients that never send headers.
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		slog.Info("listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fatal("server", err)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(shutdownCtx)
}

func fatal(msg string, err error) {
	slog.Error(msg, "err", err)
	os.Exit(1)
}
