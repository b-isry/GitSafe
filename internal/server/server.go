package server

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/b-isry/gitsafe/internal/providers"
	"github.com/b-isry/gitsafe/internal/state"
	"github.com/b-isry/gitsafe/internal/tokenstore"
)

//go:embed templates static
var content embed.FS

type Server struct {
	tmpls      map[string]*template.Template
	logger     *slog.Logger
	app        *App
	configPath string

	sessions   *sessionManager
	userStores *UserStoreManager

	// GitHub OAuth wiring (Phase 1). Optional: when unset, GitHub appears
	// disconnected and OAuth endpoints report a 503 "not configured".
	github *GitHubOAuth
	// connectGitHub performs the OAuth code exchange and identity lookup. It is
	// a function field (set by ConfigureCloud) so tests can stub it without
	// network access.
	connectGitHub func(ctx context.Context, code string) (token string, scopes []string, identity providers.Identity, err error)
	// revokeToken invalidates a GitHub access token server-side through the
	// GitHub API using the deployment's client credentials. It is a function
	// field so tests can stub the network call; the production default is wired
	// by ConfigureCloud and is always best-effort on disconnect.
	revokeToken func(ctx context.Context, token string) error

	// Drive OAuth wiring. Optional: when unset, Drive appears not connected and
	// its endpoints report a 503 "not configured".
	driveOAuth *DriveOAuth
	// connectDrive performs the Drive OAuth code exchange and resolves the
	// account identity + GitSafe storage folder. It is a function field (set by
	// ConfigureDriveOAuth) so tests can stub it without network access.
	connectDrive func(ctx context.Context, code string) (driveConnectResult, error)
	// driveRevoke invalidates a Drive token server-side through Google's
	// revocation endpoint (no client credentials needed). Best-effort on
	// disconnect; wired by ConfigureDriveOAuth.
	driveRevoke func(ctx context.Context, token string) error

	// githubLister returns the discovered repository set for an access token. It
	// is a function field so tests can stub the GitHub work without network
	// access; the production default calls the live GitHub provider.
	githubLister func(ctx context.Context, token string) ([]providers.Repository, error)

	// bundleBackup creates a git bundle for a protected GitHub repository. It is
	// a function field so tests can stub the git/network work without real
	// network access; the production default calls archiver.BundleRemoteRepo.
	bundleBackup bundleBackupFunc

	// driveUpload uploads a completed local bundle to Google Drive and returns
	// the resulting Drive file ID. It is a function field so tests can stub the
	// Google work without real credentials or network access; the production
	// default calls cloud.UploadFile. It is only invoked when Drive is enabled.
	driveUpload driveUploadFunc

	// repoSize returns the size of a repository in MB. It is a function field
	// so tests can stub the GitHub API call without network access.
	repoSize repoSizeFunc

	// jobMu serializes protected-backup job creation so a repository is never
	// given two concurrent jobs.
	jobMu sync.Mutex

	// backupSem is a global semaphore capping how many protected-repo mirrors
	// can execute at once, so a large queue of backup requests cannot spawn an
	// unbounded number of concurrent git clones/uploads.
	backupSem chan struct{}

	// jobProgressMu guards jobProgress, which holds live upload byte progress
	// for in-flight backup jobs so the polling UI can render a determinate bar.
	// Progress is intentionally never persisted to state or disk.
	jobProgressMu sync.Mutex
	jobProgress   map[string]jobProgressMetrics

	// userBackupMu guards userBackups, which tracks how many backups are
	// currently running per user (GitHub ID). Enforces one backup per user.
	userBackupMu sync.Mutex
	userBackups  map[int64]int

	// disablePerUserBackupLimit is a test-only flag to disable the one-backup-per-user limit.
	// It is not set in production and is only used by tests that need to verify
	// global concurrency limits without the per-user restriction.
	disablePerUserBackupLimit bool
}

type pageData struct {
	Active    string
	PageTitle string
}

type renderData struct {
	pageData
	Cloud CloudView
}

// TokenStoreFactory returns the per-user token store backend. It is called on
// each user's first access so the server can keep one FileStore instance or
// build fresh keyring handles without tests touching the real OS keychain.
type TokenStoreFactory func() (TokenStore, error)

// Options configures Server construction. Zero value selects the
// environment-derived token backend (see tokenStoreFactoryFromEnv).
type Options struct {
	// TokenStore overrides the token backend selection. Tests inject fakes here
	// so the construction self-test never touches a live OS keychain.
	TokenStore TokenStoreFactory
}

