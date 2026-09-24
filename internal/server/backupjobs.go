package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	"github.com/b-isry/gitsafe/internal/archiver"
	"github.com/b-isry/gitsafe/internal/providers"
	"github.com/b-isry/gitsafe/internal/state"
	"github.com/google/uuid"
)

// bundleOutcome is the result of producing a git bundle for a protected
// repository. It is deliberately free of any clone URL, token, or local repo
// path other than the bundle file itself.
type bundleOutcome struct {
	BundlePath string
	BundleName string
	SizeBytes  int64
	SHA256     string
}

// bundleBackupFunc produces the on-disk git bundle for a protected repository.
// fullName is the owner/name; token is the live OAuth token (never persisted).
// outputPath is where the bundle file is written.
type bundleBackupFunc func(ctx context.Context, fullName, token, outputPath string) (bundleOutcome, error)

// defaultBundleBackup mirrors a protected GitHub repository and writes a git
// bundle to outputPath. The clone URL is built on the server from the validated
// fullName; the token is passed via git's environment config (never in URL/argv).
func defaultBundleBackup(ctx context.Context, fullName, token, outputPath string) (bundleOutcome, error) {
	bundlePath, err := archiver.BundleRemoteRepo(ctx, fullName, token, outputPath)
	if err != nil {
		return bundleOutcome{}, err
	}

	info, err := os.Stat(bundlePath)
	if err != nil {
		return bundleOutcome{}, fmt.Errorf("stat bundle: %w", err)
	}
	sum, err := sha256File(bundlePath)
	if err != nil {
		return bundleOutcome{}, fmt.Errorf("hash bundle: %w", err)
	}
	return bundleOutcome{
		BundlePath: bundlePath,
		BundleName: filepath.Base(bundlePath),
		SizeBytes:  info.Size(),
		SHA256:     sum,
	}, nil
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// driveUploadFunc uploads a completed local bundle to the connected Google
// Drive account and returns the created Drive file ID. folderID is the GitSafe
// storage folder in the connected account; driveToken is the stored OAuth
// refresh token (never persisted to state). Real byte progress is reported
// through onProgress (nil is allowed). It keeps the cloud package out of the
// job orchestration so tests can stub the network/Google work.
type driveUploadFunc func(ctx context.Context, bundlePath, folderID, driveToken string, onProgress func(now, total int64), logger *slog.Logger) (string, error)

// repoSizeFunc returns the size of a repository in MB. It is a function field
// so tests can stub the GitHub API call without network access.
// The token is obtained from the per-user token store.
type repoSizeFunc func(ctx context.Context, fullName string) (int, error)

// sanitizeMessage strips credentials from a message before it is persisted or
// returned to the client. It guards against any code path leaking a tokenized
// URL (e.g. an auth token embedded in an error), independent of upstream
// redaction.
func sanitizeMessage(msg string) string {
	return tokenURLPattern.ReplaceAllString(msg, "$1")
}

// tokenURLPattern matches a URL userinfo (e.g. https://user:pass@) so it can be
// removed, leaving the scheme://host intact. Compiled once; safe for concurrent
// use by the async backup goroutine.
var tokenURLPattern = regexp.MustCompile(`(?i)([a-z][a-z0-9+.\-]*://)[^@\s]*@`)

// startProtectedBackup enqueues a backup job for the given protected repository
// and runs it detached from any HTTP request. It returns the created job, or an
// error if the repository is unknown or already being backed up.
func (s *Server) startProtectedBackup(userID int64, stateStore StateStore, tokenStore TokenStore, protectedRepoID string) (state.BackupJob, error) {
	repo, ok := stateStore.ProtectedRepo(protectedRepoID)
	if !ok {
		return state.BackupJob{}, errProtectedRepoNotFound
	}

	s.jobMu.Lock()
	defer s.jobMu.Unlock()
	for _, j := range stateStore.UnfinishedJobs() {
		if j.ProtectedRepoID == protectedRepoID {
			return state.BackupJob{}, errJobInFlight
		}
	}

	// Drive is the only valid backup destination. Fail at request time with a
	// clear message when no Google Drive account is connected.
	if !s.driveConnected(stateStore) {
		return state.BackupJob{}, errDriveNotConnected
	}

	now := time.Now()
	job := state.BackupJob{
		ID:              uuid.NewString(),
		ProtectedRepoID: repo.ID,
		FullName:        repo.FullName,
		State:           state.JobEnqueued,
		StartedAt:       now,
	}
	stateStore.CreateBackupJob(job)
	if err := s.saveState(stateStore); err != nil {
		// Persisting the freshly-created job failed. Remove it so it does not
		// linger as an orphaned non-terminal job that blocks future backups of
		// this repository (409) until a restart.
		if rerr := stateStore.RemoveBackupJob(job.ID); rerr != nil {
			s.logger.Warn("cleanup orphaned job after save failure", "job", job.ID, "error", rerr)
		}
		return state.BackupJob{}, err
	}
	go func() {
		// A global semaphore caps how many repository mirrors run at once; extra
		// jobs wait here in the enqueued state while slots free up. Per-repo
		// serialization is unchanged (jobMu guarantees one job per repo).
		s.backupSem <- struct{}{}
		defer func() { <-s.backupSem }()
		s.runProtectedBackup(userID, stateStore, tokenStore, job.ID, repo)
	}()
	return job, nil
}

// maxConcurrentBackups caps how many protected-repo mirrors run simultaneously.
// Backups beyond this wait in the enqueued state, preserving per-repo ordering.
const maxConcurrentBackups = 4

var (
	errProtectedRepoNotFound = fmt.Errorf("protected repository not found")
	errJobInFlight           = fmt.Errorf("a backup for this repository is already running")
	errUserBackupInFlight    = fmt.Errorf("a backup is already running for this user")
	errRepoTooLarge          = fmt.Errorf("repository exceeds maximum allowed size")
	errMaxProtectedRepos     = fmt.Errorf("maximum number of protected repositories reached")
)

// runProtectedBackup walks a single backup job through its lifecycle, persisting
// every transition. A background context is used so the job survives the client
// disconnecting after the 202 response.
func (s *Server) runProtectedBackup(userID int64, stateStore StateStore, tokenStore TokenStore, jobID string, repo state.ProtectedRepo) {
	bg := s.logger

	getJob := func() state.BackupJob {
		j, _ := stateStore.BackupJob(jobID)
		return j
	}
	setState := func(st string) {
		j := getJob()
		j.State = st
		if err := stateStore.UpdateBackupJob(j); err != nil {
			bg.Warn("update backup job state", "job", jobID, "state", st, "error", err)
			return
		}
		if err := s.saveState(stateStore); err != nil {
			bg.Warn("persist backup job state", "job", jobID, "state", st, "error", err)
		}
	}

	// Get GitHub connection and token
	conn, ok := stateStore.GitHubConnection()
	if !ok {
		s.finishProtectedJob(stateStore, jobID, repo, "GitHub is not connected")
		return
	}
	token, err := tokenStore.Get(conn.TokenRef)
	if err != nil {
		s.finishProtectedJob(stateStore, jobID, repo, "GitHub access token is unavailable")
		return
	}

	// Google Drive is the only valid backup destination
	driveConn, ok := s.driveConnection(stateStore)
	if !ok {
		s.finishProtectedJob(stateStore, jobID, repo, errDriveNotConnected.Error()+". Connect your Google account and try again.")
		return
	}
	refreshToken, err := tokenStore.Get(driveConn.TokenRef)
	if err != nil || refreshToken == "" {
		s.finishProtectedJob(stateStore, jobID, repo, "Google Drive access token is unavailable. Reconnect your account and try again.")
		return
	}

	// Acquire per-user backup slot (unless disabled for testing)
	if !s.disablePerUserBackupLimit {
		s.userBackupMu.Lock()
		if s.userBackups[userID] > 0 {
			s.userBackupMu.Unlock()
			s.finishProtectedJob(stateStore, jobID, repo, errUserBackupInFlight.Error())
			return
		}
		s.userBackups[userID]++
		s.userBackupMu.Unlock()

		// Defer releasing the user backup slot
		defer func() {
			s.userBackupMu.Lock()
			if s.userBackups[userID] > 0 {
				s.userBackups[userID]--
			}
			if s.userBackups[userID] == 0 {
				delete(s.userBackups, userID)
			}
			s.userBackupMu.Unlock()
		}()
	}

	// Create timeout context for clone+bundle phase
	timeoutMinutes := s.app.Config.BackupTimeoutMinutes
	if timeoutMinutes <= 0 {
		timeoutMinutes = 20
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutMinutes)*time.Minute)
	defer cancel()

	// Check repo size before cloning (use a short timeout for the API call)
	sizeCtx, sizeCancel := context.WithTimeout(ctx, 30*time.Second)
	repoSizeMB, err := s.checkRepoSize(sizeCtx, tokenStore, conn.TokenRef, repo.FullName)
	sizeCancel()
	if err != nil {
		bg.Error("failed to check repo size", "job", jobID, "repo", repo.FullName, "error", err)
		s.finishProtectedJob(stateStore, jobID, repo, "failed to check repository size: "+err.Error())
		return
	}
	maxRepoMB := s.app.Config.MaxRepoMB
	if maxRepoMB <= 0 {
		maxRepoMB = 500
	}
	if repoSizeMB > maxRepoMB {
		bg.Warn("repository too large", "job", jobID, "repo", repo.FullName, "sizeMB", repoSizeMB, "maxMB", maxRepoMB)
		s.finishProtectedJob(stateStore, jobID, repo, fmt.Sprintf("repository size (%d MB) exceeds maximum allowed (%d MB)", repoSizeMB, maxRepoMB))
		return
	}

	// Stage the bundle in a temporary directory
	staging, err := os.MkdirTemp("", "gitsafe-staging-*")
	if err != nil {
		s.finishProtectedJob(stateStore, jobID, repo, "could not create a temporary staging directory")
		return
	}
	defer os.RemoveAll(staging)

	setState(state.JobCloning)
	outcome, err := s.bundleBackup(ctx, repo.FullName, token, staging)
	if err != nil {
		bg.Error("protected backup failed", "job", jobID, "repo", repo.FullName, "error", err)
		s.finishProtectedJob(stateStore, jobID, repo, "backup failed: "+err.Error())
		return
	}

	setState(state.JobUploading)
	s.setJobProgress(jobID, jobProgressMetrics{UploadedBytes: 0, TotalBytes: outcome.SizeBytes})
	driveID, err := s.driveUpload(ctx, outcome.BundlePath, driveConn.StorageFolderID, refreshToken, func(now, total int64) {
		s.setJobProgress(jobID, jobProgressMetrics{UploadedBytes: now, TotalBytes: total})
	}, s.logger)
	s.clearJobProgress(jobID)
	if err != nil {
		bg.Error("drive upload failed", "job", jobID, "repo", repo.FullName, "bundle", outcome.BundleName, "error", err)
		_ = os.Remove(outcome.BundlePath)
		s.finishProtectedJob(stateStore, jobID, repo, "drive upload failed: "+err.Error())
		return
	}

	if err := os.Remove(outcome.BundlePath); err != nil {
		bg.Warn("remove temporary bundle after upload", "job", jobID, "bundle", outcome.BundlePath, "error", err)
	}

	setState(state.JobRecording)
	record := state.BackupRecord{
		ID:              uuid.NewString(),
		ProtectedRepoID: repo.ID,
		FullName:        repo.FullName,
		CreatedAt:       time.Now(),
		MirroredAt:      time.Now(),
		DefaultBranch:   repo.DefaultBranch,
		BundleName:      outcome.BundleName,
		BundleSize:      outcome.SizeBytes,
		BundleSHA256:    outcome.SHA256,
		DriveFileID:     driveID,
		Status:          state.BackupStatusUploaded,
	}
	stateStore.AddBackupRecord(record)

	j := getJob()
	j.State = state.JobCompleted
	j.FinishedAt = time.Now()
	if err := stateStore.UpdateBackupJob(j); err != nil {
		bg.Warn("complete backup job", "job", jobID, "error", err)
	}
	if err := s.saveState(stateStore); err != nil {
		bg.Warn("persist completed backup job", "job", jobID, "error", err)
	}
	bg.Info("protected backup completed", "job", jobID, "repo", repo.FullName, "bundle", outcome.BundleName, "driveFileID", driveID)
}

