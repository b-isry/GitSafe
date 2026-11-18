package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/b-isry/gitsafe/internal/archiver"
	"github.com/b-isry/gitsafe/internal/providers"
	"github.com/b-isry/gitsafe/internal/state"
)

// Opt-in daily backups to Google Drive while the user is signed out.
//
// Render's free tier has no cron and sleeps when idle, so a scheduled GitHub
// Actions workflow (.github/workflows/daily-backup.yml) wakes the instance
// and calls POST /api/cron/backup with a shared secret. The endpoint answers
// 202 immediately and the run continues in the background: users one after
// another, each user's repositories one at a time (through the same per-user
// slot and global semaphore as interactive backups). The workflow then polls
// GET /api/cron/backup, which also keeps the free instance awake (it only
// spins down after ~15 minutes without inbound requests).
//
// Runs are resumable: a repository already backed up to Drive in the last
// dailyFreshWindow, or not pushed to since its last Drive backup, is skipped,
// so re-triggering after a restart or timeout continues where it stopped
// instead of re-uploading everything.

// CronSecretEnv is the shared secret the scheduler sends as a bearer token.
const CronSecretEnv = "GITSAFE_CRON_SECRET"

// minCronSecretLen rejects guessable secrets; `openssl rand -hex 32` gives 64.
const minCronSecretLen = 32

// dailyRunBudget bounds one run so it finishes within the workflow's polling
// window; anything left is picked up by the next run.
const dailyRunBudget = 50 * time.Minute

// dailyFreshWindow: a repository with a Drive backup newer than this is not
// backed up again by the daily run.
const dailyFreshWindow = 20 * time.Hour

// cronRun is the in-memory status of the current or last run (reported to the
// workflow; per-user results are persisted in the DailyStore).
type cronRun struct {
	ID          string    `json:"id"`
	StartedAt   time.Time `json:"startedAt"`
	FinishedAt  time.Time `json:"finishedAt,omitzero"`
	Users       int       `json:"users"`
	UsersDone   int       `json:"usersDone"`
	OK          int       `json:"ok"`
	Failed      int       `json:"failed"`
	Skipped     int       `json:"skipped"`
	NeedsReauth int       `json:"needsReauth"`
	Error       string    `json:"error,omitempty"`
}

type cronState struct {
	mu      sync.Mutex
	running bool
	last    *cronRun
}

var getenv = os.Getenv

func (s *Server) cronEnabled() bool { return s.cronSecret != "" && s.daily != nil }

// cronSecretFromEnv returns the configured secret, or "" (endpoint disabled)
// when unset or too short.
func cronSecretFromEnv(logger interface{ Warn(string, ...any) }) string {
	v := strings.TrimSpace(getenv(CronSecretEnv))
	if v == "" {
		return ""
	}
	if len(v) < minCronSecretLen {
		logger.Warn("daily backups disabled: "+CronSecretEnv+" is too short", "minLength", minCronSecretLen)
		return ""
	}
	return v
}

// authorizeCron checks "Authorization: Bearer <secret>" in constant time
// (comparing SHA-256 digests so the length is not leaked either).
func (s *Server) authorizeCron(w http.ResponseWriter, r *http.Request) bool {
	if !s.cronEnabled() {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return false
	}
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	want := sha256.Sum256([]byte(s.cronSecret))
	have := sha256.Sum256([]byte(strings.TrimSpace(got)))
	if !ok || subtle.ConstantTimeCompare(want[:], have[:]) != 1 {
		w.Header().Set("WWW-Authenticate", `Bearer realm="gitsafe-cron"`)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return false
	}
	return true
}

// handleCronBackup starts a daily run (202) unless one is already running.
func (s *Server) handleCronBackup(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeCron(w, r) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if s.shuttingDown() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "server is restarting; retry shortly"})
		return
	}
	s.cron.mu.Lock()
	if s.cron.running {
		run := *s.cron.last
		s.cron.mu.Unlock()
		writeJSON(w, http.StatusAccepted, map[string]any{"status": "already_running", "run": run})
		return
	}
	run := &cronRun{ID: uuid.NewString(), StartedAt: time.Now().UTC()}
	s.cron.running = true
	s.cron.last = run
	snapshot := *run
	s.cron.mu.Unlock()

	s.bgWG.Add(1)
	go func() {
		defer s.bgWG.Done()
		s.runDailyBackups(run)
	}()
	s.logger.Info("daily backup run started", "run", run.ID)
	writeJSON(w, http.StatusAccepted, map[string]any{"status": "started", "run": snapshot})
}

