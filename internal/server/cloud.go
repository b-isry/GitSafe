package server

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/b-isry/gitsafe/internal/providers"
	"github.com/b-isry/gitsafe/internal/state"
	"github.com/google/uuid"
)

// cloudRepositoriesPage is the template key for the GitSafe page.
const cloudRepositoriesPage = "cloud-repositories"

// handleCloudRepositories renders the page that lets the user discover GitHub
// repositories and explicitly protect the ones GitSafe should back up. The
// page always presents an end-user "Connect GitHub" flow when disconnected,
// regardless of whether the deployment has configured a GitHub application.
func (s *Server) handleCloudRepositories(w http.ResponseWriter, r *http.Request) {
	view := CloudView{Configured: s.github != nil}
	if s.stateStore != nil {
		if conn, ok := s.stateStore.GitHubConnection(); ok {
			view.Connected = true
			view.Login = conn.Login
			view.Name = conn.Name
			view.AvatarURL = conn.AvatarURL
		}
	}
	s.render(w, cloudRepositoriesPage, renderData{
		pageData: pageData{Active: "cloud-repositories", PageTitle: "GitSafe"},
		Cloud:    view,
	})
}

// CloudView carries the cloud page's initial connection state and account
// details so the UI can show either the "connect GitHub" prompt or the
// connected account header. It never carries credentials, tokens, or client
// configuration.
type CloudView struct {
	Configured bool
	Connected  bool
	Login      string
	Name       string
	AvatarURL  string
}

// protectedRepoView is the JSON shape of a ProtectedRepo exposed to the UI.
type protectedRepoView struct {
	ID            string            `json:"id"`
	GitHubID      int64             `json:"githubId"`
	FullName      string            `json:"fullName"`
	DefaultBranch string            `json:"defaultBranch"`
	AddedAt       string            `json:"addedAt"`
	LatestBackup  *latestBackupView `json:"latestBackup,omitempty"`
}

// latestBackupView is the most recent backup record for a protected repo, used
// so the UI can show a status dot, timestamp, size, and a Drive link without an
// extra round trip per repository.
type latestBackupView struct {
	Status          string `json:"status"`
	CreatedAt       string `json:"createdAt"`
	BundleSizeBytes int64  `json:"bundleSizeBytes"`
	DriveViewLink   string `json:"driveViewLink,omitempty"`
}

