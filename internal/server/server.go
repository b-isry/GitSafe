package server

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	postgresdb "github.com/b-isry/gitsafe/internal/postgres"
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
	db         *sql.DB

	sessions   *sessionManager
	userStores *UserStoreManager

	// Daily backups (dailybackup.go). cronSecret empty = endpoint disabled.
	daily      DailyStore
	cronSecret string
	cron       cronState

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

	// localBackupDir is the parent directory for a local bundle while it waits
	// to be downloaded to the user's computer. Empty uses the OS temp directory.
	// The file is removed after a successful download. Tests set it so those
	// files stay inside the test's temp directory.
	localBackupDir string

	// localHoldMu guards localHold, the in-memory map of local bundles waiting
	// for the browser to download them. Paths are never persisted.
	localHoldMu sync.Mutex
	localHold   map[string]heldLocalBundle

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

	// userBackupMu guards userSlots, the per-user (GitHub ID) locks that make
	// one user's backups run one at a time, queued in arrival order.
	userBackupMu sync.Mutex
	userSlots    map[int64]*userSlot

	// disablePerUserBackupLimit is a test-only flag to disable the one-backup-per-user limit.
	// It is not set in production and is only used by tests that need to verify
	// global concurrency limits without the per-user restriction.
	disablePerUserBackupLimit bool

	// bgCtx is the parent of all detached backup work; bgCancel cancels it on
	// Shutdown and bgWG tracks the goroutines so shutdown can wait for them to
	// record their final state.
	bgCtx    context.Context
	bgCancel context.CancelFunc
	bgWG     sync.WaitGroup

	// localOnly restricts the API to loopback clients. It is true only when
	// the configured base URL is a loopback address (a local, single-machine
	// install). A public deployment sits behind a reverse proxy, so the
	// connecting address is never loopback and the check must not apply.
	localOnly bool

	// staticPages holds the parsed public pages (privacy, terms).
	staticPages map[string]*template.Template
}

type pageData struct {
	Active    string
	PageTitle string
}

type renderData struct {
	pageData
	Cloud CloudView
}

type TokenStoreFactory func(userID int64) (TokenStore, error)
type StateStoreFactory func(userID int64) (StateStore, error)

type Options struct {
	TokenStore TokenStoreFactory
	StateStore StateStoreFactory
}