// UserStoreManager manages per-user state and token stores.
// Each user gets their own isolated state file and token namespace.
type UserStoreManager struct {
	mu           sync.RWMutex
	stores       map[int64]*userStores
	basePath     string
	logger       *slog.Logger
	tokenFactory TokenStoreFactory
}

type userStores struct {
	state StateStore
	token TokenStore
}

// UserStoreManager creates a new manager for per-user stores.
// basePath is the directory where per-user state files will be stored (e.g., ".gitsafe").
func NewUserStoreManager(basePath string, logger *slog.Logger) *UserStoreManager {
	return &UserStoreManager{
		stores:   make(map[int64]*userStores),
		basePath: basePath,
		logger:   logger,
	}
}

// getOrCreate returns the per-user stores for the given GitHub user ID.
// It creates them on first access.
func (m *UserStoreManager) getOrCreate(userID int64) (*userStores, error) {
	m.mu.RLock()
	if stores, ok := m.stores[userID]; ok {
		m.mu.RUnlock()
		return stores, nil
	}
	m.mu.RUnlock()

	m.mu.Lock()
	defer m.mu.Unlock()

	// Double-check after acquiring write lock
	if stores, ok := m.stores[userID]; ok {
		return stores, nil
	}

	// Create per-user state file path: .gitsafe/state-{userID}.json
	statePath := filepath.Join(m.basePath, fmt.Sprintf("state-%d.json", userID))
	st, err := state.Open(statePath)
	if err != nil {
		return nil, fmt.Errorf("open user state store: %w", err)
	}

	factory := m.tokenFactory
	if factory == nil {
		// Defensive fallback for direct manager construction; production and
		// tests go through New/NewWithOptions which always set a factory.
		factory = func() (TokenStore, error) { return tokenstore.New(), nil }
	}
	tokenStore, err := factory()
	if err != nil {
		return nil, fmt.Errorf("create user token store: %w", err)
	}

	stores := &userStores{
		state: st,
		token: tokenStore,
	}
	m.stores[userID] = stores
	return stores, nil
}

// requireUser extracts the authenticated user from the request session.
// Returns the user's GitHub ID, state store, token store, and session.
// If no valid session or no authenticated user, returns an error.
func (s *Server) requireUser(r *http.Request) (int64, StateStore, TokenStore, *session, error) {
	sess := s.sessionFromRequest(r)
	if sess == nil {
		return 0, nil, nil, nil, fmt.Errorf("no session")
	}
	if sess.userID == 0 {
		return 0, nil, nil, nil, fmt.Errorf("no authenticated user")
	}
	stores, err := s.userStores.getOrCreate(sess.userID)
	if err != nil {
		return 0, nil, nil, nil, err
	}
	return sess.userID, stores.state, stores.token, sess, nil
}

// requireUserOrZero is like requireUser but returns zero values instead of error
// for handlers that need to render different content for authenticated vs unauthenticated users.
func (s *Server) requireUserOrZero(r *http.Request) (int64, StateStore, TokenStore, *session) {
	userID, st, tok, sess, err := s.requireUser(r)
	if err != nil {
		return 0, nil, nil, nil
	}
	return userID, st, tok, sess
}

var pages = []string{cloudRepositoriesPage}

// ConfigPathFile is the fallback location used when New is given an empty path
// (for example in tests where persistence is not exercised).
const ConfigPathFile = "config.yaml"

func New(logger *slog.Logger, app *App, configPath string) (*Server, error) {
	return NewWithOptions(logger, app, configPath, Options{})
}