// finishProtectedJob marks a job failed with a sanitized message and persists it.
func (s *Server) finishProtectedJob(stateStore StateStore, jobID string, repo state.ProtectedRepo, message string) {
	j, ok := stateStore.BackupJob(jobID)
	if !ok {
		return
	}
	message = sanitizeMessage(message)
	j.State = state.JobFailed
	j.FinishedAt = time.Now()
	j.Error = message
	if err := stateStore.UpdateBackupJob(j); err != nil {
		s.logger.Warn("fail backup job", "job", jobID, "error", err)
	}
	if err := s.saveState(stateStore); err != nil {
		s.logger.Warn("persist failed backup job", "job", jobID, "error", err)
	}
}

// checkRepoSize queries the GitHub API for the repository size in MB.
// Returns the size in MB, or an error if the repository cannot be found.
func (s *Server) checkRepoSize(ctx context.Context, tokenStore TokenStore, tokenRef, fullName string) (int, error) {
	if s.repoSize != nil {
		return s.repoSize(ctx, fullName)
	}
	// Get the GitHub token from the per-user token store using the reference
	// recorded on the connection (a per-user ref such as "github.42").
	token, err := tokenStore.Get(tokenRef)
	if err != nil {
		return 0, err
	}
	client := s.githubClientFor(token)
	repo, err := client.Repository(ctx, fullName)
	if err != nil {
		return 0, err
	}
	// GitHub API returns size in KB
	sizeKB := repo.SizeKB
	if sizeKB <= 0 {
		return 0, nil
	}
	return int((sizeKB + 1023) / 1024), nil
}

