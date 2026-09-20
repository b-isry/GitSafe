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

	"github.com/b-isry/gitsafe/internal/config"
	"github.com/b-isry/gitsafe/internal/server"
	"github.com/b-isry/gitsafe/internal/state"
	"github.com/b-isry/gitsafe/internal/tokenstore"
	"github.com/joho/godotenv"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	// Load the optional .env file first so deployment-level values (the GitHub
	// OAuth client id/secret) are set before config is read and expanded.
	if err := godotenv.Load(); err != nil && !os.IsNotExist(err) {
		logger.Warn("failed to load .env", "error", err)
	}

	cfg, err := config.Load("config.yaml")
	if err != nil {
		logger.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	// Resolve the output path to an absolute directory up front. Backups run git
	// with `-C <mirror>`, so a relative path would be resolved against the server
	// working directory rather than the expected bundle location.
	if abs, err := filepath.Abs(cfg.OutputPath); err == nil {
		cfg.OutputPath = abs
	}

	if err := os.MkdirAll(cfg.OutputPath, 0o755); err != nil {
		logger.Error("failed to create output directory", "path", cfg.OutputPath, "error", err)
		os.Exit(1)
	}

	app := &server.App{
		Config:     cfg,
		OutputPath: cfg.OutputPath,
	}

	srv, err := server.New(logger, app, "config.yaml")
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
	baseURL := cfg.BaseURLOrDefault()
	logger.Info("base URL configured", "baseURL", baseURL)
	var oauth *server.GitHubOAuth
	if gh, ok := server.GitHubOAuthFromEnv(cfg.GitHub.ClientID, baseURL); ok {
		oauth = gh
		logger.Info("github oauth configured")
	}
	var driveOAuth *server.DriveOAuth
	if d, ok := server.DriveOAuthFromEnv(cfg.DriveOAuth.ClientID, baseURL); ok {
		driveOAuth = d
		logger.Info("drive oauth configured")
	}
	srv.ConfigureCloud(stateStore, tokenstore.New(), oauth)
	srv.ConfigureDriveOAuth(driveOAuth)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	addr := ":" + port
	httpSrv := &http.Server{
		Addr:    addr,
		Handler: srv.Routes(),
	}

	// Graceful shutdown: on interrupt, stop accepting connections and let any
	// in-flight requests finish so the process never hangs.
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
	logger.Info("gitsafe web stopped")
}