// NewWithOptions builds a Server, selecting the per-user token backend and
// verifying it with a Set/Get/Delete self-test before boot. A backend that
// cannot round-trip a token (for example a headless container whose OS keyring
// has no Secret Service) refuses startup with an error instead of failing at
// the first login.
func NewWithOptions(logger *slog.Logger, app *App, configPath string, opts Options) (*Server, error) {
	tmpls := make(map[string]*template.Template, len(pages))
	for _, p := range pages {
		t, err := template.New("gitsafe").ParseFS(
			content, "templates/layout.html", "templates/"+p+".html")
		if err != nil {
			return nil, err
		}
		tmpls[p] = t
	}
	if configPath == "" {
		configPath = ConfigPathFile
	}
	// Determine base path for per-user state files
	// Use the directory of the config file, or default to .gitsafe
	basePath := filepath.Dir(configPath)
	if basePath == "." {
		basePath = ".gitsafe"
	}

	factory := opts.TokenStore
	if factory == nil {
		var err error
		factory, err = tokenStoreFactoryFromEnv(basePath)
		if err != nil {
			return nil, fmt.Errorf("configure token store: %w", err)
		}
	}
	if err := probeTokenStore(factory); err != nil {
		return nil, fmt.Errorf("token store self-test: %w", err)
	}

	srv := &Server{
		tmpls:        tmpls,
		logger:       logger,
		app:          app,
		configPath:   configPath,
		sessions:     newSessionManager(),
		userStores:   NewUserStoreManager(basePath, logger),
		bundleBackup: defaultBundleBackup,
		backupSem:    make(chan struct{}, maxConcurrentBackups),
		jobProgress:  make(map[string]jobProgressMetrics),
		userBackups:  make(map[int64]int),
		repoSize:     nil, // nil means use default implementation
	}
	srv.userStores.tokenFactory = factory
	srv.githubLister = func(ctx context.Context, token string) ([]providers.Repository, error) {
		return srv.githubClientFor(token).ListRepositories(ctx)
	}
	return srv, nil
}

// probeTokenStore exercises the selected backend with a throwaway reference.
// Any failure (construction, Set, Get, Delete, or a wrong read-back) refuses
// the boot.
func probeTokenStore(factory TokenStoreFactory) error {
	st, err := factory()
	if err != nil {
		return fmt.Errorf("create backend: %w", err)
	}
	ref := "__gitsafe_probe_" + randToken(8)
	if err := st.Set(ref, "probe"); err != nil {
		return fmt.Errorf("set: %w", err)
	}
	got, err := st.Get(ref)
	if err != nil {
		return fmt.Errorf("get: %w", err)
	}
	if got != "probe" {
		return fmt.Errorf("read-back mismatch: got %q", got)
	}
	if err := st.Delete(ref); err != nil {
		return fmt.Errorf("delete: %w", err)
	}
	return nil
}

func (s *Server) Routes() http.Handler {
	staticFS, err := fs.Sub(content, "static")
	if err != nil {
		panic(err)
	}

	mux := http.NewServeMux()
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))
	mux.HandleFunc("/", s.handleCloudRepositories)

	// Health check endpoint (no rate limiting, no auth)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	// Rate limiting middleware
	apiLimiter := s.rateLimitMiddleware(
		s.app.Config.RateLimitRequests,
		time.Duration(s.app.Config.RateLimitWindowSeconds)*time.Second,
		s.getRateLimitKey,
	)
	authLimiter := s.rateLimitMiddleware(
		s.app.Config.AuthRateLimitRequests,
		time.Duration(s.app.Config.AuthRateLimitWindowSeconds)*time.Second,
		s.getRateLimitKey,
	)

	// Phase 1: GitHub OAuth and cloud connections.
	mux.Handle("GET /api/csrf", authLimiter(s.loopback(s.handleCSRF)))
	mux.Handle("GET /api/auth/github", authLimiter(s.loopback(s.handleGitHubLogin)))
	mux.Handle("GET /api/auth/github/callback", authLimiter(s.loopback(s.handleGitHubCallback)))
	mux.Handle("GET /api/connections", apiLimiter(s.loopback(s.handleAPIConnections)))
	mux.Handle("GET /api/repositories", apiLimiter(s.loopback(s.handleAPIRepositories)))
	mux.Handle("DELETE /api/connections/github", apiLimiter(s.loopback(s.handleDisconnectGitHub)))

	// Auth: logout and account deletion
	mux.Handle("POST /api/auth/logout", apiLimiter(s.loopback(s.handleLogout)))
	mux.Handle("POST /api/account/delete", apiLimiter(s.loopback(s.handleDeleteAccount)))

	// Drive OAuth and connection management.
	mux.Handle("GET /api/auth/drive", authLimiter(s.loopback(s.handleDriveLogin)))
	mux.Handle("GET /api/auth/drive/callback", authLimiter(s.loopback(s.handleDriveCallback)))
	mux.Handle("DELETE /api/connections/drive", apiLimiter(s.loopback(s.handleDisconnectDrive)))

	// Legacy protection endpoints, retained so existing tooling keeps working.
	// The dashboard no longer uses the protect/protected model.
	mux.Handle("GET /api/protected-repositories", apiLimiter(s.loopback(s.handleListProtectedRepositories)))
	mux.Handle("POST /api/protected-repositories", apiLimiter(s.loopback(s.handleProtectRepositories)))
	mux.Handle("DELETE /api/protected-repositories/{id}", apiLimiter(s.loopback(s.handleRemoveProtectedRepository)))

	// Direct backup flow: any GitHub repository can be backed up without being
	// protected first, and "back up all" sweeps every repository directly.
	mux.Handle("POST /api/repositories/{githubId}/backup", apiLimiter(s.loopback(s.handleBackupRepo)))
	mux.Handle("POST /api/backups", apiLimiter(s.loopback(s.handleBackupAll)))

	// Phase 3: backup execution and job/history inspection.
	mux.Handle("POST /api/protected-repositories/{id}/backup", apiLimiter(s.loopback(s.handleBackupProtectedRepo)))
	mux.Handle("GET /api/backup-jobs/{id}", apiLimiter(s.loopback(s.handleProtectedBackupJob)))
	mux.Handle("GET /api/protected-repositories/{id}/backup-jobs", apiLimiter(s.loopback(s.handleProtectedBackupJobs)))
	mux.Handle("GET /api/protected-repositories/{id}/backups", apiLimiter(s.loopback(s.handleProtectedBackupHistory)))

	return mux
}

