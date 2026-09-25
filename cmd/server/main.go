package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/b-isry/gitsafe/internal/archiver"
	"github.com/b-isry/gitsafe/internal/config"
	"github.com/b-isry/gitsafe/internal/server"
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

	// Validate git is available and at least version 2.31 (required for http.extraheader)
	if err := archiver.ValidateGitVersion(2, 31); err != nil {
		logger.Error("git validation failed", "error", err)
		os.Exit(1)
	}
	// Log git version
	if out, err := exec.Command("git", "--version").Output(); err == nil {
		logger.Info("git version", "version", strings.TrimSpace(string(out)))
	}

	// Sweep stale gitsafe-* temp dirs at startup
	sweepStaleTempDirs(logger)

	app := &server.App{
		Config:     cfg,
		OutputPath: cfg.OutputPath,
	}

	baseURL := cfg.BaseURLOrDefault()
	logger.Info("base URL configured", "baseURL", baseURL)

	srv, err := server.New(logger, app, "config.yaml")
	if err != nil {
		logger.Error("failed to create server", "error", err)
		os.Exit(1)
	}
	defer func() {
		if err := srv.Close(); err != nil {
			logger.Warn("database shutdown", "error", err)
		}
	}()

	// Phase 1: optional GitHub OAuth.
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
	srv.ConfigureCloud(oauth)
	srv.ConfigureDriveOAuth(driveOAuth)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	addr := ":" + port

	// HTTP server with security hardening
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           maxBytesMiddleware(securityHeadersMiddleware(srv.Routes())),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// Graceful shutdown: on interrupt or SIGTERM, stop accepting connections
	// and let any in-flight requests finish so the process never hangs.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	go func() {
		<-ctx.Done()
		logger.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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

// sweepStaleTempDirs removes any gitsafe-* temp directories left over from
// previous runs. This prevents disk exhaustion from orphaned temp dirs.
func sweepStaleTempDirs(logger *slog.Logger) {
	tmpDir := os.TempDir()
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		logger.Warn("failed to read temp dir for sweep", "error", err)
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, "gitsafe-") {
			path := filepath.Join(tmpDir, name)
			if err := os.RemoveAll(path); err != nil {
				logger.Warn("failed to remove stale temp dir", "path", path, "error", err)
			} else {
				logger.Info("removed stale temp dir", "path", path)
			}
		}
	}
}

// maxBytesMiddleware limits request body size to prevent memory exhaustion.
func maxBytesMiddleware(next http.Handler) http.Handler {
	const maxBodySize = 10 << 20 // 10 MB
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxBodySize)
		next.ServeHTTP(w, r)
	})
}

// securityHeadersMiddleware adds security headers to all responses.
func securityHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Content Security Policy - restrictive by default
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; font-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		// HSTS in production mode (non-loopback base URL)
		// Note: In production, the base URL would be https://domain.com
		// For now, we only set HSTS if the request is HTTPS
		if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains; preload")
		}
		next.ServeHTTP(w, r)
	})
}