// handleAPIRepositories lists discovered repositories for the connected account
// and annotates each with its actual backup state: whether the repository has a
// successful backup on record and what the latest successful backup was. Returns
// a 503 when GitHub is not configured, a 409 when not connected, and
// provider-specific errors for upstream failures.
func (s *Server) handleAPIRepositories(w http.ResponseWriter, r *http.Request) {
	if !s.cloudReady(w) {
		return
	}
	repos, err := s.discoverRepositories(r)
	if err != nil {
		s.writeDiscoveryError(w, err)
		return
	}

	// Backup status is derived from actual successful backup records, never from
	// configuration. The internal per-repository record (which the backup engine
	// requires to correlate jobs and history) is used only to look up those
	// records; the user never sees or creates it directly.
	internal := map[int64]state.ProtectedRepo{}
	for _, p := range s.stateStore.ProtectedRepos() {
		internal[p.GitHubID] = p
	}

	threshold := s.app.ConfigSnapshot().Days
	now := time.Now()
	out := make([]map[string]any, 0, len(repos))
	for _, rp := range repos {
		daysSinceLastPush, stale := pushStaleness(rp.PushedAt, now, threshold)
		var lb *latestBackupView
		if rec, ok := internal[rp.ID]; ok {
			lb = latestBackupFor(s.stateStore.BackupRecordsForRepo(rec.ID))
		}
		out = append(out, map[string]any{
			"githubId":          rp.ID,
			"fullName":          rp.FullName,
			"private":           rp.Private,
			"fork":              rp.Fork,
			"archived":          rp.Archived,
			"defaultBranch":     rp.DefaultBranch,
			"updatedAt":         rp.UpdatedAt,
			"pushedAt":          rp.PushedAt,
			"sizeKB":            rp.SizeKB,
			"cloneUrl":          rp.CloneURL,
			"backedUp":          lb != nil,
			"latestBackup":      lb,
			"daysSinceLastPush": daysSinceLastPush,
			"stale":             stale,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"repositories": out,
	})
}

// findOrCreateInternalRepo returns the internal per-repository record the backup
// engine uses to track jobs and history for a GitHub repository, creating it on
// demand. This is what lets the dashboard back up any repository directly: the
// engine's record is materialized lazily at backup time instead of requiring a
// user-facing "protect" step first.
func (s *Server) findOrCreateInternalRepo(githubID int64, fullName, defaultBranch string) (state.ProtectedRepo, error) {
	for _, r := range s.stateStore.ProtectedRepos() {
		if r.GitHubID == githubID {
			return r, nil
		}
	}
	rec := state.ProtectedRepo{
		ID:            uuid.NewString(),
		GitHubID:      githubID,
		FullName:      fullName,
		DefaultBranch: defaultBranch,
		AddedAt:       time.Now(),
	}
	if err := s.stateStore.AddProtectedRepo(rec); err != nil {
		// A concurrent request already created the record; reuse it so this
		// repository is never duplicated internally.
		if errors.Is(err, state.ErrDuplicate) {
			for _, r := range s.stateStore.ProtectedRepos() {
				if r.GitHubID == githubID {
					return r, nil
				}
			}
		}
		return state.ProtectedRepo{}, err
	}
	if err := s.saveState(); err != nil {
		return state.ProtectedRepo{}, err
	}
	return rec, nil
}

// pushStaleness derives the staleness view for a discovered repository. A zero
// PushedAt means "never pushed" and reports a nil day count (serialized as JSON
// null) with stale true. Otherwise the elapsed days since the last push are
// compared against the configured threshold.
func pushStaleness(pushedAt, now time.Time, threshold int) (daysSinceLastPush *int, stale bool) {
	if pushedAt.IsZero() {
		return nil, true
	}
	days := int(now.Sub(pushedAt).Hours() / 24)
	return &days, days >= threshold
}

// discoverRepositories resolves the connection token and returns the live
// discovered repository set for the authenticated account.
func (s *Server) discoverRepositories(r *http.Request) ([]providers.Repository, error) {
	conn, ok := s.stateStore.GitHubConnection()
	if !ok {
		return nil, errGitHubDisconnected
	}
	token, err := s.tokenStore.Get(conn.TokenRef)
	if err != nil {
		return nil, errTokenMissing
	}
	return s.githubLister(r.Context(), token)
}

// s (small) sentinel errors keep discovery failures distinguishable in handlers.
var (
	errGitHubDisconnected = errors.New("github not connected")
	errTokenMissing       = errors.New("github access token missing")
)

// writeDiscoveryError maps a discovery error to the appropriate HTTP response.
func (s *Server) writeDiscoveryError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errGitHubDisconnected):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "GitHub is not connected."})
	case errors.Is(err, errTokenMissing):
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Access token is missing. Reconnect GitHub."})
	case errors.Is(err, providers.ErrUnauthorized):
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "The GitHub token is no longer valid. Reconnect."})
	case errors.Is(err, providers.ErrRateLimited):
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "GitHub rate limit reached. Try again later."})
	default:
		// Surface a generic, human-readable message rather than raw upstream
		// error text, which could embed URLs, tokens, or implementation detail.
		s.logger.Warn("github discovery failed", "error", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "Could not reach GitHub. Please try again later."})
	}
}

// protectRequest is the body accepted by POST /api/protected-repositories.
type protectRequest struct {
	RepoIDs []int64 `json:"repoIds"`
}

// maxProtectBatch bounds one protect request so a flooding local client cannot
// submit an unbounded repository-id list.
const maxProtectBatch = 500

// handleProtectRepositories explicitly protects the selected repositories. It
// validates each id against the live discovered set (so stale/nonexistent ids
// are never persisted), is idempotent for already-protected repositories, and
// requires a valid CSRF token.
func (s *Server) handleProtectRepositories(w http.ResponseWriter, r *http.Request) {
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

	var req protectRequest
	if err := decodeJSONBody(w, r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if len(req.RepoIDs) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "repoIds is required."})
		return
	}
	if len(req.RepoIDs) > maxProtectBatch {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "too many repositories in one request."})
		return
	}

	repos, err := s.discoverRepositories(r)
	if err != nil {
		s.writeDiscoveryError(w, err)
		return
	}

	protected := map[int64]bool{}
	for _, p := range s.stateStore.ProtectedRepos() {
		protected[p.GitHubID] = true
	}

	toAdd, already, errs := resolveProtectCandidates(repos, req.RepoIDs, protected)

	added := []protectedRepoView{}
	for _, rec := range toAdd {
		rec.ID = uuid.NewString()
		rec.AddedAt = time.Now()
		if err := s.stateStore.AddProtectedRepo(rec); err != nil {
			// A concurrent duplicate falls back to "already protected".
			already = append(already, rec.GitHubID)
			continue
		}
		added = append(added, toProtectedView(rec))
	}
	if err := s.saveState(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not persist the protected repositories"})
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"protected":        added,
		"alreadyProtected": already,
		"errors":           errs,
	})
}

