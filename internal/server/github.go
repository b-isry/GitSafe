package server

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/b-isry/gitsafe/internal/githuboauth"
	"github.com/b-isry/gitsafe/internal/providers"
	"github.com/b-isry/gitsafe/internal/state"
)

// GitHubOAuth is the Phase 1 OAuth application configuration used by the server.
// ClientID and RedirectURL are non-secret. ClientSecret is supplied separately
// from the environment and never serialized.
type GitHubOAuth struct {
	ClientID     string
	ClientSecret string
	RedirectURL  string
}

// StateStore is the subset of the persistent state store GitHub OAuth,
// repository protection, and backup job/history need. It is an interface so
// handlers are testable without a real state file.
type StateStore interface {
	GitHubConnection() (state.GitHubConnection, bool)
	SetGitHubConnection(state.GitHubConnection)
	ClearGitHubConnection()
	ProtectedRepos() []state.ProtectedRepo
	ProtectedRepo(id string) (state.ProtectedRepo, bool)
	AddProtectedRepo(r state.ProtectedRepo) error
	RemoveProtectedRepo(id string) error
	// Backup jobs and history (Phase 3).
	BackupJobs() []state.BackupJob
	CreateBackupJob(j state.BackupJob)
	UpdateBackupJob(j state.BackupJob) error
	RemoveBackupJob(id string) error
	BackupJob(id string) (state.BackupJob, bool)
	UnfinishedJobs() []state.BackupJob
	AddBackupRecord(rec state.BackupRecord)
	UpdateBackupRecord(rec state.BackupRecord) error
	RemoveBackupRecord(id string) error
	BackupRecords() []state.BackupRecord
	BackupRecordsForRepo(protectedRepoID string) []state.BackupRecord
	Save() error
}

// TokenStore is the subset of the token store GitHub OAuth needs.
type TokenStore interface {
	Set(ref, value string) error
	Get(ref string) (string, error)
	Delete(ref string) error
}

const githubClientSecretEnv = "GITSAFE_GITHUB_CLIENT_SECRET"

// GitHubOAuthFromEnv returns the OAuth configuration from non-secret settings
// and the client secret read from the environment. Returns (nil, false) when
// the server should run without GitHub (simply appearing disconnected).
func GitHubOAuthFromEnv(clientID, redirectURL string) (*GitHubOAuth, bool) {
	secret := os.Getenv(githubClientSecretEnv)
	if clientID == "" || secret == "" {
		return nil, false
	}
	oauth := &GitHubOAuth{
		ClientID:     clientID,
		ClientSecret: secret,
		RedirectURL:  redirectURL,
	}
	// Absent RedirectURL falls back to the canonical callback below.
	if oauth.RedirectURL == "" {
		oauth.RedirectURL = oauthRedirectURL
	}
	return oauth, true
}

// oauthRedirectURL is the local callback the browser hits after GitHub returns.
// The server binds 127.0.0.1, so the callback uses the loopback address.
const oauthRedirectURL = "http://127.0.0.1:8080/api/auth/github/callback"

func (s *Server) githubClientFor(token string) providers.Client {
	return providers.NewGitHubProvider(token)
}

// ConfigureCloud wires the Phase 1 cloud integration (state, token store, and
// optional GitHub OAuth) into the server. When oauth is nil, GitHub simply
// appears disconnected and its endpoints report 503.
func (s *Server) ConfigureCloud(stateStore StateStore, tokenStore TokenStore, oauth *GitHubOAuth) {
	s.stateStore = stateStore
	s.tokenStore = tokenStore
	s.github = oauth
	if oauth != nil {
		s.connectGitHub = func(ctx context.Context, code string) (string, []string, providers.Identity, error) {
			oauthClient := githuboauth.New(githuboauth.Config{
				ClientID:     oauth.ClientID,
				ClientSecret: oauth.ClientSecret,
				RedirectURL:  oauth.RedirectURL,
			})
			token, _, err := oauthClient.Exchange(ctx, code)
			if err != nil {
				return "", nil, providers.Identity{}, err
			}
			identity, err := providers.NewGitHubProvider(token).Identity(ctx)
			if err != nil {
				return "", nil, providers.Identity{}, err
			}
			return token, identity.Scopes, identity, nil
		}
	}
}

// handleGitHubLogin begins the OAuth flow: mint a session, bind a single-use
// OAuth state to it, and redirect to GitHub.
func (s *Server) handleGitHubLogin(w http.ResponseWriter, r *http.Request) {
	if s.github == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "GitHub integration is not configured.",
		})
		return
	}
	sess := s.ensureSession(w, r)
	stateVal := randToken(32)
	sess.ghState = stateVal
	s.sessions.put(sess)

	oauth := githuboauth.New(githuboauth.Config{
		ClientID:     s.github.ClientID,
		ClientSecret: s.github.ClientSecret,
		RedirectURL:  s.github.RedirectURL,
		Scopes:       []string{providers.ScopeRepo},
	})
	http.Redirect(w, r, oauth.AuthorizeURL(stateVal), http.StatusFound)
}

