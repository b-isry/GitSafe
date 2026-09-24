package server

import (
	"context"
	"errors"
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
	DriveConnection() (state.DriveConnection, bool)
	SetDriveConnection(state.DriveConnection)
	ClearDriveConnection()
	ClearAll()
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

const (
	// githubClientSecretEnv is the deployment-level GitHub OAuth client secret.
	// It is deliberately NOT part of user configuration: GitSafe operators own
	// it, the end user never sees it, and it is never written to config files,
	// logs, cookies, or API responses.
	githubClientSecretEnv = "GITSAFE_GITHUB_CLIENT_SECRET"
	// githubClientIDEnv is the deployment-level GitHub OAuth client id. It is
	// non-secret (it appears in the authorization URL by design) but still
	// deployment-owned: the UI never surfaces it.
	githubClientIDEnv = "GITSAFE_GITHUB_CLIENT_ID"
)

// GitHubOAuthFromEnv returns the OAuth configuration from non-secret settings
// and the client secret read from the environment. Returns (nil, false) when
// the server should run without GitHub (simply appearing disconnected to the
// user). The client id may come from the environment variable first, falling
// back to the configured value (config file), so operators can supply both
// credentials purely through the environment.
// baseURL is the application's base URL (e.g. https://gitsafe.onrender.com or
// http://127.0.0.1:8080). It is used to construct the GitHub callback URL.
func GitHubOAuthFromEnv(clientID, baseURL string) (*GitHubOAuth, bool) {
	if id := os.Getenv(githubClientIDEnv); id != "" {
		clientID = id
	}
	secret := os.Getenv(githubClientSecretEnv)
	if clientID == "" || secret == "" {
		return nil, false
	}
	redirectURL := strings.TrimSuffix(baseURL, "/") + "/api/auth/github/callback"
	oauth := &GitHubOAuth{
		ClientID:     clientID,
		ClientSecret: secret,
		RedirectURL:  redirectURL,
	}
	return oauth, true
}

func (s *Server) githubClientFor(token string) providers.Client {
	return providers.NewGitHubProvider(token)
}

// ConfigureCloud wires the Phase 1 GitHub OAuth into the server.
// When oauth is nil, GitHub simply appears disconnected and its endpoints report 503.
// Per-user state and token stores are managed by the UserStoreManager and accessed
// via requireUser on each request.
func (s *Server) ConfigureCloud(oauth *GitHubOAuth) {
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
		// Server-side token revocation needs the client secret; without one it
		// is skipped (it is best-effort anyway — local removal is the boundary).
		if oauth.ClientSecret != "" {
			s.revokeToken = func(ctx context.Context, token string) error {
				return githuboauth.New(githuboauth.Config{
					ClientID:     oauth.ClientID,
					ClientSecret: oauth.ClientSecret,
				}).Revoke(ctx, token)
			}
		}
	}
}

// handleGitHubLogin begins the OAuth flow: mint a session, bind a single-use
// OAuth state to it, and redirect to GitHub. When the deployment has not
// configured a GitHub application, the user gets a graceful page — never
// developer-setup instructions or raw errors.
func (s *Server) handleGitHubLogin(w http.ResponseWriter, r *http.Request) {
	if s.github == nil {
		s.writeGitHubUnavailable(w, "GitHub sign-in is not available right now.", "This can be enabled by your GitSafe administrator. Please try again later.")
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
// connection (token in the keychain, reference in state). Users see friendly,
// specific messages (e.g. "authorization was cancelled") and never raw OAuth
// errors, environment variables, or secrets.
func (s *Server) handleGitHubCallback(w http.ResponseWriter, r *http.Request) {
	if s.github == nil {
		s.writeGitHubUnavailable(w, "GitHub sign-in is not available right now.", "This can be enabled by your GitSafe administrator. Please try again later.")
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
		// A declined authorization comes back without a code, with an error
		// query parameter. Anything else missing a code is treated generically.
		if r.URL.Query().Get("error") == "access_denied" {
			s.oauthError(w, "GitHub authorization was cancelled.")
			return
		}
		s.oauthError(w, "GitHub did not return an authorization code. Please try again.")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	token, _, identity, err := s.connectGitHub(ctx, code)
	if err != nil {
		if errors.Is(err, githuboauth.ErrDenied) {
			s.oauthError(w, "GitHub authorization was cancelled.")
			return
		}
		s.logger.Warn("github oauth callback failed", "error", err)
		s.oauthError(w, "Could not finish connecting to GitHub. Please try again.")
		return
	}

	if !hasScope(identity.Scopes, providers.ScopeRepo) {
		s.oauthError(w, "the required 'repo' scope was not granted")
		return
	}

	// Create per-user stores for this user
	userID := identity.ID
	stores, err := s.userStores.getOrCreate(userID)
	if err != nil {
		s.oauthError(w, "could not create user stores")
		return
	}
	stateStore := stores.state
	tokenStore := stores.token

	// Store the token in the per-user token store
	if err := tokenStore.Set(tokenRefGitHub, token); err != nil {
		s.logger.Error("github oauth: store access token", "cause", err)
		s.oauthError(w, "could not store the access token securely")
		return
	}

	// Create GitHub connection in the per-user state store
	stateStore.SetGitHubConnection(state.GitHubConnection{
		GitHubID:    identity.ID,
		Login:       identity.Login,
		Name:        identity.Name,
		AvatarURL:   identity.AvatarURL,
		Scopes:      identity.Scopes,
		ConnectedAt: time.Now(),
		TokenRef:    tokenRefGitHub,
	})
	if err := s.saveState(stores.state); err != nil {
		s.logger.Error("github oauth: persist connection state", "cause", err)
		s.oauthError(w, "could not persist the connection state")
		return
	}

	// Set the user ID in the session
	sess.userID = userID
	s.sessions.put(sess)

	http.Redirect(w, r, "/", http.StatusFound)
}

// handleAPIConnections lists the current cloud connections.
func (s *Server) handleAPIConnections(w http.ResponseWriter, r *http.Request) {
	_, stateStore, _, _, err := s.requireUser(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "No active session. Refresh the page and try again."})
		return
	}

	github := map[string]any{
		"connected":  false,
		"provider":   "github",
		"configured": s.github != nil,
	}
	if conn, ok := stateStore.GitHubConnection(); ok {
		github["connected"] = true
		github["login"] = conn.Login
		github["githubId"] = conn.GitHubID
		github["scopes"] = conn.Scopes
	}
	drive := map[string]any{
		"connected":  s.driveConnected(stateStore),
		"configured": s.driveConfigured(),
	}
	if dc, ok := s.driveConnection(stateStore); ok {
		drive["accountEmail"] = dc.AccountEmail
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"github": github,
		"drive":  drive,
	})
}

