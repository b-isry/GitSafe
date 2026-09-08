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
	"time"

	"github.com/b-isry/gitsafe/internal/archiver"
	"github.com/b-isry/gitsafe/internal/cloud"
	"github.com/b-isry/gitsafe/internal/config"
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

// driveUploadFunc uploads a completed local bundle to Google Drive and returns
// the created Drive file ID. It keeps the cloud package out of the job
// orchestration so tests can stub the network/Google work.
type driveUploadFunc func(ctx context.Context, bundlePath string, cloudCfg config.CloudConfig, logger *slog.Logger) (string, error)

// defaultDriveUpload uploads a bundle to Drive using the configured service
// account and returns the new Drive file ID.
func defaultDriveUpload(ctx context.Context, bundlePath string, cloudCfg config.CloudConfig, logger *slog.Logger) (string, error) {
	return cloud.UploadFile(ctx, cloudCfg, bundlePath, logger, nil)
}

// driveDeleteFunc deletes a Drive file by ID using the configured service
// account, returning nil only when Drive confirms the deletion.
type driveDeleteFunc func(ctx context.Context, fileID string, cloudCfg config.CloudConfig, logger *slog.Logger) error

// defaultDriveDelete removes a Drive file by ID. It returns nil only on a
// provider-confirmed success; used by retention cleanup for IDs GitSafe itself
// recorded.
func defaultDriveDelete(ctx context.Context, fileID string, cloudCfg config.CloudConfig, logger *slog.Logger) error {
	return cloud.DeleteFile(ctx, cloudCfg, fileID, logger)
}

// driveEnabledFor reports whether Drive upload is enabled for a given cloud
// configuration snapshot.
func driveEnabledFor(cfg config.CloudConfig) bool {
	return cfg.Enabled && cfg.CredentialsFile != ""
}

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

	// Snapshot the configuration once under the app lock so this detached
	// goroutine is insulated from a concurrent settings save and reads a stable
	// output path and cloud config for the whole run.
	cfg := s.app.ConfigSnapshot()

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

	setState(state.JobCloning)
	outcome, err := s.bundleBackup(ctx, repo.FullName, token, cfg.OutputPath)
	if err != nil {
		bg.Error("protected backup failed", "job", jobID, "repo", repo.FullName, "error", err)
		s.finishProtectedJob(jobID, repo, "backup failed: "+err.Error())
		return
	}
	setState(state.JobBundling)

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
		// The bundle is always written to local disk first. Drive upload, when
		// enabled, runs afterwards and upgrades the status to "uploaded".
		Status: state.BackupStatusBundled,
	}
	s.stateStore.AddBackupRecord(record)

	setState(state.JobRecording)

	if driveEnabledFor(cfg.Cloud) {
		setState(state.JobUploading)
		driveID, err := s.driveUpload(ctx, outcome.BundlePath, cfg.Cloud, s.logger)
		if err != nil {
			// Preserve the local bundle and its record; only the job fails.
			bg.Error("drive upload failed", "job", jobID, "repo", repo.FullName, "bundle", outcome.BundleName, "error", err)
			s.finishProtectedJob(jobID, repo, "drive upload failed: "+err.Error())
			return
		}
		record.DriveFileID = driveID
		record.Status = state.BackupStatusUploaded
		if err := s.stateStore.UpdateBackupRecord(record); err != nil {
			bg.Warn("persist drive upload status", "job", jobID, "record", record.ID, "error", err)
		}
	}

	j := getJob()
	j.State = state.JobCompleted
	j.FinishedAt = time.Now()
	if err := s.stateStore.UpdateBackupJob(j); err != nil {
		bg.Warn("complete backup job", "job", jobID, "error", err)
	}
	if err := s.saveState(); err != nil {
		bg.Warn("persist completed backup job", "job", jobID, "error", err)
	}
	bg.Info("protected backup completed", "job", jobID, "repo", repo.FullName, "bundle", outcome.BundleName, "driveFileID", record.DriveFileID)
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
	case err != nil:
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not start the backup"})
		return
	}
	writeJSON(w, http.StatusAccepted, toJobView(job))
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
	writeJSON(w, http.StatusOK, toJobView(job))
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
	writeJSON(w, http.StatusOK, map[string]any{"jobs": jobsView(out)})
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

// jobView is the JSON shape of a backup job exposed to the UI.
type jobView struct {
	ID              string `json:"id"`
	ProtectedRepoID string `json:"protectedRepoId"`
	FullName        string `json:"fullName"`
	State           string `json:"state"`
	StartedAt       string `json:"startedAt"`
	FinishedAt      string `json:"finishedAt,omitempty"`
	Error           string `json:"error,omitempty"`
}

func toJobView(j state.BackupJob) jobView {
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
	return v
}

func jobsView(in []state.BackupJob) []jobView {
	out := make([]jobView, 0, len(in))
	for _, j := range in {
		out = append(out, toJobView(j))
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
