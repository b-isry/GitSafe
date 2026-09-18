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
// bundle to outputPath. The tokenized clone URL is built at runtime and is
// never persisted.
func defaultBundleBackup(ctx context.Context, fullName, token, outputPath string) (bundleOutcome, error) {
	cloneURL := fmt.Sprintf("https://x-access-token:%s@github.com/%s.git", token, fullName)
	bundlePath, err := archiver.BundleRemoteRepo(cloneURL, outputPath)
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
func (s *Server) startProtectedBackup(protectedRepoID string) (state.BackupJob, error) {
	repo, ok := s.stateStore.ProtectedRepo(protectedRepoID)
	if !ok {
		return state.BackupJob{}, errProtectedRepoNotFound
	}

	s.jobMu.Lock()
	defer s.jobMu.Unlock()
	for _, j := range s.stateStore.UnfinishedJobs() {
		if j.ProtectedRepoID == protectedRepoID {
			return state.BackupJob{}, errJobInFlight
		}
	}

	// Drive is the only valid backup destination. Fail at request time with a
	// clear message when no Google Drive account is connected.
	if !s.driveConnected() {
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
	s.stateStore.CreateBackupJob(job)
	if err := s.saveState(); err != nil {
		// Persisting the freshly-created job failed. Remove it so it does not
		// linger as an orphaned non-terminal job that blocks future backups of
		// this repository (409) until a restart.
		if rerr := s.stateStore.RemoveBackupJob(job.ID); rerr != nil {
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
		s.runProtectedBackup(job.ID, repo)
	}()
	return job, nil
}

// maxConcurrentBackups caps how many protected-repo mirrors run simultaneously.
// Backups beyond this wait in the enqueued state, preserving per-repo ordering.
const maxConcurrentBackups = 4

var (
	errProtectedRepoNotFound = fmt.Errorf("protected repository not found")
	errJobInFlight           = fmt.Errorf("a backup for this repository is already running")
)

// runProtectedBackup walks a single backup job through its lifecycle, persisting
// every transition. A background context is used so the job survives the client
// disconnecting after the 202 response.
func (s *Server) runProtectedBackup(jobID string, repo state.ProtectedRepo) {
	ctx := context.Background()
	bg := s.logger

	getJob := func() state.BackupJob {
		j, _ := s.stateStore.BackupJob(jobID)
		return j
	}
	setState := func(st string) {
		j := getJob()
		j.State = st
		if err := s.stateStore.UpdateBackupJob(j); err != nil {
			bg.Warn("update backup job state", "job", jobID, "state", st, "error", err)
			return
		}
		if err := s.saveState(); err != nil {
			bg.Warn("persist backup job state", "job", jobID, "state", st, "error", err)
		}
	}

	conn, ok := s.stateStore.GitHubConnection()
	if !ok {
		s.finishProtectedJob(jobID, repo, "GitHub is not connected")
		return
	}
	token, err := s.tokenStore.Get(conn.TokenRef)
	if err != nil {
		s.finishProtectedJob(jobID, repo, "GitHub access token is unavailable")
		return
	}

	// Google Drive is the only valid backup destination: without a connected
	// account a backup cannot complete. Re-checked here (in addition to the
	// request-time check) so a disconnect mid-flight fails cleanly too.
	driveConn, ok := s.driveConnection()
	if !ok {
		s.finishProtectedJob(jobID, repo, errDriveNotConnected.Error()+". Connect your Google account and try again.")
		return
	}
	refreshToken, err := s.tokenStore.Get(driveConn.TokenRef)
	if err != nil || refreshToken == "" {
		s.finishProtectedJob(jobID, repo, "Google Drive access token is unavailable. Reconnect your account and try again.")
		return
	}

	// Stage the bundle in a temporary directory so no permanent local copy
	// survives a finished backup. The temporary directory is always removed.
	staging, err := os.MkdirTemp("", "gitsafe-staging-*")
	if err != nil {
		s.finishProtectedJob(jobID, repo, "could not create a temporary staging directory")
		return
	}
	defer os.RemoveAll(staging)

	setState(state.JobCloning)
	outcome, err := s.bundleBackup(ctx, repo.FullName, token, staging)
	if err != nil {
		bg.Error("protected backup failed", "job", jobID, "repo", repo.FullName, "error", err)
		s.finishProtectedJob(jobID, repo, "backup failed: "+err.Error())
		return
	}

	setState(state.JobUploading)
	s.setJobProgress(jobID, jobProgressMetrics{UploadedBytes: 0, TotalBytes: outcome.SizeBytes})
	driveID, err := s.driveUpload(ctx, outcome.BundlePath, driveConn.StorageFolderID, refreshToken, func(now, total int64) {
		s.setJobProgress(jobID, jobProgressMetrics{UploadedBytes: now, TotalBytes: total})
	}, s.logger)
	// Upload progress is live-only: it is cleared once the upload ends so
	// terminal jobs never report bytes.
	s.clearJobProgress(jobID)
	if err != nil {
		// Upload failed: remove the staged bundle (best-effort) so no local
		// copy lingers, then fail the job with no backup record.
		bg.Error("drive upload failed", "job", jobID, "repo", repo.FullName, "bundle", outcome.BundleName, "error", err)
		_ = os.Remove(outcome.BundlePath)
		s.finishProtectedJob(jobID, repo, "drive upload failed: "+err.Error())
		return
	}

	// Upload succeeded: the temporary local bundle is no longer needed.
	if err := os.Remove(outcome.BundlePath); err != nil {
		bg.Warn("remove temporary bundle after upload", "job", jobID, "bundle", outcome.BundlePath, "error", err)
	}

	// The backup record exists only for completed Drive uploads: every visible
	// backup is a real Drive copy.
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
	s.stateStore.AddBackupRecord(record)

	j := getJob()
	j.State = state.JobCompleted
	j.FinishedAt = time.Now()
	if err := s.stateStore.UpdateBackupJob(j); err != nil {
		bg.Warn("complete backup job", "job", jobID, "error", err)
	}
	if err := s.saveState(); err != nil {
		bg.Warn("persist completed backup job", "job", jobID, "error", err)
	}
	bg.Info("protected backup completed", "job", jobID, "repo", repo.FullName, "bundle", outcome.BundleName, "driveFileID", driveID)
}

// finishProtectedJob marks a job failed with a sanitized message and persists it.
func (s *Server) finishProtectedJob(jobID string, repo state.ProtectedRepo, message string) {
	j, ok := s.stateStore.BackupJob(jobID)
	if !ok {
		return
	}
	message = sanitizeMessage(message)
	j.State = state.JobFailed
	j.FinishedAt = time.Now()
	j.Error = message
	if err := s.stateStore.UpdateBackupJob(j); err != nil {
		s.logger.Warn("fail backup job", "job", jobID, "error", err)
	}
	if err := s.saveState(); err != nil {
		s.logger.Warn("persist failed backup job", "job", jobID, "error", err)
	}
}

// --- handlers ---

// handleBackupProtectedRepo triggers a backup for a single protected repository.
func (s *Server) handleBackupProtectedRepo(w http.ResponseWriter, r *http.Request) {
	if !s.cloudReady(w) {
		return
	}
	sess := s.sessionFromRequest(r)
	if sess == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "No active session. Refresh the page and try again."})
		return
	}
	if !validateCSRF(sess, r.Header.Get("X-CSRF-Token")) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "CSRF validation failed."})
		return
	}

	id := r.PathValue("id")
	job, err := s.startProtectedBackup(id)
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
	if !s.cloudReady(w) {
		return
	}
	sess := s.sessionFromRequest(r)
	if sess == nil {
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

	repos, err := s.discoverRepositories(r)
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

	internalRepo, err := s.findOrCreateInternalRepo(found.ID, found.FullName, found.DefaultBranch)
	if err != nil {
		s.logger.Warn("ensure internal repo record", "repo", found.FullName, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not start the backup"})
		return
	}

	job, err := s.startProtectedBackup(internalRepo.ID)
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
	if !s.cloudReady(w) {
		return
	}
	sess := s.sessionFromRequest(r)
	if sess == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "No active session. Refresh the page and try again."})
		return
	}
	if !validateCSRF(sess, r.Header.Get("X-CSRF-Token")) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "CSRF validation failed."})
		return
	}
	if !s.driveConnected() {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "Google Drive is not connected. Connect your Google account before backing up.",
		})
		return
	}

	repos, err := s.discoverRepositories(r)
	if err != nil {
		s.writeDiscoveryError(w, err)
		return
	}

	var started []jobView
	for _, repo := range repos {
		internalRepo, err := s.findOrCreateInternalRepo(repo.ID, repo.FullName, repo.DefaultBranch)
		if err != nil {
			s.logger.Warn("ensure internal repo record (backup all)", "repo", repo.FullName, "error", err)
			continue
		}
		job, err := s.startProtectedBackup(internalRepo.ID)
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
	if s.github == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "GitHub integration is not configured."})
		return
	}
	id := r.PathValue("id")
	job, ok := s.stateStore.BackupJob(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "backup job not found"})
		return
	}
	writeJSON(w, http.StatusOK, s.toJobView(job))
}

// handleProtectedBackupJobs lists the backup jobs for a protected repository.
func (s *Server) handleProtectedBackupJobs(w http.ResponseWriter, r *http.Request) {
	if s.github == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "GitHub integration is not configured."})
		return
	}
	id := r.PathValue("id")
	if _, ok := s.stateStore.ProtectedRepo(id); !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "protected repository not found"})
		return
	}
	jobs := s.stateStore.BackupJobs()
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
	if s.github == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "GitHub integration is not configured."})
		return
	}
	id := r.PathValue("id")
	if _, ok := s.stateStore.ProtectedRepo(id); !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "protected repository not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"backups": backupsView(s.stateStore.BackupRecordsForRepo(id))})
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