// handleDisconnectGitHub removes the GitHub token and connection. Local
// credential removal is the security boundary and always runs; server-side
// token revocation via GitHub's API is a best-effort cleanup using the
// deployment's client credentials, and a revocation failure never blocks the
// disconnect.
func (s *Server) handleDisconnectGitHub(w http.ResponseWriter, r *http.Request) {
	_, stateStore, tokenStore, sess, err := s.requireUser(r)
	if err != nil {
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

	var token string
	if conn, ok := stateStore.GitHubConnection(); ok {
		if t, err := tokenStore.Get(conn.TokenRef); err == nil {
			token = t
		}
		if err := tokenStore.Delete(conn.TokenRef); err != nil {
			s.logger.Error("github oauth: delete access token", "cause", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{
				"error": "could not remove the access token",
			})
			return
		}
		stateStore.ClearGitHubConnection()
	}
	if err := s.saveState(stateStore); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "could not persist the disconnect",
		})
		return
	}

	// Best-effort server-side revocation. The local credential is already gone;
	// GitHub's endpoint authenticates with the app's own client credentials
	// (never the user's token as bearer auth), so a failure only loses cleanup.
	if token != "" && s.revokeToken != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		if err := s.revokeToken(ctx, token); err != nil {
			s.logger.Warn("github oauth: token revocation failed (local credential already removed)", "error", err)
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "disconnected"})
}

// writeGitHubUnavailable renders a self-contained, user-facing page explaining
// that GitHub sign-in cannot be started right now. It must never mention
// environment variables, client ids/secrets, or developer setup steps.
func (s *Server) writeGitHubUnavailable(w http.ResponseWriter, title, body string) {
	s.logger.Info("github oauth unconfigured")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusServiceUnavailable)
	fmt.Fprintf(w, githubUnavailableHTML, title, body)
}

const githubUnavailableHTML = `<!doctype html>
<html lang="en" data-theme="dark">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>GitSafe</title>
  <link rel="stylesheet" href="/static/styles.css">
</head>
<body>
  <div class="app">
    <header class="topbar-nav">
      <span class="topbar-nav__brand">
        <span class="brand__mark" aria-hidden="true"></span>
        <span class="brand__name">GitSafe</span>
      </span>
    </header>
    <main class="content">
      <div class="onboarding">
        <div class="hero-card">
          <h1 class="hero-card__title">%s</h1>
          <p class="hero-card__subtitle">%s</p>
          <a class="btn btn--primary" href="/" style="margin-top:1.5rem">Back to GitSafe</a>
        </div>
      </div>
    </main>
  </div>
</body>
</html>`

// handleCSRF issues the session's CSRF token. It must be fetched (establishing a
// session) before mutating endpoints that require X-CSRF-Token.
func (s *Server) handleCSRF(w http.ResponseWriter, r *http.Request) {
	sess := s.ensureSession(w, r)
	writeJSON(w, http.StatusOK, map[string]string{"csrfToken": sess.csrf})
}

// saveState persists the given state store, surfacing an error for the caller.
func (s *Server) saveState(stateStore StateStore) error {
	return stateStore.Save()
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