// handleCronStatus reports the current or last run for the scheduler.
func (s *Server) handleCronStatus(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeCron(w, r) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.cron.mu.Lock()
	defer s.cron.mu.Unlock()
	resp := map[string]any{"running": s.cron.running}
	if s.cron.last != nil {
		resp["run"] = *s.cron.last
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) updateCronRun(run *cronRun, f func(*cronRun)) {
	s.cron.mu.Lock()
	defer s.cron.mu.Unlock()
	f(run)
}

// runDailyBackups processes every opted-in user sequentially.
func (s *Server) runDailyBackups(run *cronRun) {
	defer func() {
		s.cron.mu.Lock()
		run.FinishedAt = time.Now().UTC()
		s.cron.running = false
		s.cron.mu.Unlock()
		s.logger.Info("daily backup run finished", "run", run.ID, "users", run.Users,
			"ok", run.OK, "failed", run.Failed, "skipped", run.Skipped, "needsReauth", run.NeedsReauth)
	}()
	s.sweepExpiredLocalBundles()

	ctx, cancel := context.WithTimeout(s.backgroundContext(), dailyRunBudget)
	defer cancel()

	users, err := s.daily.ListEnabled(ctx)
	if err != nil {
		s.logger.Error("daily backup: list users", "error", err)
		s.updateCronRun(run, func(r *cronRun) { r.Error = "could not list users" })
		return
	}
	s.updateCronRun(run, func(r *cronRun) { r.Users = len(users) })
	for _, userID := range users {
		if ctx.Err() != nil {
			break
		}
		res := s.runDailyForUser(ctx, userID)
		// Record with a fresh context so a run cut short by the budget still
		// saves what it did.
		recCtx, recCancel := context.WithTimeout(context.Background(), dailyQueryTimeout)
		if err := s.daily.RecordRun(recCtx, res); err != nil {
			s.logger.Error("daily backup: record result", "user", userID, "error", err)
		}
		recCancel()
		s.updateCronRun(run, func(r *cronRun) {
			r.UsersDone++
			r.OK += res.LastOK
			r.Failed += res.LastFailed
			r.Skipped += res.LastSkipped
			if res.NeedsReauth != "" {
				r.NeedsReauth++
			}
		})
	}
}

// runDailyForUser backs up one user's repositories to Drive and returns the
// result to persist.
func (s *Server) runDailyForUser(ctx context.Context, userID int64) DailySettings {
	res := DailySettings{UserID: userID, LastRunAt: time.Now().UTC()}
	needs := func(provider, msg string) DailySettings {
		res.NeedsReauth = provider
		res.LastStatus = dailyStatusNeedsReauth
		res.LastMessage = msg
		return res
	}
	const ghMsg = "GitHub access was revoked or expired. Reconnect GitHub to resume daily backups."
	const drvMsg = "Google Drive access was revoked or expired. Reconnect Google Drive to resume daily backups."

	stores, err := s.userStores.getOrCreate(userID)
	if err != nil {
		s.logger.Error("daily backup: open user stores", "user", userID, "error", err)
		res.LastStatus, res.LastMessage = dailyStatusFailed, "Could not load your GitSafe data."
		return res
	}
	st, tk := stores.state, stores.token

	if conn, ok := st.GitHubConnection(); !ok {
		return needs(reauthGitHub, ghMsg)
	} else if tok, err := tk.Get(conn.TokenRef); err != nil || tok == "" {
		return needs(reauthGitHub, ghMsg)
	}
	if !s.driveConnected(st) || s.driveUpload == nil {
		return needs(reauthDrive, drvMsg)
	}
	if _, err := s.driveRefreshToken(st, tk); err != nil {
		return needs(reauthDrive, drvMsg)
	}

	listCtx, listCancel := context.WithTimeout(ctx, 2*time.Minute)
	repos, err := s.discoverRepositories(listCtx, st, tk)
	listCancel()
	if err != nil {
		if errors.Is(err, providers.ErrUnauthorized) || errors.Is(err, errTokenMissing) || errors.Is(err, errGitHubDisconnected) {
			return needs(reauthGitHub, ghMsg)
		}
		s.logger.Warn("daily backup: list repositories", "user", userID, "error", err)
		res.LastStatus, res.LastMessage = dailyStatusFailed, "Could not list your repositories from GitHub; will retry tomorrow."
		return res
	}

	var failures []string
	interrupted := false
	for _, repo := range repos {
		if ctx.Err() != nil {
			interrupted = true
			break
		}
		internal, err := s.findOrCreateInternalRepo(st, repo.ID, repo.FullName, repo.DefaultBranch)
		if err != nil {
			res.LastFailed++
			failures = append(failures, repo.FullName+": could not start")
			continue
		}
		if dailyUpToDate(st, internal.ID, repo.PushedAt, time.Now()) {
			res.LastSkipped++
			continue
		}
		job, queued, dest, err := s.enqueueProtectedBackup(st, internal.ID, state.DestinationDrive)
		switch {
		case errors.Is(err, errJobInFlight):
			res.LastSkipped++
			continue
		case errors.Is(err, errDriveNotConnected):
			return needs(reauthDrive, drvMsg)
		case err != nil:
			res.LastFailed++
			failures = append(failures, repo.FullName+": could not start")
			continue
		}
		err = s.runQueuedBackup(userID, st, tk, job.ID, queued, dest)
		switch {
		case err == nil:
			res.LastOK++
		case errors.Is(err, archiver.ErrEmptyRepository):
			res.LastSkipped++
		case errors.Is(err, errGitHubAuth):
			return needs(reauthGitHub, ghMsg)
		case errors.Is(err, errDriveAuth):
			return needs(reauthDrive, drvMsg)
		case ctx.Err() != nil || s.shuttingDown():
			interrupted = true
		default:
			res.LastFailed++
			msg := "failed"
			if j, ok := st.BackupJob(job.ID); ok && j.Error != "" {
				msg = j.Error
			}
			failures = append(failures, repo.FullName+": "+msg)
		}
		if interrupted {
			break
		}
	}

	switch {
	case res.LastFailed > 0 && res.LastOK == 0:
		res.LastStatus = dailyStatusFailed
	case res.LastFailed > 0 || interrupted:
		res.LastStatus = dailyStatusPartial
	default:
		res.LastStatus = dailyStatusOK
	}
	parts := []string{fmt.Sprintf("%d backed up, %d unchanged or skipped, %d failed", res.LastOK, res.LastSkipped, res.LastFailed)}
	if interrupted {
		parts = append(parts, "stopped early (time limit or server restart); the next run continues")
	}
	if len(failures) > 0 {
		if len(failures) > 3 {
			failures = append(failures[:3], fmt.Sprintf("and %d more", len(failures)-3))
		}
		parts = append(parts, strings.Join(failures, "; "))
	}
	res.LastMessage = truncate(sanitizeMessage(strings.Join(parts, ". ")), 500)
	return res
}

// dailyUpToDate reports whether the repository already has a Drive backup
// that is fresh (within dailyFreshWindow) or newer than its last push. The
// 30-minute margin covers pushes that landed while that backup was cloning.
func dailyUpToDate(st StateStore, protectedRepoID string, pushedAt, now time.Time) bool {
	records := st.BackupRecordsForRepo(protectedRepoID)
	for i := len(records) - 1; i >= 0; i-- {
		rec := records[i]
		if rec.DriveFileID == "" {
			continue
		}
		if now.Sub(rec.CreatedAt) < dailyFreshWindow {
			return true
		}
		return !pushedAt.IsZero() && pushedAt.Before(rec.CreatedAt.Add(-30*time.Minute))
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// --- dashboard API ---

type dailyView struct {
	DailySettings
	Available      bool `json:"available"`
	DriveConnected bool `json:"driveConnected"`
}

// handleGetDailyBackup returns the signed-in user's daily-backup settings.
func (s *Server) handleGetDailyBackup(w http.ResponseWriter, r *http.Request) {
	userID, st, _, _, err := s.requireUser(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "No active session. Refresh the page and try again."})
		return
	}
	v := dailyView{Available: s.cronEnabled(), DriveConnected: s.driveConnected(st)}
	if s.daily != nil {
		settings, err := s.daily.Get(r.Context(), userID)
		if err != nil {
			s.logger.Warn("daily backup: load settings", "error", err)
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "Could not load daily backup settings."})
			return
		}
		v.DailySettings = settings
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, v)
}

