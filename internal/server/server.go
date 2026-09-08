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
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/b-isry/gitsafe/internal/backup"
	"github.com/b-isry/gitsafe/internal/config"
	"github.com/b-isry/gitsafe/internal/history"
	"github.com/b-isry/gitsafe/internal/notify"
	"github.com/b-isry/gitsafe/internal/providers"
	"github.com/b-isry/gitsafe/internal/retention"
	"github.com/b-isry/gitsafe/internal/schedule"
	"github.com/google/uuid"
)

//go:embed templates static
var content embed.FS

type Server struct {
	tmpls      map[string]*template.Template
	logger     *slog.Logger
	app        *App
	store      BackupStore
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

	// bundleBackup creates a git bundle for a protected GitHub repository. It is
	// a function field so tests can stub the git/network work without real
	// network access; the production default calls archiver.BundleRemoteRepo.
	bundleBackup bundleBackupFunc

	// driveUpload uploads a completed local bundle to Google Drive and returns
	// the resulting Drive file ID. It is a function field so tests can stub the
	// Google work without real credentials or network access; the production
	// default calls cloud.UploadFile. It is only invoked when Drive is enabled.
	driveUpload driveUploadFunc

	// driveDelete deletes a Drive file by ID (used by retention cleanup) and is a
	// function field so tests can stub the Google work. Only IDs recorded in
	// BackupRecord.DriveFileID are ever passed. The production default calls
	// cloud.DeleteFile and only reports success when Drive confirms it.
	driveDelete driveDeleteFunc

	// jobMu serializes protected-backup job creation so a repository is never
	// given two concurrent jobs.
	jobMu sync.Mutex

	// backupSem is a global semaphore capping how many protected-repo mirrors
	// can execute at once, so a large queue of backup requests cannot spawn an
	// unbounded number of concurrent git clones/uploads.
	backupSem chan struct{}

	// cleanupMu serializes manual retention-cleanup runs so two triggers cannot
	// execute destructively at the same time. It never blocks normal backup
	// operations.
	cleanupMu sync.Mutex

	// lastCleanup holds the most recent manual cleanup result for the retention
	// UI, guarded by cleanupMu.
	lastCleanup retention.Result

	// schedMu guards the running cleanup scheduler (Phase 7). Replaced on every
	// configuration change rather than mutated in place.
	schedMu sync.Mutex
	sched   *schedule.Scheduler
	// schedClock lets tests drive the scheduler deterministically. nil means the
	// scheduler uses the wall clock.
	schedClock schedule.Clock

	// statusMu guards cleanupStatus so a status read never blocks on a running
	// cleanup.
	statusMu      sync.Mutex
	cleanupStatus cleanupStatus

	// cleanupNotifier receives a notification after each completed cleanup run.
	// A notification failure never fails the cleanup itself.
	cleanupNotifier CleanupNotifier

	// notifyWebhook is the webhook delivery engine used for cleanup
	// notifications and the manual "test notification" endpoint. nil means
	// webhook delivery is unavailable.
	notifyWebhook *notify.Notifier

	// historyStore persists completed cleanup runs (newest-first, bounded by the
	// configured limit). nil disables history recording.
	historyStore *history.Store
}

type pageData struct {
	Active    string
	PageTitle string
}

type renderData struct {
	pageData
	Dashboard  Dashboard
	Repos      []Repository
	Filter     string
	Detail     RepositoryDetail
	BackupRows []BackupRow
	Settings   SettingsView
	Scanning   bool
	Skipped    []SkippedRepo
	Cloud      CloudView
}

var pages = []string{"dashboard", "repositories", "repository", "backups", "settings", cloudRepositoriesPage}

// ConfigPathFile is the fallback location used when New is given an empty path
// (for example in tests where persistence is not exercised).
const ConfigPathFile = "config.yaml"