// UserStoreManager manages per-user state and token stores.
// Each user gets their own isolated state file and token namespace.
type UserStoreManager struct {
	mu           sync.RWMutex
	stores       map[int64]*userStores
	basePath     string
	logger       *slog.Logger
	stateFactory StateStoreFactory
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
		stateFactory: func(userID int64) (StateStore, error) {
			statePath := filepath.Join(basePath, fmt.Sprintf("state-%d.json", userID))
			return state.Open(statePath)
		},
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

	stateFactory := m.stateFactory
	if stateFactory == nil {
		statePath := filepath.Join(m.basePath, fmt.Sprintf("state-%d.json", userID))
		stateFactory = func(int64) (StateStore, error) { return state.Open(statePath) }
	}
	st, err := stateFactory(userID)
	if err != nil {
		return nil, fmt.Errorf("open user state store: %w", err)
	}

	factory := m.tokenFactory
	if factory == nil {
		factory = func(int64) (TokenStore, error) { return tokenstore.New(), nil }
	}
	tokenStore, err := factory(userID)
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

// forget drops the cached stores of a user (after account deletion), so a
// later sign-in starts from freshly loaded, empty state.
func (m *UserStoreManager) forget(userID int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.stores, userID)
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
	if app == nil {
		return nil, errors.New("server: app is required")
	}
	if err := validateDeploymentEnvironment(app.Config.BaseURLOrDefault()); err != nil {
		return nil, err
	}

	var db *sql.DB
	databaseURL := strings.TrimSpace(os.Getenv(DatabaseURLEnv))
	if databaseURL != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		var err error
		db, err = postgresdb.Open(ctx, databaseURL)
		if err != nil {
			cancel()
			return nil, err
		}
		if err := postgresdb.EnsureSchema(ctx, db); err != nil {
			cancel()
			_ = db.Close()
			return nil, err
		}
		cancel()
	}
	keepDatabase := false
	defer func() {
		if !keepDatabase && db != nil {
			_ = db.Close()
		}
	}()

	tmpls := make(map[string]*template.Template, len(pages))
	for _, p := range pages {
		t, err := template.New("gitsafe").ParseFS(
			content, "templates/layout.html", "templates/"+p+".html")
		if err != nil {
			return nil, err
		}
		tmpls[p] = t
	}
	staticPages := map[string]*template.Template{}
	for _, p := range []string{"privacy", "terms"} {
		t, err := template.ParseFS(content, "templates/"+p+".html")
		if err != nil {
			return nil, err
		}
		staticPages[p] = t
	}
	if configPath == "" {
		configPath = ConfigPathFile
	}
	basePath := filepath.Dir(configPath)
	if basePath == "." {
		basePath = ".gitsafe"
	}

	tokenFactory := opts.TokenStore
	if tokenFactory == nil {
		var err error
		tokenFactory, err = tokenStoreFactoryFromEnv(db, basePath)
		if err != nil {
			return nil, fmt.Errorf("configure token store: %w", err)
		}
	}
	stateFactory := opts.StateStore
	if stateFactory == nil {
		if db != nil {
			stateFactory = func(userID int64) (StateStore, error) {
				return state.OpenPostgres(db, userID)
			}
		} else {
			stateFactory = func(userID int64) (StateStore, error) {
				statePath := filepath.Join(basePath, fmt.Sprintf("state-%d.json", userID))
				return state.Open(statePath)
			}
		}
	}

	probeUserID := time.Now().UnixNano()
	if probeUserID <= 0 {
		probeUserID = -probeUserID
	}
	if err := probeTokenStore(tokenFactory, probeUserID); err != nil {
		return nil, fmt.Errorf("token store self-test: %w", err)
	}
	if db != nil {
		if err := probeStateStore(stateFactory, probeUserID); err != nil {
			return nil, fmt.Errorf("state store self-test: %w", err)
		}
	}

	bgCtx, bgCancel := context.WithCancel(context.Background())
	srv := &Server{
		tmpls:        tmpls,
		staticPages:  staticPages,
		logger:       logger,
		app:          app,
		configPath:   configPath,
		db:           db,
		sessions:     newSessionManager(sessionKeyMaterial(), strings.HasPrefix(app.Config.BaseURLOrDefault(), "https://")),
		userStores:   NewUserStoreManager(basePath, logger),
		bundleBackup: defaultBundleBackup,
		backupSem:    make(chan struct{}, maxConcurrentBackups),
		jobProgress:  make(map[string]jobProgressMetrics),
		userSlots:    make(map[int64]*userSlot),
		repoSize:     nil,
		bgCtx:        bgCtx,
		bgCancel:     bgCancel,
		localOnly:    IsLoopbackHost(app.Config.BaseURLOrDefault()),
		cronSecret:   cronSecretFromEnv(logger),
	}
	if db != nil {
		srv.daily = newPostgresDailyStore(db)
	} else {
		srv.daily = newMemoryDailyStore()
	}
	srv.userStores.tokenFactory = tokenFactory
	srv.userStores.stateFactory = stateFactory
	srv.githubLister = func(ctx context.Context, token string) ([]providers.Repository, error) {
		return srv.githubClientFor(token).ListRepositories(ctx)
	}
	keepDatabase = true
	return srv, nil
}

// SessionKeyEnv optionally supplies a dedicated key for signing session and
// OAuth-state cookies. When unset, the key is derived from GITSAFE_TOKEN_KEY
// (domain-separated, so the token-encryption key itself is never used for
// signing). Rotating either value signs everyone out.
const SessionKeyEnv = "GITSAFE_SESSION_KEY"

func sessionKeyMaterial() string {
	if v := os.Getenv(SessionKeyEnv); v != "" {
		return v
	}
	if v := os.Getenv(TokenKeyEnv); v != "" {
		return "derived-from-token-key|" + v
	}
	return ""
}

func probeTokenStore(factory TokenStoreFactory, userID int64) (err error) {
	st, err := factory(userID)
	if err != nil {
		return fmt.Errorf("create backend: %w", err)
	}
	ref := "__gitsafe_probe_" + randToken(8)
	defer func() {
		if cleanupErr := st.Delete(ref); err == nil && cleanupErr != nil {
			err = fmt.Errorf("delete: %w", cleanupErr)
		}
	}()
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
	return nil
}