// --- handlers ---

// handleBackupProtectedRepo triggers a backup for a single protected repository.
func (s *Server) handleBackupProtectedRepo(w http.ResponseWriter, r *http.Request) {
	userID, stateStore, tokenStore, sess, err := s.requireUser(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "No active session. Refresh the page and try again."})
		return
	}
	if !s.cloudReady(w, userID, stateStore, tokenStore) {
		return
	}
	if !validateCSRF(sess, r.Header.Get("X-CSRF-Token")) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "CSRF validation failed."})
		return
	}

	id := r.PathValue("id")
	job, err := s.startProtectedBackup(userID, stateStore, tokenStore, id)
	switch {
	case err == errProtectedRepoNotFound:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "protected repository not found"})
		return
	case err == errJobInFlight:
		writeJSON(w, http.StatusConflict, map[string]string{"error": "A backup for this repository is already running."})
		return
	case err == errDriveNotConnected:
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "Google Drive is not connected. Connect your Google account before backing up.",
		})
		return
	case err != nil:
		s.logger.Error("start protected backup", "cause", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not start the backup"})
		return
	}
	writeJSON(w, http.StatusAccepted, s.toJobView(job))
}

// handleBackupRepo triggers a backup for a single GitHub repository addressed by
// its GitHub numeric id, without requiring the user to protect it first. It
// resolves the repository against the live discovered set and materializes the
// backup engine's internal per-repository record on demand, then delegates to
// the existing backup machinery (startProtectedBackup) unchanged.
func (s *Server) handleBackupRepo(w http.ResponseWriter, r *http.Request) {
	userID, stateStore, tokenStore, sess, err := s.requireUser(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "No active session. Refresh the page and try again."})
		return
	}
	if !s.cloudReady(w, userID, stateStore, tokenStore) {
		return
	}
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "No active session. Refresh the page and try again."})
		return
	}
	if !validateCSRF(sess, r.Header.Get("X-CSRF-Token")) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "CSRF validation failed."})
		return
	}

	githubID, err := strconv.ParseInt(r.PathValue("githubId"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid repository id."})
		return
	}

	repos, err := s.discoverRepositories(r.Context(), stateStore, tokenStore)
	if err != nil {
		s.writeDiscoveryError(w, err)
		return
	}
	var found *providers.Repository
	for i := range repos {
		if repos[i].ID == githubID {
			found = &repos[i]
			break
		}
	}
	if found == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "repository not found on GitHub"})
		return
	}

	internalRepo, err := s.findOrCreateInternalRepo(stateStore, found.ID, found.FullName, found.DefaultBranch)
	if err != nil {
		s.logger.Warn("ensure internal repo record", "repo", found.FullName, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not start the backup"})
		return
	}

	job, err := s.startProtectedBackup(userID, stateStore, tokenStore, internalRepo.ID)
	switch {
	case err == errJobInFlight:
		writeJSON(w, http.StatusConflict, map[string]string{"error": "A backup for this repository is already running."})
		return
	case err == errDriveNotConnected:
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "Google Drive is not connected. Connect your Google account before backing up.",
		})
		return
	case err != nil:
		s.logger.Warn("start backup", "repo", found.FullName, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not start the backup"})
		return
	}
	writeJSON(w, http.StatusAccepted, s.toJobView(job))
}