// handleGitHubCallback completes the OAuth flow: validate the single-use state,
// exchange the code for a token, resolve the account identity, and persist the
// connection (token in the keychain, reference in state).
func (s *Server) handleGitHubCallback(w http.ResponseWriter, r *http.Request) {
	if s.github == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "GitHub integration is not configured.",
		})
		return
	}
	sess := s.sessionFromRequest(r)
	if sess == nil {
		s.oauthError(w, "session expired; start over")
		return
	}

	stateVal := r.URL.Query().Get("state")
	if !s.sessions.consumeGitHubState(sess.id, stateVal) {
		s.oauthError(w, "invalid or reused OAuth state")
		return
	}

	code := r.URL.Query().Get("code")
	if code == "" {
		// A missing code with error_* params means the user declined.
		s.oauthError(w, "authorization was declined or failed")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	token, _, identity, err := s.connectGitHub(ctx, code)
	if err != nil {
		s.oauthError(w, "GitHub connection failed: "+err.Error())
		return
	}

	if !hasScope(identity.Scopes, providers.ScopeRepo) {
		s.oauthError(w, "the required 'repo' scope was not granted")
		return
	}

	if err := s.tokenStore.Set(tokenRefGitHub, token); err != nil {
		s.oauthError(w, "could not store the access token securely")
		return
	}

	s.stateStore.SetGitHubConnection(state.GitHubConnection{
		GitHubID:    identity.ID,
		Login:       identity.Login,
		Name:        identity.Name,
		AvatarURL:   identity.AvatarURL,
		Scopes:      identity.Scopes,
		ConnectedAt: time.Now(),
		TokenRef:    tokenRefGitHub,
	})
	if err := s.saveState(); err != nil {
		s.oauthError(w, "could not persist the connection state")
		return
	}

	http.Redirect(w, r, "/", http.StatusFound)
}

// handleAPIConnections lists the current cloud connections.
func (s *Server) handleAPIConnections(w http.ResponseWriter, r *http.Request) {
	github := map[string]any{
		"connected":  false,
		"provider":   "github",
		"configured": s.github != nil,
	}
	if s.stateStore != nil {
		if conn, ok := s.stateStore.GitHubConnection(); ok {
			github["connected"] = true
			github["login"] = conn.Login
			github["githubId"] = conn.GitHubID
			github["scopes"] = conn.Scopes
		}
	}
	cfg := s.app.Config.Cloud
	driveConfigured := cfg.Enabled && cfg.CredentialsFile != ""
	drive := map[string]any{
		"connected":  driveConfigured,
		"configured": driveConfigured,
	}
	if driveConfigured {
		drive["credentialsFile"] = cfg.CredentialsFile
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"github": github,
		"drive":  drive,
	})
}

// handleDisconnectGitHub removes the GitHub token and connection.
func (s *Server) handleDisconnectGitHub(w http.ResponseWriter, r *http.Request) {
	sess := s.sessionFromRequest(r)
	if sess == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{
			"error": "No active session. Refresh the page and try again.",
		})
		return
	}
	if !validateCSRF(sess, r.Header.Get("X-CSRF-Token")) {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error": "CSRF validation failed.",
		})
		return
	}

	if conn, ok := s.stateStore.GitHubConnection(); ok {
		if err := s.tokenStore.Delete(conn.TokenRef); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{
				"error": "could not remove the access token",
			})
			return
		}
		s.stateStore.ClearGitHubConnection()
	}
	if err := s.saveState(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "could not persist the disconnect",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "disconnected"})
}

// handleCSRF issues the session's CSRF token. It must be fetched (establishing a
// session) before mutating endpoints that require X-CSRF-Token.
func (s *Server) handleCSRF(w http.ResponseWriter, r *http.Request) {
	sess := s.ensureSession(w, r)
	writeJSON(w, http.StatusOK, map[string]string{"csrfToken": sess.csrf})
}

// saveState persists the state store, surfacing an error for the caller.
func (s *Server) saveState() error {
	return s.stateStore.Save()
}

// tokenRefGitHub is the tokenstore reference used for the GitHub primary token.
const tokenRefGitHub = "github.primary"

// hasScope reports whether scopes contains the required scope.
func hasScope(scopes []string, required string) bool {
	for _, s := range scopes {
		if strings.TrimSpace(s) == required {
			return true
		}
	}
	return false
}

// oauthError renders a plain-text error page and logs it without any token.
func (s *Server) oauthError(w http.ResponseWriter, msg string) {
	s.logger.Warn("github oauth", "error", msg)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusBadRequest)
	fmt.Fprintf(w, "GitHub connection failed: %s\n", msg)
}