func probeStateStore(factory StateStoreFactory, userID int64) (err error) {
	st, err := factory(userID)
	if err != nil {
		return fmt.Errorf("create backend: %w", err)
	}
	if cleaner, ok := st.(interface{ Delete() error }); ok {
		defer func() {
			if cleanupErr := cleaner.Delete(); err == nil && cleanupErr != nil {
				err = fmt.Errorf("delete: %w", cleanupErr)
			}
		}()
	}
	connection := state.GitHubConnection{
		GitHubID:    userID,
		Login:       "__gitsafe_probe__",
		ConnectedAt: time.Now().UTC(),
		TokenRef:    "__gitsafe_state_probe__",
	}
	st.SetGitHubConnection(connection)
	if err := st.Save(); err != nil {
		return fmt.Errorf("save: %w", err)
	}
	reloaded, err := factory(userID)
	if err != nil {
		return fmt.Errorf("reload: %w", err)
	}
	got, ok := reloaded.GitHubConnection()
	if !ok || got.GitHubID != userID || got.Login != connection.Login || got.TokenRef != connection.TokenRef {
		return errors.New("read-back mismatch")
	}
	return nil
}

func (s *Server) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Server) Routes() http.Handler {
	staticFS, err := fs.Sub(content, "static")
	if err != nil {
		panic(err)
	}

	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))
	// "/{$}" matches only the root path. Everything else that no route claims
	// gets a real 404 instead of silently rendering the landing page (which
	// also made a misconfigured health check look healthy).
	mux.HandleFunc("GET /{$}", s.handleCloudRepositories)
	mux.HandleFunc("/", s.handleNotFound)

	// Health check endpoint (no rate limiting, no auth, no database round
	// trip: a DB ping here would keep a free Postgres instance from ever
	// suspending). GET patterns also answer HEAD.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	mux.HandleFunc("GET /privacy", s.handlePrivacy)
	mux.HandleFunc("GET /terms", s.handleTerms)

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
	// Daily backups: the scheduler endpoints authenticate with the shared
	// bearer secret (no session, no loopback guard); the settings endpoints are
	// regular session + CSRF APIs.
	mux.Handle("POST /api/cron/backup", authLimiter(http.HandlerFunc(s.handleCronBackup)))
	mux.Handle("GET /api/cron/backup", apiLimiter(http.HandlerFunc(s.handleCronStatus)))
	mux.Handle("GET /api/daily-backup", apiLimiter(s.loopback(s.handleGetDailyBackup)))
	mux.Handle("PUT /api/daily-backup", apiLimiter(s.loopback(s.handlePutDailyBackup)))

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
	mux.Handle("GET /api/backup-jobs/{id}/bundle", apiLimiter(s.loopback(s.handleDownloadLocalBundle)))
	mux.Handle("GET /api/backup-jobs/{id}", apiLimiter(s.loopback(s.handleProtectedBackupJob)))
	mux.Handle("GET /api/protected-repositories/{id}/backup-jobs", apiLimiter(s.loopback(s.handleProtectedBackupJobs)))
	mux.Handle("GET /api/protected-repositories/{id}/backups", apiLimiter(s.loopback(s.handleProtectedBackupHistory)))

	return mux
}

// loopback restricts an API endpoint to local clients when GitSafe runs as a
// local, single-machine install (loopback base URL), so it is not reachable
// from the LAN. For a public deployment (non-loopback base URL) requests
// arrive through the hosting provider's proxy, so the check is skipped; every
// endpoint still enforces its own session and CSRF checks.
func (s *Server) loopback(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.localOnly {
			next(w, r)
			return
		}
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil || !net.ParseIP(host).IsLoopback() {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next(w, r)
	})
}

// handlePrivacy renders the public privacy page. (The page is required for
// the Google OAuth consent screen.)
func (s *Server) handlePrivacy(w http.ResponseWriter, r *http.Request) {
	s.renderStatic(w, "privacy")
}

// handleTerms renders the public terms page.
func (s *Server) handleTerms(w http.ResponseWriter, r *http.Request) {
	s.renderStatic(w, "terms")
}