// handleBackupAll starts backups for the user's GitHub repositories directly.
// Nothing has to be protected first: every repository is eligible, matching the
// existing backup engine (each starts unless a backup is already running for
// it). Drive connectivity is required, since the engine only writes to Drive;
// in-flight repositories are skipped, not treated as errors.
func (s *Server) handleBackupAll(w http.ResponseWriter, r *http.Request) {
	userID, stateStore, tokenStore, sess, err := s.requireUser(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "No active session. Refresh the page and try again."})
		return
	}
	if !s.cloudReady(w, userID, stateStore, tokenStore) {
		return
	}
	if !validateCSRF(sess, r.Header.Get("X-CSRF-Token")) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "CSRF validation failed."})
		return
	}
	if !s.driveConnected(stateStore) {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "Google Drive is not connected. Connect your Google account before backing up.",
		})
		return
	}

	repos, err := s.discoverRepositories(r.Context(), stateStore, tokenStore)
	if err != nil {
		s.writeDiscoveryError(w, err)
		return
	}

	var started []jobView
	for _, repo := range repos {
		internalRepo, err := s.findOrCreateInternalRepo(stateStore, repo.ID, repo.FullName, repo.DefaultBranch)
		if err != nil {
			s.logger.Warn("ensure internal repo record (backup all)", "repo", repo.FullName, "error", err)
			continue
		}
		job, err := s.startProtectedBackup(userID, stateStore, tokenStore, internalRepo.ID)
		if err == errJobInFlight {
			continue // already running; skip rather than fail the sweep
		}
		if err != nil {
			s.logger.Warn("start backup (backup all)", "repo", repo.FullName, "error", err)
			continue
		}
		started = append(started, s.toJobView(job))
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"jobs": started})
}

