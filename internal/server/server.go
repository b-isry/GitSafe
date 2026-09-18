package server

import (
	"context"
	"embed"
	"encoding/json"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"sync"

	"github.com/b-isry/gitsafe/internal/providers"
)

//go:embed templates static
var content embed.FS

type Server struct {
	tmpls      map[string]*template.Template
	logger     *slog.Logger
	app        *App
	configPath string

	sessions *sessionManager

	// GitHub OAuth wiring (Phase 1). Optional: when unset, GitHub appears
	// disconnected and OAuth endpoints report a 503 "not configured".
	stateStore StateStore
	tokenStore TokenStore
	github     *GitHubOAuth
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
}

type pageData struct {
	Active    string
	PageTitle string
}

type renderData struct {
	pageData
	Cloud CloudView
}

var pages = []string{cloudRepositoriesPage}

// ConfigPathFile is the fallback location used when New is given an empty path
// (for example in tests where persistence is not exercised).
const ConfigPathFile = "config.yaml"

func New(logger *slog.Logger, app *App, configPath string) (*Server, error) {
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
	srv := &Server{
		tmpls:        tmpls,
		logger:       logger,
		app:          app,
		configPath:   configPath,
		sessions:     newSessionManager(),
		bundleBackup: defaultBundleBackup,
		backupSem:    make(chan struct{}, maxConcurrentBackups),
		jobProgress:  make(map[string]jobProgressMetrics),
	}
	srv.githubLister = func(ctx context.Context, token string) ([]providers.Repository, error) {
		return srv.githubClientFor(token).ListRepositories(ctx)
	}
	return srv, nil
}

func (s *Server) Routes() http.Handler {
	staticFS, err := fs.Sub(content, "static")
	if err != nil {
		panic(err)
	}

	mux := http.NewServeMux()
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))
	mux.HandleFunc("/", s.handleCloudRepositories)

	// Phase 1: GitHub OAuth and cloud connections.
	mux.Handle("GET /api/csrf", s.loopback(s.handleCSRF))
	mux.Handle("GET /api/auth/github", s.loopback(s.handleGitHubLogin))
	mux.Handle("GET /api/auth/github/callback", s.loopback(s.handleGitHubCallback))
	mux.Handle("GET /api/connections", s.loopback(s.handleAPIConnections))
	mux.Handle("GET /api/repositories", s.loopback(s.handleAPIRepositories))
	mux.Handle("DELETE /api/connections/github", s.loopback(s.handleDisconnectGitHub))

	// Drive OAuth and connection management.
	mux.Handle("GET /api/auth/drive", s.loopback(s.handleDriveLogin))
	mux.Handle("GET /api/auth/drive/callback", s.loopback(s.handleDriveCallback))
	mux.Handle("DELETE /api/connections/drive", s.loopback(s.handleDisconnectDrive))

	// Legacy protection endpoints, retained so existing tooling keeps working.
	// The dashboard no longer uses the protect/protected model.
	mux.Handle("GET /api/protected-repositories", s.loopback(s.handleListProtectedRepositories))
	mux.Handle("POST /api/protected-repositories", s.loopback(s.handleProtectRepositories))
	mux.Handle("DELETE /api/protected-repositories/{id}", s.loopback(s.handleRemoveProtectedRepository))

	// Direct backup flow: any GitHub repository can be backed up without being
	// protected first, and "back up all" sweeps every repository directly.
	mux.Handle("POST /api/repositories/{githubId}/backup", s.loopback(s.handleBackupRepo))
	mux.Handle("POST /api/backups", s.loopback(s.handleBackupAll))

	// Phase 3: backup execution and job/history inspection.
	mux.Handle("POST /api/protected-repositories/{id}/backup", s.loopback(s.handleBackupProtectedRepo))
	mux.Handle("GET /api/backup-jobs/{id}", s.loopback(s.handleProtectedBackupJob))
	mux.Handle("GET /api/protected-repositories/{id}/backup-jobs", s.loopback(s.handleProtectedBackupJobs))
	mux.Handle("GET /api/protected-repositories/{id}/backups", s.loopback(s.handleProtectedBackupHistory))

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