// renderStatic renders a public page. The page files wrap their markup in a
// {{define "content"}} block, so that block (not the empty file-level
// template) is what must be executed; executing the file template rendered
// nothing and returned a 500.
func (s *Server) renderStatic(w http.ResponseWriter, page string) {
	t, ok := s.staticPages[page]
	if !ok {
		http.NotFound(w, nil)
		return
	}
	var buf strings.Builder
	if err := t.ExecuteTemplate(&buf, "content", struct{ LastUpdated string }{
		LastUpdated: pagesLastUpdated,
	}); err != nil {
		s.logger.Error("render page", "page", page, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(buf.String()))
}

// pagesLastUpdated is the revision date shown on the privacy and terms pages.
// It is a fixed date (bump it when the text changes), not "today".
const pagesLastUpdated = "2026-10-08"

// handleNotFound answers any path no route claims: JSON for API paths, a
// small HTML page otherwise.
func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	fmt.Fprint(w, notFoundHTML)
}

const notFoundHTML = `<!doctype html>
<html lang="en" data-theme="dark">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Not found · GitSafe</title>
  <link rel="stylesheet" href="/static/styles.css">
</head>
<body>
  <div class="app">
    <main class="content">
      <div class="onboarding">
        <div class="hero-card">
          <h1 class="hero-card__title">Page not found</h1>
          <p class="hero-card__subtitle">There is nothing at this address.</p>
          <a class="btn btn--primary" href="/" style="margin-top:1.5rem">Back to GitSafe</a>
        </div>
      </div>
    </main>
  </div>
</body>
</html>`

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
	s.clearSessionCookie(w)
	writeJSON(w, http.StatusOK, map[string]string{"status": "logged out"})
}

// handleDeleteAccount revokes OAuth grants, deletes the stored tokens and all
// persisted user data, and destroys the session.
func (s *Server) handleDeleteAccount(w http.ResponseWriter, r *http.Request) {
	userID, stateStore, tokenStore, sess, err := s.requireUser(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "No active session"})
		return
	}
	if !validateCSRF(sess, r.Header.Get("X-CSRF-Token")) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "CSRF validation failed"})
		return
	}

	// Best-effort server-side revocation first (needs the tokens).
	if conn, ok := stateStore.GitHubConnection(); ok && s.revokeToken != nil {
		if token, err := tokenStore.Get(conn.TokenRef); err == nil {
			ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
			if err := s.revokeToken(ctx, token); err != nil {
				s.logger.Warn("account delete: github revocation failed", "error", err)
			}
			cancel()
		}
	}
	if driveConn, ok := s.driveConnection(stateStore); ok && s.driveRevoke != nil {
		if token, err := tokenStore.Get(driveConn.TokenRef); err == nil {
			ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
			if err := s.driveRevoke(ctx, token); err != nil {
				s.logger.Warn("account delete: drive revocation failed", "error", err)
			}
			cancel()
		}
	}

	// Delete the stored (encrypted) tokens. Previously they were left behind
	// in the token store after "account deleted".
	refs := map[string]bool{
		tokenstore.GitHubTokenFor(userID): true,
		tokenstore.DriveTokenFor(userID):  true,
	}
	if conn, ok := stateStore.GitHubConnection(); ok && conn.TokenRef != "" {
		refs[conn.TokenRef] = true
	}
	if dc, ok := stateStore.DriveConnection(); ok && dc.TokenRef != "" {
		refs[dc.TokenRef] = true
	}
	for ref := range refs {
		if err := tokenStore.Delete(ref); err != nil && !errors.Is(err, tokenstore.ErrNotFound) {
			s.logger.Error("account delete: remove token", "cause", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not delete stored credentials"})
			return
		}
	}

	// Delete the user's data directory (always the session user's ID; the
	// connection may already be gone).
	_ = os.RemoveAll(filepath.Join(s.app.OutputPath, fmt.Sprintf("user-%d", userID)))

	// Clear AND persist the state (previously only cleared in memory, so the
	// data came back after a restart).
	stateStore.ClearAll()
	if err := s.saveState(stateStore); err != nil {
		s.logger.Error("account delete: persist cleared state", "cause", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not delete account data"})
		return
	}
	if deleter, ok := stateStore.(interface{ Delete() error }); ok {
		if err := deleter.Delete(); err != nil {
			s.logger.Warn("account delete: remove state row", "error", err)
		}
	}
	s.userStores.forget(userID)
	if s.daily != nil {
		if err := s.daily.Delete(r.Context(), userID); err != nil {
			s.logger.Warn("account delete: remove daily backup settings", "error", err)
		}
	}

	// Destroy session
	s.clearSessionCookie(w)

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
