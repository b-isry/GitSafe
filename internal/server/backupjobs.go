package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
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

// heldLocalBundle is a finished local backup waiting for the browser to
// download it onto the computer the user is using. It lives only in memory
// and in a temp directory on the server.
type heldLocalBundle struct {
	path      string
	name      string
	createdAt time.Time
}

// localBundleTTL bounds how long a finished local bundle waits on the server
// for the browser to download it. Without a bound, an abandoned download
// would keep a full repository bundle on the (small, ephemeral) disk until
// the next restart.
const localBundleTTL = time.Hour

// stageBundleForDownload moves a finished staging bundle into a temp directory
// so the staging directory can be removed while the file waits for download.
// The destination is the server's temp directory, not the user's computer.
func (s *Server) stageBundleForDownload(src, name string) (string, error) {
	name = filepath.Base(name)
	if name == "" || name == "." || strings.ContainsAny(name, `/\`) {
		return "", fmt.Errorf("invalid bundle name")
	}
	dir, err := os.MkdirTemp(s.localBackupDir, "gitsafe-download-*")
	if err != nil {
		return "", fmt.Errorf("prepare bundle download: %w", err)
	}
	dest, err := moveBundleToLocal(src, dir, name)
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	return dest, nil
}

func (s *Server) rememberLocalBundle(jobID, path, name string) {
	s.sweepExpiredLocalBundles()
	s.localHoldMu.Lock()
	defer s.localHoldMu.Unlock()
	if s.localHold == nil {
		s.localHold = map[string]heldLocalBundle{}
	}
	s.localHold[jobID] = heldLocalBundle{path: path, name: name, createdAt: time.Now()}
}

// sweepExpiredLocalBundles deletes held local bundles older than
// localBundleTTL that were never downloaded.
func (s *Server) sweepExpiredLocalBundles() {
	cutoff := time.Now().Add(-localBundleTTL)
	s.localHoldMu.Lock()
	var expired []heldLocalBundle
	for id, held := range s.localHold {
		if held.createdAt.Before(cutoff) {
			expired = append(expired, held)
			delete(s.localHold, id)
		}
	}
	s.localHoldMu.Unlock()
	for _, held := range expired {
		_ = os.RemoveAll(filepath.Dir(held.path))
	}
}

func (s *Server) localBundle(jobID string) (heldLocalBundle, bool) {
	s.localHoldMu.Lock()
	defer s.localHoldMu.Unlock()
	held, ok := s.localHold[jobID]
	return held, ok
}

// discardLocalBundle removes a held bundle and its temp directory.
func (s *Server) discardLocalBundle(jobID string) {
	s.localHoldMu.Lock()
	held, ok := s.localHold[jobID]
	delete(s.localHold, jobID)
	s.localHoldMu.Unlock()
	if !ok {
		return
	}
	_ = os.RemoveAll(filepath.Dir(held.path))
}

// moveBundleToLocal moves a finished staging bundle into dir, keeping the
// existing bundle filename. It refuses to overwrite a file that is already
// there. A cross-device rename falls back to a copy, and the staging file is
// removed only after that copy succeeds.
func moveBundleToLocal(src, dir, name string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create local backup folder: %w", err)
	}
	name = filepath.Base(name)
	if name == "" || name == "." || strings.ContainsAny(name, `/\`) {
		return "", fmt.Errorf("invalid bundle name")
	}
	dest := filepath.Join(dir, name)
	if _, err := os.Lstat(dest); err == nil {
		return "", fmt.Errorf("local backup %s already exists", name)
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("check local backup: %w", err)
	}
	if err := os.Rename(src, dest); err == nil {
		return dest, nil
	}
	if err := copyFileExclusive(src, dest); err != nil {
		return "", fmt.Errorf("save local backup: %w", err)
	}
	// The durable copy is in place. Staging-directory cleanup removes the
	// temporary file if this delete fails.
	_ = os.Remove(src)
	return dest, nil
}

func copyFileExclusive(src, dest string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		_ = os.Remove(dest)
		return copyErr
	}
	if closeErr != nil {
		_ = os.Remove(dest)
		return closeErr
	}
	return nil
}

// backupDestination reads the optional destination from a backup request.
// A missing or empty body keeps the existing Drive default.
func backupDestination(w http.ResponseWriter, r *http.Request) (string, bool) {
	if r.Body == nil || r.ContentLength == 0 {
		return state.DestinationDrive, true
	}
	var req struct {
		Destination string `json:"destination"`
	}
	if err := decodeJSONBody(w, r, &req); err != nil {
		if errors.Is(err, io.EOF) {
			return state.DestinationDrive, true
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": `Invalid backup destination. Use "drive" or "local".`,
		})
		return "", false
	}
	switch strings.TrimSpace(req.Destination) {
	case "", state.DestinationDrive:
		return state.DestinationDrive, true
	case state.DestinationLocal:
		return state.DestinationLocal, true
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": `Invalid backup destination. Use "drive" or "local".`,
		})
		return "", false
	}
}

// driveUploadFunc uploads a completed local bundle to the connected Google
// Drive account and returns the resulting Drive file ID. folderID is the
// GitSafe storage folder in the connected account. existingFileID is the Drive
// file of this repository's previous backup ("" for none): when it still
// exists it is replaced in place, otherwise a new file is created. driveToken
// is the stored OAuth refresh token (never persisted to state). Real byte
// progress is reported through onProgress (nil is allowed). It keeps the cloud
// package out of the job orchestration so tests can stub the network work.
type driveUploadFunc func(ctx context.Context, bundlePath, folderID, existingFileID, driveToken string, onProgress func(now, total int64), logger *slog.Logger) (string, error)

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
// error if the repository is unknown or already being backed up. destination is
// state.DestinationDrive or state.DestinationLocal; anything else is treated as
// Drive so existing callers keep the original destination.
func (s *Server) startProtectedBackup(userID int64, stateStore StateStore, tokenStore TokenStore, protectedRepoID, destination string) (state.BackupJob, error) {
	job, repo, destination, err := s.enqueueProtectedBackup(stateStore, protectedRepoID, destination)
	if err != nil {
		return state.BackupJob{}, err
	}
	s.bgWG.Add(1)
	go func() {
		defer s.bgWG.Done()
		_ = s.runQueuedBackup(userID, stateStore, tokenStore, job.ID, repo, destination)
	}()
	return job, nil
}

// enqueueProtectedBackup validates and persists a new enqueued job without
// running it.
func (s *Server) enqueueProtectedBackup(stateStore StateStore, protectedRepoID, destination string) (state.BackupJob, state.ProtectedRepo, string, error) {
	if destination != state.DestinationLocal {
		destination = state.DestinationDrive
	}
	repo, ok := stateStore.ProtectedRepo(protectedRepoID)
	if !ok {
		return state.BackupJob{}, state.ProtectedRepo{}, "", errProtectedRepoNotFound
	}

	s.jobMu.Lock()
	defer s.jobMu.Unlock()
	for _, j := range stateStore.UnfinishedJobs() {
		if j.ProtectedRepoID == protectedRepoID {
			return state.BackupJob{}, state.ProtectedRepo{}, "", errJobInFlight
		}
	}

	// Drive backups still require a connected account. Local backups do not.
	if destination == state.DestinationDrive && !s.driveConnected(stateStore) {
		return state.BackupJob{}, state.ProtectedRepo{}, "", errDriveNotConnected
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
		return state.BackupJob{}, state.ProtectedRepo{}, "", err
	}
	return job, repo, destination, nil
}

// runQueuedBackup waits for this user's backup slot and a global slot, then
// runs the job. Jobs of one user run one at a time in arrival order instead of
// failing with "a backup is already running for this user" (which made "Back
// up all" back up only one repository). The global semaphore still caps how
// many mirrors run at once across all users.
func (s *Server) runQueuedBackup(userID int64, stateStore StateStore, tokenStore TokenStore, jobID string, repo state.ProtectedRepo, destination string) error {
	ctx := s.backgroundContext()
	if !s.disablePerUserBackupLimit {
		release, err := s.acquireUserSlot(ctx, userID)
		if err != nil {
			s.finishProtectedJob(stateStore, jobID, repo, "interrupted before it started")
			return err
		}
		defer release()
	}
	select {
	case s.backupSem <- struct{}{}:
	case <-ctx.Done():
		s.finishProtectedJob(stateStore, jobID, repo, "interrupted before it started")
		return ctx.Err()
	}
	defer func() { <-s.backupSem }()
	return s.runProtectedBackup(userID, stateStore, tokenStore, jobID, repo, destination)
}

// userSlot is a per-user, one-at-a-time lock with a reference count so idle
// users do not leave entries behind.
type userSlot struct {
	ch   chan struct{}
	refs int
}

// acquireUserSlot blocks until userID has no other running backup (or ctx is
// done) and returns the release function.
func (s *Server) acquireUserSlot(ctx context.Context, userID int64) (func(), error) {
	s.userBackupMu.Lock()
	if s.userSlots == nil {
		s.userSlots = map[int64]*userSlot{}
	}
	slot, ok := s.userSlots[userID]
	if !ok {
		slot = &userSlot{ch: make(chan struct{}, 1)}
		s.userSlots[userID] = slot
	}
	slot.refs++
	s.userBackupMu.Unlock()

	drop := func() {
		s.userBackupMu.Lock()
		slot.refs--
		if slot.refs == 0 {
			delete(s.userSlots, userID)
		}
		s.userBackupMu.Unlock()
	}
	select {
	case slot.ch <- struct{}{}:
		return func() {
			<-slot.ch
			drop()
		}, nil
	case <-ctx.Done():
		drop()
		return nil, ctx.Err()
	}
}

// backgroundContext is the parent context of all detached backup work. It is
// cancelled by Shutdown so a SIGTERM (Render deploys and free-instance
// spin-down) stops git/Drive work and records jobs as interrupted instead of
// leaving them half-written.
func (s *Server) backgroundContext() context.Context {
	if s.bgCtx == nil {
		return context.Background()
	}
	return s.bgCtx
}

// shuttingDown reports whether Shutdown has been called.
func (s *Server) shuttingDown() bool {
	return s.bgCtx != nil && s.bgCtx.Err() != nil
}

// Shutdown cancels detached backup work and waits (bounded by ctx) for jobs to
// record their final state.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.bgCancel != nil {
		s.bgCancel()
	}
	done := make(chan struct{})
	go func() {
		s.bgWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// maxConcurrentBackups caps how many protected-repo mirrors run simultaneously.
// Backups beyond this wait in the enqueued state, preserving per-repo ordering.
const maxConcurrentBackups = 4

var (
	errProtectedRepoNotFound = fmt.Errorf("protected repository not found")
	errJobInFlight           = fmt.Errorf("a backup for this repository is already running")
	errRepoTooLarge          = fmt.Errorf("repository exceeds maximum allowed size")
	// errGitHubAuth / errDriveAuth classify failures caused by a revoked or
	// expired authorization, so the daily runner can stop and ask the user to
	// reconnect instead of retrying every repository.
	errGitHubAuth = errors.New("GitHub authorization is no longer valid")
	errDriveAuth  = errors.New("Google Drive authorization is no longer valid")
)

// runProtectedBackup walks a single backup job through its lifecycle, persisting
// every transition. A background context is used so the job survives the client
// disconnecting after the 202 response. The returned error is the cause of a
// failure (nil on success); the job record is updated either way.
func (s *Server) runProtectedBackup(userID int64, stateStore StateStore, tokenStore TokenStore, jobID string, repo state.ProtectedRepo, destination string) error {
	if destination != state.DestinationLocal {
		destination = state.DestinationDrive
	}
	bg := s.logger
	defer s.clearJobProgress(jobID)

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
	fail := func(cause error, message string) error {
		s.finishProtectedJob(stateStore, jobID, repo, message)
		return cause
	}

	// Get GitHub connection and token
	conn, ok := stateStore.GitHubConnection()
	if !ok {
		return fail(errGitHubAuth, "GitHub is not connected")
	}
	token, err := tokenStore.Get(conn.TokenRef)
	if err != nil || token == "" {
		return fail(errGitHubAuth, "GitHub access token is unavailable")
	}

	// Google Drive remains required only when that destination was selected.
	var driveConn state.DriveConnection
	var refreshToken string
	if destination == state.DestinationDrive {
		var connected bool
		driveConn, connected = s.driveConnection(stateStore)
		if !connected || s.driveUpload == nil {
			return fail(errDriveAuth, errDriveNotConnected.Error()+". Connect your Google account and try again.")
		}
		refreshToken, err = tokenStore.Get(driveConn.TokenRef)
		if err != nil || refreshToken == "" {
			return fail(errDriveAuth, "Google Drive access token is unavailable. Reconnect your account and try again.")
		}
	}

	// Create timeout context for the whole job (size check, clone, bundle,
	// upload). It is derived from the server's background context so shutdown
	// cancels it.
	timeoutMinutes := s.app.Config.BackupTimeoutMinutes
	if timeoutMinutes <= 0 {
		timeoutMinutes = 20
	}
	ctx, cancel := context.WithTimeout(s.backgroundContext(), time.Duration(timeoutMinutes)*time.Minute)
	defer cancel()

	// Check repo size before cloning (use a short timeout for the API call)
	sizeCtx, sizeCancel := context.WithTimeout(ctx, 30*time.Second)
	repoSizeMB, err := s.checkRepoSize(sizeCtx, tokenStore, conn.TokenRef, repo.FullName)
	sizeCancel()
	if err != nil {
		bg.Error("failed to check repo size", "job", jobID, "repo", repo.FullName, "error", err)
		if errors.Is(err, providers.ErrUnauthorized) {
			return fail(errGitHubAuth, "GitHub authorization is no longer valid. Reconnect GitHub and try again.")
		}
		return fail(err, "failed to check repository size: "+err.Error())
	}
	maxRepoMB := s.app.Config.MaxRepoMB
	if maxRepoMB <= 0 {
		maxRepoMB = 500
	}
	if repoSizeMB > maxRepoMB {
		bg.Warn("repository too large", "job", jobID, "repo", repo.FullName, "sizeMB", repoSizeMB, "maxMB", maxRepoMB)
		return fail(errRepoTooLarge, fmt.Sprintf("repository size (%d MB) exceeds maximum allowed (%d MB)", repoSizeMB, maxRepoMB))
	}

	// Stage the bundle in a temporary directory
	staging, err := os.MkdirTemp("", "gitsafe-staging-*")
	if err != nil {
		return fail(err, "could not create a temporary staging directory")
	}
	defer os.RemoveAll(staging)

	setState(state.JobCloning)
	outcome, err := s.bundleBackup(ctx, repo.FullName, token, staging)
	if err != nil {
		if errors.Is(err, archiver.ErrEmptyRepository) {
			return fail(err, "repository is empty; there are no commits to back up")
		}
		bg.Error("protected backup failed", "job", jobID, "repo", repo.FullName, "error", err)
		return fail(err, "backup failed: "+err.Error())
	}

	if destination == state.DestinationLocal {
		return s.finishLocalBackup(stateStore, jobID, repo, outcome, bg, setState, getJob)
	}

	setState(state.JobUploading)
	s.setJobProgress(jobID, jobProgressMetrics{UploadedBytes: 0, TotalBytes: outcome.SizeBytes})

	// Re-backups replace this repository's previous Drive file in place. A
	// newer local backup has no Drive file ID, so walk back to the latest
	// record that does. If that file was deleted or trashed in Drive, the
	// uploader creates a new one instead of failing forever.
	records := stateStore.BackupRecordsForRepo(repo.ID)
	var existingDriveFileID string
	for i := len(records) - 1; i >= 0; i-- {
		if records[i].DriveFileID != "" {
			existingDriveFileID = records[i].DriveFileID
			break
		}
	}

	driveID, err := s.driveUpload(ctx, outcome.BundlePath, driveConn.StorageFolderID, existingDriveFileID, refreshToken,
		func(now, total int64) {
			s.setJobProgress(jobID, jobProgressMetrics{UploadedBytes: now, TotalBytes: total})
		}, s.logger)
	// The staging directory is removed by the deferred RemoveAll; remove the
	// bundle now as well so a long-running process frees disk promptly.
	if rmErr := os.Remove(outcome.BundlePath); rmErr != nil && !os.IsNotExist(rmErr) {
		bg.Warn("remove temporary bundle after backup", "job", jobID, "bundle", outcome.BundleName, "error", rmErr)
	}
	if err != nil {
		bg.Error("drive upload failed", "job", jobID, "repo", repo.FullName, "bundle", outcome.BundleName, "error", err)
		if errors.Is(err, errDriveAuth) {
			return fail(err, "Google Drive authorization is no longer valid. Reconnect Google Drive and try again.")
		}
		return fail(err, "drive upload failed: "+err.Error())
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
		Destination:     state.DestinationDrive,
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
	return nil
}

// finishLocalBackup holds the staging bundle until the browser downloads it
// onto the computer the user is using, then records a successful local backup.
// The staging bundle is removed on failure. Nothing is written to the server
// user's home directory.
func (s *Server) finishLocalBackup(stateStore StateStore, jobID string, repo state.ProtectedRepo, outcome bundleOutcome, bg *slog.Logger, setState func(string), getJob func() state.BackupJob) error {
	path, err := s.stageBundleForDownload(outcome.BundlePath, outcome.BundleName)
	if err != nil {
		_ = os.Remove(outcome.BundlePath)
		bg.Error("local backup failed", "job", jobID, "repo", repo.FullName, "bundle", outcome.BundleName, "error", err)
		s.finishProtectedJob(stateStore, jobID, repo, "local backup failed: "+err.Error())
		return err
	}
	s.rememberLocalBundle(jobID, path, outcome.BundleName)

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
		Destination:     state.DestinationLocal,
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
	bg.Info("local backup completed", "job", jobID, "repo", repo.FullName, "bundle", outcome.BundleName)
	return nil
}

// finishProtectedJob marks a job failed with a sanitized message and persists it.
// A job cut short by server shutdown is recorded as interrupted instead, with
// a message telling the user to run it again.
func (s *Server) finishProtectedJob(stateStore StateStore, jobID string, repo state.ProtectedRepo, message string) {
	j, ok := stateStore.BackupJob(jobID)
	if !ok {
		return
	}
	message = sanitizeMessage(message)
	j.State = state.JobFailed
	if s.shuttingDown() {
		j.State = state.JobInterrupted
		message = "interrupted because the server restarted; run the backup again"
	}
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
	destination, ok := backupDestination(w, r)
	if !ok {
		return
	}
	job, err := s.startProtectedBackup(userID, stateStore, tokenStore, id, destination)
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
	if !validateCSRF(sess, r.Header.Get("X-CSRF-Token")) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "CSRF validation failed."})
		return
	}
	destination, ok := backupDestination(w, r)
	if !ok {
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

	job, err := s.startProtectedBackup(userID, stateStore, tokenStore, internalRepo.ID, destination)
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
// it). Drive connectivity is required only when the destination is Drive.
// In-flight repositories are skipped, not treated as errors.
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
	destination, ok := backupDestination(w, r)
	if !ok {
		return
	}
	if destination == state.DestinationDrive && !s.driveConnected(stateStore) {
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
		job, err := s.startProtectedBackup(userID, stateStore, tokenStore, internalRepo.ID, destination)
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

// handleDownloadLocalBundle sends a finished local bundle to the browser so the
// user can save it on the computer they are using. The server copy is deleted
// only after the response is fully written. Another user's session cannot
// download it, because the job must exist in the caller's own state.
func (s *Server) handleDownloadLocalBundle(w http.ResponseWriter, r *http.Request) {
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
	if _, ok := stateStore.BackupJob(id); !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "backup job not found"})
		return
	}
	held, ok := s.localBundle(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "This bundle is no longer available to download."})
		return
	}
	f, err := os.Open(held.path)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "This bundle is no longer available to download."})
		return
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "This bundle is no longer available to download."})
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", attachmentDisposition(held.name))
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	// A bundle can take longer to send than the server's normal write limit.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(time.Hour))
	if _, err := io.Copy(w, f); err != nil {
		f.Close()
		s.logger.Warn("local bundle download interrupted", "job", id, "error", err)
		return
	}
	f.Close()
	s.discardLocalBundle(id)
	s.sweepExpiredLocalBundles()
}

func attachmentDisposition(name string) string {
	name = filepath.Base(name)
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || strings.ContainsRune(`"\/;`, r) {
			return -1
		}
		return r
	}, name)
	if name == "" || name == "." {
		name = "backup.bundle"
	}
	return `attachment; filename="` + name + `"`
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
	// DownloadURL is set while a local bundle is waiting for the browser to
	// save it on the user's computer. It is omitted for Drive backups.
	DownloadURL string `json:"downloadUrl,omitempty"`
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
	if _, ok := s.localBundle(j.ID); ok {
		v.DownloadURL = "/api/backup-jobs/" + j.ID + "/bundle"
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
	Destination     string `json:"destination,omitempty"`
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
			Destination:     r.Destination,
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