// handleProtectedBackupJob returns a single state-backed backup job by id.
func (s *Server) handleProtectedBackupJob(w http.ResponseWriter, r *http.Request) {
	_, stateStore, _, _, err := s.requireUser(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "No active session. Refresh the page and try again."})
		return
	}
	if s.github == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "GitHub integration is not configured."})
		return
	}
	id := r.PathValue("id")
	job, ok := stateStore.BackupJob(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "backup job not found"})
		return
	}
	writeJSON(w, http.StatusOK, s.toJobView(job))
}

// handleProtectedBackupJobs lists the backup jobs for a protected repository.
func (s *Server) handleProtectedBackupJobs(w http.ResponseWriter, r *http.Request) {
	_, stateStore, _, _, err := s.requireUser(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "No active session. Refresh the page and try again."})
		return
	}
	if s.github == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "GitHub integration is not configured."})
		return
	}
	id := r.PathValue("id")
	if _, ok := stateStore.ProtectedRepo(id); !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "protected repository not found"})
		return
	}
	jobs := stateStore.BackupJobs()
	var out []state.BackupJob
	for _, j := range jobs {
		if j.ProtectedRepoID == id {
			out = append(out, j)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": s.jobsView(out)})
}

// handleProtectedBackupHistory lists the immutable backup records for a
// protected repository.
func (s *Server) handleProtectedBackupHistory(w http.ResponseWriter, r *http.Request) {
	_, stateStore, _, _, err := s.requireUser(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "No active session. Refresh the page and try again."})
		return
	}
	if s.github == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "GitHub integration is not configured."})
		return
	}
	id := r.PathValue("id")
	if _, ok := stateStore.ProtectedRepo(id); !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "protected repository not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"backups": backupsView(stateStore.BackupRecordsForRepo(id))})
}