func New(logger *slog.Logger, app *App, store BackupStore, configPath string) (*Server, error) {
	funcs := template.FuncMap{
		"humanSize":   humanSize,
		"statusLabel": statusLabel,
		"repoStatus":  repoStatus,
	}

	tmpls := make(map[string]*template.Template, len(pages)+1)
	for _, p := range pages {
		t, err := template.New("gitsafe").Funcs(funcs).ParseFS(
			content, "templates/layout.html", "templates/"+p+".html")
		if err != nil {
			return nil, err
		}
		tmpls[p] = t
	}
	// The first-run setup page is a standalone document without the main
	// sidebar/topbar layout, so it is parsed on its own.
	setupT, err := template.New("setup").Funcs(funcs).ParseFS(content, "templates/setup.html")
	if err != nil {
		return nil, err
	}
	tmpls["setup"] = setupT
	if configPath == "" {
		configPath = ConfigPathFile
	}
	srv := &Server{
		tmpls: tmpls, logger: logger, app: app, store: store,
		configPath:   configPath,
		sessions:     newSessionManager(),
		bundleBackup: defaultBundleBackup,
		driveUpload:  defaultDriveUpload,
		driveDelete:  defaultDriveDelete,
		backupSem:    make(chan struct{}, maxConcurrentBackups),
	}
	webhook := notify.New(
		func() config.NotificationConfig { return srv.app.ConfigSnapshot().Notifications },
		logger,
	)
	srv.notifyWebhook = webhook
	srv.cleanupNotifier = notifyCleanupNotifier{
		logger:  logger,
		webhook: webhook,
		status:  func() cleanupStatus { return srv.snapshotStatus() },
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
	mux.HandleFunc("/", s.handleDashboard)
	mux.HandleFunc("/repositories", s.handleRepositories)
	mux.HandleFunc("/repositories/{ref}", s.handleRepositoryDetail)
	mux.HandleFunc("/backups", s.handleBackups)
	mux.HandleFunc("POST /backups", s.handleCreateBackup)
	mux.HandleFunc("GET /backups/{id}", s.handleBackupJob)
	mux.HandleFunc("GET /settings", s.handleSettings)
	mux.HandleFunc("POST /settings", s.handleSaveSettings)
	mux.HandleFunc("GET /setup", s.handleSetup)
	mux.HandleFunc("POST /setup", s.handleSetupSubmit)
	mux.Handle("GET /api/folders", s.loopback(s.handleListFolders))
	mux.Handle("GET /api/status", s.loopback(s.handleScanStatus))
	mux.Handle("POST /api/scan", s.loopback(s.handleScanNow))

	// Phase 1: GitHub OAuth and cloud connections.
	mux.Handle("GET /api/csrf", s.loopback(s.handleCSRF))
	mux.Handle("GET /api/auth/github", s.loopback(s.handleGitHubLogin))
	mux.Handle("GET /api/auth/github/callback", s.loopback(s.handleGitHubCallback))
	mux.Handle("GET /api/connections", s.loopback(s.handleAPIConnections))
	mux.Handle("GET /api/repositories", s.loopback(s.handleAPIRepositories))
	mux.Handle("DELETE /api/connections/github", s.loopback(s.handleDisconnectGitHub))

	// Phase 2: GitHub repository discovery + protection/selection.
	mux.HandleFunc("GET /cloud-repositories", s.handleCloudRepositories)
	mux.Handle("GET /api/protected-repositories", s.loopback(s.handleListProtectedRepositories))
	mux.Handle("POST /api/protected-repositories", s.loopback(s.handleProtectRepositories))
	mux.Handle("DELETE /api/protected-repositories/{id}", s.loopback(s.handleRemoveProtectedRepository))

	// Phase 3: backup execution and job/history inspection.
	mux.Handle("POST /api/protected-repositories/{id}/backup", s.loopback(s.handleBackupProtectedRepo))
	mux.Handle("GET /api/backup-jobs/{id}", s.loopback(s.handleProtectedBackupJob))
	mux.Handle("GET /api/protected-repositories/{id}/backup-jobs", s.loopback(s.handleProtectedBackupJobs))
	mux.Handle("GET /api/protected-repositories/{id}/backups", s.loopback(s.handleProtectedBackupHistory))

	// Phase 6: retention configuration and manual cleanup.
	mux.Handle("GET /api/retention", s.loopback(s.handleRetentionGet))
	mux.Handle("PUT /api/retention", s.loopback(s.handleRetentionPut))
	mux.Handle("POST /api/retention/cleanup", s.loopback(s.handleRetentionCleanup))
	mux.Handle("POST /api/retention/notify-test", s.loopback(s.handleRetentionNotifyTest))

	return s.requireRoot(mux)
}

// loopback restricts an API endpoint to local clients. Directory browsing and
// scan control must never be reachable from the network; the web server itself
// is a local machine tool.
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

// requireRoot is the first-run gate. Until the user has chosen a root directory
// the application has nothing to scan, so every page is redirected to the setup
// screen. Static assets and the setup flow itself stay reachable.
func (s *Server) requireRoot(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.app.Config.RootPath == "" &&
			r.URL.Path != "/setup" &&
			!strings.HasPrefix(r.URL.Path, "/setup/") &&
			!strings.HasPrefix(r.URL.Path, "/static/") &&
			!strings.HasPrefix(r.URL.Path, "/api/") {
			http.Redirect(w, r, "/setup", http.StatusFound)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// --- pages ---

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	repos, backups := s.loadAll(r)
	dash := buildDashboard(repos, backups, s.app.Threshold, s.app.Config.RootPath)
	scanning, lastScan, _ := s.app.ScanStatus()
	if !scanning && lastScan.IsZero() && len(repos) == 0 {
		// No snapshot yet (first scan in flight or fresh start): surface the
		// scanning state so the page reads "Scanning repositories…" instead of
		// a misleading empty list. A completed scan that found zero repositories
		// has a non-zero lastScan and correctly shows the empty state.
		scanning = true
	}
	s.render(w, "dashboard", renderData{
		pageData:  pageData{Active: "dashboard", PageTitle: "Dashboard"},
		Dashboard: dash,
		Scanning:  scanning,
		Skipped:   s.app.SkippedRepos(),
	})
}

func (s *Server) handleRepositories(w http.ResponseWriter, r *http.Request) {
	repos, backups := s.loadAll(r)
	filter := r.URL.Query().Get("filter")
	view := toRepositoryViews(repos, backups)
	if filter != "" {
		view = filterRepositories(view, filter)
	}
	s.render(w, "repositories", renderData{
		pageData: pageData{Active: "repositories", PageTitle: "Repositories"},
		Repos:    view,
		Filter:   filter,
	})
}

func (s *Server) handleRepositoryDetail(w http.ResponseWriter, r *http.Request) {
	repos, backups := s.loadAll(r)
	ref := r.PathValue("ref")
	var repo RepositoryStatus
	found := false
	for _, rp := range repos {
		if pathID(rp.Path) == ref {
			repo = rp
			found = true
			break
		}
	}
	if !found {
		http.NotFound(w, r)
		return
	}

	var repoBackups []BackupRow
	for _, b := range backups {
		if b.RepoName == repo.Name {
			repoBackups = append(repoBackups, BackupRow{
				Repo:      repo.Name,
				Filename:  b.Filename,
				SizeBytes: b.SizeBytes,
				Created:   b.Created,
			})
		}
	}

	s.render(w, "repository", renderData{
		pageData: pageData{Active: "repositories", PageTitle: repo.Name},
		Detail: RepositoryDetail{
			Repository: toRepositoryView(repo, repoBackups),
			Threshold:  s.app.Threshold,
			Backups:    repoBackups,
		},
	})
}

func (s *Server) handleBackups(w http.ResponseWriter, r *http.Request) {
	_, backups := s.loadAll(r)
	var rows []BackupRow
	for _, b := range backups {
		rows = append(rows, BackupRow{
			Repo:      b.RepoName,
			Filename:  b.Filename,
			SizeBytes: b.SizeBytes,
			Created:   b.Created,
		})
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Created.After(rows[j].Created) })
	s.render(w, "backups", renderData{
		pageData:   pageData{Active: "backups", PageTitle: "Backups"},
		BackupRows: rows,
	})
}

// --- backup API ---

// backupRequest accepts either a single repoPath (repository detail page) or a
// list of repoPaths (dashboard selection).
type backupRequest struct {
	RepoPath  string   `json:"repoPath"`
	RepoPaths []string `json:"repoPaths"`
}

type jobResponse struct {
	ID       string `json:"id"`
	RepoPath string `json:"repoPath"`
}

func (s *Server) handleCreateBackup(w http.ResponseWriter, r *http.Request) {
	var req backupRequest
	if err := decodeJSONBody(w, r, &req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if len(req.RepoPaths) > 0 {
		if len(req.RepoPaths) > maxBatchBackups {
			http.Error(w, "too many repositories in one request", http.StatusBadRequest)
			return
		}
		s.startBatchBackups(w, req.RepoPaths)
		return
	}
	if req.RepoPath == "" {
		http.Error(w, "repoPath is required", http.StatusBadRequest)
		return
	}

	job, started := s.createJob(req.RepoPath)
	if !started {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error":    "That repository is already being backed up.",
			"repoPath": req.RepoPath,
		})
		return
	}
	writeJSON(w, http.StatusAccepted, jobResponse{ID: job.ID, RepoPath: job.RepoPath})
}