// resolveProtectCandidates is a pure function that turns a set of requested
// GitHub ids into the ProtectedRepo records to persist, the ids that were
// already protected (idempotent skip), and per-id errors for repositories that
// no longer exist on GitHub.
func resolveProtectCandidates(
	discovered []providers.Repository,
	requested []int64,
	alreadyProtected map[int64]bool,
) (toAdd []state.ProtectedRepo, already []int64, errs map[int64]string) {
	byID := make(map[int64]providers.Repository, len(discovered))
	for _, rp := range discovered {
		byID[rp.ID] = rp
	}
	errs = map[int64]string{}
	seen := map[int64]bool{}
	for _, id := range requested {
		if seen[id] {
			continue
		}
		seen[id] = true
		if alreadyProtected[id] {
			already = append(already, id)
			continue
		}
		repo, ok := byID[id]
		if !ok {
			errs[id] = "repository no longer exists on GitHub"
			continue
		}
		toAdd = append(toAdd, state.ProtectedRepo{
			GitHubID:      repo.ID,
			FullName:      repo.FullName,
			DefaultBranch: repo.DefaultBranch,
		})
	}
	return toAdd, already, errs
}

func toProtectedView(r state.ProtectedRepo) protectedRepoView {
	return protectedRepoView{
		ID:            r.ID,
		GitHubID:      r.GitHubID,
		FullName:      r.FullName,
		DefaultBranch: r.DefaultBranch,
		AddedAt:       r.AddedAt.Format(time.RFC3339),
	}
}

// handleListProtectedRepositories returns the currently protected repositories
// from state (the source of truth), without contacting GitHub.
func (s *Server) handleListProtectedRepositories(w http.ResponseWriter, _ *http.Request) {
	if s.github == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "GitHub integration is not configured."})
		return
	}
	repos := s.stateStore.ProtectedRepos()
	out := make([]protectedRepoView, 0, len(repos))
	for _, rp := range repos {
		view := toProtectedView(rp)
		view.LatestBackup = latestBackupFor(s.stateStore.BackupRecordsForRepo(rp.ID))
		out = append(out, view)
	}
	writeJSON(w, http.StatusOK, map[string]any{"repositories": out})
}

// latestBackupFor picks the newest successful backup record and shapes it for
// the UI. It returns nil when the repository has no successful backup yet. Only
// records with Status == BackupStatusUploaded count: a failed attempt, or
// merely having been configured/protected, never establishes a backup.
func latestBackupFor(records []state.BackupRecord) *latestBackupView {
	var latest *state.BackupRecord
	for i := range records {
		if records[i].Status != state.BackupStatusUploaded {
			continue
		}
		if latest == nil || records[i].CreatedAt.After(latest.CreatedAt) {
			latest = &records[i]
		}
	}
	if latest == nil {
		return nil
	}
	view := &latestBackupView{
		Status:          latest.Status,
		CreatedAt:       latest.CreatedAt.Format(time.RFC3339),
		BundleSizeBytes: latest.BundleSize,
	}
	if latest.DriveFileID != "" {
		view.DriveViewLink = fmt.Sprintf("https://drive.google.com/file/d/%s/view", latest.DriveFileID)
	}
	return view
}

// handleRemoveProtectedRepository removes protection for a single repository by
// its local ProtectedRepo id. It requires a valid CSRF token.
func (s *Server) handleRemoveProtectedRepository(w http.ResponseWriter, r *http.Request) {
	if s.github == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "GitHub integration is not configured."})
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

	id := strings.TrimSpace(r.PathValue("id"))
	if err := s.stateStore.RemoveProtectedRepo(id); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "protected repository not found"})
		return
	}
	if err := s.saveState(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not persist the change"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "removed"})
}

// cloudReady reports whether the GitHub integration and its state/token stores
// are wired up, writing a 503 when not. It is used by handlers that need the
// cloud connection.
func (s *Server) cloudReady(w http.ResponseWriter) bool {
	if s.github == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "GitHub integration is not configured."})
		return false
	}
	if s.stateStore == nil || s.tokenStore == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "GitHub integration is not configured."})
		return false
	}
	return true
}