// loopback restricts an API endpoint to local clients. Sensitive endpoints must
// never be reachable from the network; the web server itself is a local machine
// tool.
func (s *Server) loopback(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil || !net.ParseIP(host).IsLoopback() {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next(w, r)
	})
}

func (s *Server) render(w http.ResponseWriter, page string, data renderData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpls[page].ExecuteTemplate(w, "layout.html", data); err != nil {
		s.logger.Error("render page", "page", page, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// handleLogout destroys the current session.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	sess := s.sessionFromRequest(r)
	if sess == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "No active session"})
		return
	}
	if !validateCSRF(sess, r.Header.Get("X-CSRF-Token")) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "CSRF validation failed"})
		return
	}
	s.sessions.remove(sess.id)
	writeJSON(w, http.StatusOK, map[string]string{"status": "logged out"})
}

// handleDeleteAccount revokes OAuth grants, deletes user data, and destroys the session.
func (s *Server) handleDeleteAccount(w http.ResponseWriter, r *http.Request) {
	_, stateStore, tokenStore, sess, err := s.requireUser(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "No active session"})
		return
	}
	if !validateCSRF(sess, r.Header.Get("X-CSRF-Token")) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "CSRF validation failed"})
		return
	}

	// Get GitHub connection for revocation
	conn, ok := stateStore.GitHubConnection()
	if ok && s.revokeToken != nil {
		if token, err := tokenStore.Get(conn.TokenRef); err == nil {
			ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
			_ = s.revokeToken(ctx, token)
			cancel()
		}
	}

	// Revoke Drive token
	if driveConn, ok := s.driveConnection(stateStore); ok && s.driveRevoke != nil {
		if token, err := tokenStore.Get(driveConn.TokenRef); err == nil {
			ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
			_ = s.driveRevoke(ctx, token)
			cancel()
		}
	}

	// Delete user's data directory
	userDataDir := filepath.Join(s.app.OutputPath, fmt.Sprintf("user-%d", conn.GitHubID))
	_ = os.RemoveAll(userDataDir)

	// Clear state store
	stateStore.ClearGitHubConnection()
	stateStore.ClearDriveConnection()
	// Remove all protected repos, backup records, and jobs for this user
	// (In a real implementation, we'd filter by user; for now clear all)
	stateStore.ClearAll()

	// Destroy session
	s.sessions.remove(sess.id)

	writeJSON(w, http.StatusOK, map[string]string{"status": "account deleted"})
}

// maxRequestBody bounds every JSON request body so a damaged or malicious local
// client cannot force unbounded memory allocation. It is generous for every
// current endpoint (protected-repository batches).
const maxRequestBody = 1 << 20 // 1 MiB

// decodeJSONBody decodes r.Body into v through a size-capped reader. It returns
// an error for too-large or malformed bodies; callers map it to a 400 response.
// The max-bytes reader writes its own "request body too large" response and
// disables keep-alive for oversized bodies.
func decodeJSONBody(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	return json.NewDecoder(r.Body).Decode(v)
}