// maxBatchBackups bounds how many local repositories one /backups request can
// start, protecting the v1 backup path from a path-flooding client.
const maxBatchBackups = 200

// startBatchBackups starts one backup per requested repository, skipping any
// that already have a job in flight so a repository is never backed up twice
// concurrently.
func (s *Server) startBatchBackups(w http.ResponseWriter, paths []string) {
	seen := make(map[string]bool, len(paths))
	jobs := make([]jobResponse, 0, len(paths))
	var alreadyRunning []string
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		job, started := s.createJob(p)
		if !started {
			alreadyRunning = append(alreadyRunning, p)
			continue
		}
		jobs = append(jobs, jobResponse{ID: job.ID, RepoPath: job.RepoPath})
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"jobs":           jobs,
		"alreadyRunning": alreadyRunning,
	})
}

func (s *Server) createJob(repoPath string) (*BackupJob, bool) {
	if s.store.ActiveForRepo(repoPath) {
		return nil, false
	}
	id := uuid.NewString()
	job := s.store.Create(id, repoPath)
	go s.runBackup(id, repoPath)
	return job, true
}

// runBackup performs the actual backup detached from any HTTP request. Using a
// background context means the operation survives the client disconnecting the
// moment the 202 response is written — without this the request context is
// canceled as soon as the handler returns and in-flight uploads (and any future
// work) would abort with "context canceled".
func (s *Server) runBackup(id, repoPath string) {
	ctx := context.Background()
	res := s.app.Runner.Run(ctx, backup.Request{
		RepoPath:      repoPath,
		OutputPath:    s.app.OutputPath,
		BackupHistory: s.app.Config.BackupHistory,
		Cloud:         s.app.Config.Cloud,
	}, nil)
	s.store.Update(id, res)
}