// --- response shaping ---

// jobProgressMetrics is the live upload byte progress for a single backup job,
// kept in memory only (never persisted) and merged into the job API view.
type jobProgressMetrics struct {
	UploadedBytes int64
	TotalBytes    int64
}

func (s *Server) setJobProgress(id string, m jobProgressMetrics) {
	s.jobProgressMu.Lock()
	defer s.jobProgressMu.Unlock()
	s.jobProgress[id] = m
}

// jobProgressFor returns the live upload progress for a job, if any.
func (s *Server) jobProgressFor(id string) (jobProgressMetrics, bool) {
	s.jobProgressMu.Lock()
	defer s.jobProgressMu.Unlock()
	m, ok := s.jobProgress[id]
	return m, ok
}

func (s *Server) clearJobProgress(id string) {
	s.jobProgressMu.Lock()
	defer s.jobProgressMu.Unlock()
	delete(s.jobProgress, id)
}

// jobView is the JSON shape of a backup job exposed to the UI.
type jobView struct {
	ID              string `json:"id"`
	ProtectedRepoID string `json:"protectedRepoId"`
	FullName        string `json:"fullName"`
	State           string `json:"state"`
	StartedAt       string `json:"startedAt"`
	FinishedAt      string `json:"finishedAt,omitempty"`
	Error           string `json:"error,omitempty"`
	// UploadedBytes/TotalBytes/Progress are live-only upload progress (see
	// jobProgressMetrics); they are omitted for non-running jobs.
	UploadedBytes int64 `json:"uploadedBytes,omitempty"`
	TotalBytes    int64 `json:"totalBytes,omitempty"`
	Progress      int   `json:"progress,omitempty"`
}

func (s *Server) toJobView(j state.BackupJob) jobView {
	v := jobView{
		ID:              j.ID,
		ProtectedRepoID: j.ProtectedRepoID,
		FullName:        j.FullName,
		State:           j.State,
		StartedAt:       formatTime(j.StartedAt),
		Error:           j.Error,
	}
	if !j.FinishedAt.IsZero() {
		v.FinishedAt = j.FinishedAt.Format(time.RFC3339)
	}
	if m, ok := s.jobProgressFor(j.ID); ok {
		v.UploadedBytes = m.UploadedBytes
		v.TotalBytes = m.TotalBytes
		if m.TotalBytes > 0 {
			pct := m.UploadedBytes * 100 / m.TotalBytes
			if pct < 0 {
				pct = 0
			}
			if pct > 100 {
				pct = 100
			}
			v.Progress = int(pct)
		}
	}
	return v
}

func (s *Server) jobsView(in []state.BackupJob) []jobView {
	out := make([]jobView, 0, len(in))
	for _, j := range in {
		out = append(out, s.toJobView(j))
	}
	return out
}

// backupView is the JSON shape of an immutable backup record.
type backupView struct {
	ID              string `json:"id"`
	ProtectedRepoID string `json:"protectedRepoId"`
	FullName        string `json:"fullName"`
	CreatedAt       string `json:"createdAt"`
	DefaultBranch   string `json:"defaultBranch"`
	BundleName      string `json:"bundleName"`
	BundleSize      int64  `json:"bundleSize"`
	BundleSHA256    string `json:"bundleSHA256"`
	DriveFileID     string `json:"driveFileId,omitempty"`
	Status          string `json:"status"`
}

func backupsView(in []state.BackupRecord) []backupView {
	out := make([]backupView, 0, len(in))
	for _, r := range in {
		out = append(out, backupView{
			ID:              r.ID,
			ProtectedRepoID: r.ProtectedRepoID,
			FullName:        r.FullName,
			CreatedAt:       formatTime(r.CreatedAt),
			DefaultBranch:   r.DefaultBranch,
			BundleName:      r.BundleName,
			BundleSize:      r.BundleSize,
			BundleSHA256:    r.BundleSHA256,
			DriveFileID:     r.DriveFileID,
			Status:          r.Status,
		})
	}
	return out
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}
