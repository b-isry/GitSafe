package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"github.com/b-isry/gitsafe/internal/backup"
	"github.com/b-isry/gitsafe/internal/config"
	"github.com/b-isry/gitsafe/internal/server"
	"github.com/b-isry/gitsafe/internal/state"
	"github.com/b-isry/gitsafe/internal/tokenstore"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	cfg, err := config.Load("config.yaml")
	if err != nil {
		logger.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	// Resolve the output path to an absolute directory up front. Backups run git
	// with `-C <repo>`, which would resolve a relative path against each
	// repository rather than the server working directory.
	if abs, err := filepath.Abs(cfg.OutputPath); err == nil {
		cfg.OutputPath = abs
	}

	if err := os.MkdirAll(cfg.OutputPath, 0o755); err != nil {
		logger.Error("failed to create output directory", "path", cfg.OutputPath, "error", err)
		os.Exit(1)
	}

	app := &server.App{
		Logger:     logger,
		Config:     cfg,
		Runner:     backup.New(logger),
		OutputPath: cfg.OutputPath,
		Threshold:  cfg.Days,
	}

	store := server.NewBackupStore(logger)
	srv, err := server.New(logger, app, store, "config.yaml")
	if err != nil {
		logger.Error("failed to create server", "error", err)
		os.Exit(1)
	}

	// Phase 1: persistent state + keychain token store + optional GitHub OAuth.
	stateStore, err := state.Open(state.DefaultPath)
	if err != nil {
		logger.Error("failed to open state store", "error", err)
		os.Exit(1)
	}
	var oauth *server.GitHubOAuth
	if gh, ok := server.GitHubOAuthFromEnv(cfg.GitHub.ClientID, cfg.GitHub.RedirectURL); ok {
		oauth = gh
		logger.Info("github oauth configured")
	}
	srv.ConfigureCloud(stateStore, tokenstore.New(), oauth)
	// Schedule periodic retention cleanup when the config enables it.
	srv.StartCleanupScheduler()

	addr := "127.0.0.1:8080"
	httpSrv := &http.Server{
		Addr:    addr,
		Handler: srv.Routes(),
	}

	// Graceful shutdown: on interrupt, stop accepting connections, let any
	// in-flight requests finish, and stop the cleanup scheduler cleanly so no
	// goroutine is leaked and the process never hangs.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	go func() {
		<-ctx.Done()
		logger.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			logger.Warn("http shutdown", "error", err)
		}
	}()

	logger.Info("gitsafe web listening", "addr", addr)
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("server stopped", "error", err)
		os.Exit(1)
	}
	srv.StopCleanupScheduler()
	logger.Info("gitsafe web stopped")
}