// handlePutDailyBackup turns daily backups on or off ({"enabled": bool}).
func (s *Server) handlePutDailyBackup(w http.ResponseWriter, r *http.Request) {
	userID, st, _, sess, err := s.requireUser(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "No active session. Refresh the page and try again."})
		return
	}
	if !validateCSRF(sess, r.Header.Get("X-CSRF-Token")) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "CSRF validation failed."})
		return
	}
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if err := decodeJSONBody(w, r, &body); err != nil || body.Enabled == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": `expected {"enabled": true|false}`})
		return
	}
	if !s.cronEnabled() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Daily backups are not available on this server."})
		return
	}
	if *body.Enabled && !s.driveConnected(st) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "Connect Google Drive before turning on daily backups."})
		return
	}
	if err := s.daily.SetEnabled(r.Context(), userID, *body.Enabled); err != nil {
		s.logger.Error("daily backup: save setting", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Could not save the setting. Please try again."})
		return
	}
	s.handleGetDailyBackup(w, r)
}

// --- lifecycle hooks ---

// disableDaily turns daily backups off when a required connection is removed.
func (s *Server) disableDaily(ctx context.Context, userID int64) {
	if s.daily == nil || userID == 0 {
		return
	}
	if err := s.daily.SetEnabled(ctx, userID, false); err != nil {
		s.logger.Warn("daily backup: disable after disconnect", "error", err)
	}
}

func (s *Server) clearDailyReauth(ctx context.Context, userID int64, provider string) {
	if s.daily == nil {
		return
	}
	if err := s.daily.ClearNeedsReauth(ctx, userID, provider); err != nil {
		s.logger.Warn("daily backup: clear reauth flag", "error", err)
	}
}