// handleScanStatus exposes repository-discovery state for the UI to show
// "Scanning repositories…" and to know when a scan finished.
func (s *Server) handleScanStatus(w http.ResponseWriter, r *http.Request) {
	scanning, lastScan, skipped := s.app.ScanStatus()
	writeJSON(w, http.StatusOK, map[string]any{
		"rootPath": s.app.Config.RootPath,
		"scanning": scanning,
		"lastScan": lastScan,
		"skipped":  skipped,
	})
}

// handleScanNow triggers a detached background re-scan (the "Scan now" action).
func (s *Server) handleScanNow(w http.ResponseWriter, r *http.Request) {
	if s.app.Config.RootPath == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "No root folder configured."})
		return
	}
	s.app.StartRefresh()
	scanning, _, _ := s.app.ScanStatus()
	writeJSON(w, http.StatusOK, map[string]any{"scanning": scanning})
}

func (s *Server) handleBackupJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	job, ok := s.store.Get(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (s *Server) handlePage(active, title string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.render(w, active, renderData{pageData: pageData{Active: active, PageTitle: title}})
	}
}

func (s *Server) render(w http.ResponseWriter, page string, data renderData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpls[page].ExecuteTemplate(w, "layout.html", data); err != nil {
		s.logger.Error("render page", "page", page, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// renderStandalone renders a document that does not use the main layout
// (currently only the first-run setup page). The page template must define a
// block matching its map key.
func (s *Server) renderStandalone(w http.ResponseWriter, page string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpls[page].ExecuteTemplate(w, page, data); err != nil {
		s.logger.Error("render standalone page", "page", page, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// --- helpers ---

func (s *Server) loadAll(r *http.Request) ([]RepositoryStatus, []StoredBackup) {
	repos, err := s.app.Repos(r.Context())
	if err != nil {
		s.logger.Error("discover repositories", "error", err)
		repos = nil
	}
	backups, err := s.app.Backups(r.Context())
	if err != nil {
		s.logger.Error("discover backups", "error", err)
		backups = nil
	}
	return repos, backups
}

func buildDashboard(repos []RepositoryStatus, backups []StoredBackup, threshold int, root string) Dashboard {
	view := toRepositoryViews(repos, backups)
	stale, back, fail := 0, 0, 0
	for _, r := range view {
		if r.Stale {
			stale++
		}
		if r.BackupStatus == "backed-up" {
			back++
		}
		if r.BackupStatus == "failed" {
			fail++
		}
	}
	bundleRows := make([]BackupRow, 0, len(backups))
	for _, b := range backups {
		bundleRows = append(bundleRows, BackupRow{
			Repo:      b.RepoName,
			Filename:  b.Filename,
			SizeBytes: b.SizeBytes,
			Created:   b.Created,
		})
	}
	sort.SliceStable(bundleRows, func(i, j int) bool { return bundleRows[i].Created.After(bundleRows[j].Created) })
	if len(bundleRows) > 6 {
		bundleRows = bundleRows[:6]
	}

	lastScan := time.Time{}
	if len(repos) > 0 {
		lastScan = time.Now()
	}

	return Dashboard{
		Stats: DashboardStats{
			ReposScanned: len(view),
			Stale:        stale,
			Backups:      back,
			Attention:    stale - back + fail,
			LastScan:     lastScan,
		},
		Repos:   view,
		Backups: bundleRows,
	}
}

func toRepositoryViews(repos []RepositoryStatus, backups []StoredBackup) []Repository {
	out := make([]Repository, 0, len(repos))
	for _, r := range repos {
		lb := lastBackupFor(r.Name, backups)
		status := "not-backed-up"
		if lb != nil {
			status = "backed-up"
		}
		out = append(out, Repository{
			ID:           pathID(r.Path),
			Name:         r.Name,
			Path:         r.Path,
			Source:       r.Source,
			LastCommit:   r.LastCommit,
			StaleDays:    r.StaleDays,
			Stale:        r.Stale,
			BackupStatus: status,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func toRepositoryView(r RepositoryStatus, backups []BackupRow) Repository {
	status := "not-backed-up"
	if len(backups) > 0 {
		status = "backed-up"
	}
	return Repository{
		ID:           pathID(r.Path),
		Name:         r.Name,
		Path:         r.Path,
		Source:       r.Source,
		LastCommit:   r.LastCommit,
		StaleDays:    r.StaleDays,
		Stale:        r.Stale,
		BackupStatus: status,
	}
}

func lastBackupFor(name string, backups []StoredBackup) *StoredBackup {
	var best *StoredBackup
	for i := range backups {
		if backups[i].RepoName != name {
			continue
		}
		if best == nil || backups[i].Created.After(best.Created) {
			best = &backups[i]
		}
	}
	return best
}

func filterRepositories(repos []Repository, filter string) []Repository {
	out := make([]Repository, 0, len(repos))
	for _, r := range repos {
		switch filter {
		case "stale":
			if r.Stale {
				out = append(out, r)
			}
		case "backed-up":
			if r.BackupStatus == "backed-up" {
				out = append(out, r)
			}
		case "not-backed-up":
			if r.BackupStatus != "backed-up" {
				out = append(out, r)
			}
		default:
			out = append(out, r)
		}
	}
	return out
}

// pathID is a URL-safe identifier derived from the repository path.
func pathID(p string) string {
	p = filepath.Clean(p)
	p = strings.TrimPrefix(p, string(filepath.Separator))
	p = strings.ReplaceAll(p, string(filepath.Separator), "-")
	p = strings.ReplaceAll(p, ":", "")
	return p
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// maxRequestBody bounds every JSON request body so a damaged or malicious local
// client cannot force unbounded memory allocation. It is generous for every
// current endpoint (settings, retention, protected-repository batches).
const maxRequestBody = 1 << 20 // 1 MiB

// decodeJSONBody decodes r.Body into v through a size-capped reader. It returns
// an error for too-large or malformed bodies; callers map it to a 400 response.
// The max-bytes reader writes its own "request body too large" response and
// disables keep-alive for oversized bodies.
func decodeJSONBody(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	return json.NewDecoder(r.Body).Decode(v)
}

func humanSize(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

func statusLabel(s string) string {
	switch s {
	case "protected":
		return "Protected"
	case "stale":
		return "Stale"
	case "failed":
		return "Failed"
	case "progress":
		return "In progress"
	}
	return strings.Title(s)
}

func repoStatus(r Repository) string {
	if r.BackupStatus == "failed" {
		return "failed"
	}
	if r.Stale {
		return "stale"
	}
	return "protected"
}
