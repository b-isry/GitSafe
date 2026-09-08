package server

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/b-isry/gitsafe/internal/providers"
	"github.com/b-isry/gitsafe/internal/state"
	"github.com/google/uuid"
)

// cloudRepositoriesPage is the template key for the "Cloud Repositories" page.
const cloudRepositoriesPage = "cloud-repositories"

// handleCloudRepositories renders the page that lets the user discover GitHub
// repositories and explicitly protect the ones GitSafe should back up.
func (s *Server) handleCloudRepositories(w http.ResponseWriter, r *http.Request) {
	connected, configured := false, s.github != nil
	if s.stateStore != nil {
		if _, ok := s.stateStore.GitHubConnection(); ok {
			connected = true
		}
	}
	s.render(w, cloudRepositoriesPage, renderData{
		pageData: pageData{Active: "cloud-repositories", PageTitle: "Cloud Repositories"},
		Cloud: CloudView{
			Configured: configured,
			Connected:  connected,
		},
	})
}

// CloudView carries the cloud page's initial connection state so the UI can
// decide whether to offer the discovery flow or a "connect GitHub" prompt.
type CloudView struct {
	Configured bool
	Connected  bool
}

// protectedRepoView is the JSON shape of a ProtectedRepo exposed to the UI.
type protectedRepoView struct {
	ID            string `json:"id"`
	GitHubID      int64  `json:"githubId"`
	FullName      string `json:"fullName"`
	DefaultBranch string `json:"defaultBranch"`
	AddedAt       string `json:"addedAt"`
}

// handleAPIRepositories lists discovered repositories for the connected account
// and annotates which of them are already protected. Returns a 503 when GitHub
// is not configured, a 409 when not connected, and provider-specific errors for
// upstream failures.
func (s *Server) handleAPIRepositories(w http.ResponseWriter, r *http.Request) {
	if !s.cloudReady(w) {
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
	protectedList := make([]int64, 0, len(protected))
	for _, rp := range repos {
		if protected[rp.ID] {
			protectedList = append(protectedList, rp.ID)
		}
	}

	out := make([]map[string]any, 0, len(repos))
	for _, rp := range repos {
		out = append(out, map[string]any{
			"githubId":      rp.ID,
			"fullName":      rp.FullName,
			"private":       rp.Private,
			"fork":          rp.Fork,
			"archived":      rp.Archived,
			"defaultBranch": rp.DefaultBranch,
			"updatedAt":     rp.UpdatedAt,
			"pushedAt":      rp.PushedAt,
			"sizeKB":        rp.SizeKB,
			"cloneUrl":      rp.CloneURL,
			"protected":     protected[rp.ID],
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"repositories": out,
		"protected":    protectedList,
	})
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
	return s.githubClientFor(token).ListRepositories(r.Context())
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
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "Could not reach GitHub: " + err.Error()})
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
		out = append(out, toProtectedView(rp))
	}
	writeJSON(w, http.StatusOK, map[string]any{"repositories": out})
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
