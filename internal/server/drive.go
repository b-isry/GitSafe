package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"google.golang.org/api/drive/v3"

	"github.com/b-isry/gitsafe/internal/cloud"
	"github.com/b-isry/gitsafe/internal/driveoauth"
	"github.com/b-isry/gitsafe/internal/state"
	"github.com/b-isry/gitsafe/internal/tokenstore"
)

// DriveOAuth is the Google OAuth application configuration used by the server.
// ClientID and RedirectURL are non-secret. ClientSecret is supplied separately
// from the environment and never serialized.
type DriveOAuth struct {
	ClientID     string
	ClientSecret string
	RedirectURL  string
}

const (
	// driveClientSecretEnv is the deployment-level Google OAuth client secret.
	// It is deliberately NOT part of user configuration and is never written to
	// config files, logs, cookies, or API responses.
	driveClientSecretEnv = "GITSAFE_DRIVE_CLIENT_SECRET"
	// driveClientIDEnv is the deployment-level Google OAuth client id. It is
	// non-secret (it appears in the authorization URL by design) but still
	// deployment-owned: the UI never surfaces it.
	driveClientIDEnv = "GITSAFE_DRIVE_CLIENT_ID"
)

// DriveOAuthFromEnv returns the OAuth configuration from non-secret settings
// and the client secret read from the environment. Returns (nil, false) when
// the server should run without a Google Drive connection (appearing simply
// not connected to the user). The client id may come from the environment
// variable first, falling back to the configured value (config file).
// baseURL is the application's base URL (e.g. https://gitsafe.example.com or
// http://127.0.0.1:8080). It is used to construct the Drive callback URL.
func DriveOAuthFromEnv(clientID, baseURL string) (*DriveOAuth, bool) {
	if id := os.Getenv(driveClientIDEnv); id != "" {
		clientID = id
	}
	secret := os.Getenv(driveClientSecretEnv)
	if clientID == "" || secret == "" {
		return nil, false
	}
	redirectURL := strings.TrimSuffix(baseURL, "/") + "/api/auth/drive/callback"
	oauth := &DriveOAuth{
		ClientID:     clientID,
		ClientSecret: secret,
		RedirectURL:  redirectURL,
	}
	return oauth, true
}

// driveConnectResult is everything the Drive callback needs to persist the
// connection: the refresh token (kept in the keychain) plus the account email
// and GitSafe storage-folder ID resolved from the live access token.
type driveConnectResult struct {
	RefreshToken    string
	AccountEmail    string
	StorageFolderID string
}

// ConfigureDriveOAuth wires the Google Drive OAuth integration into the server.
// When oauth is nil, Drive simply appears not connected and its endpoints
// report 503. Configuring Drive also installs the production upload
// implementation that mints fresh access tokens from the stored refresh token.
func (s *Server) ConfigureDriveOAuth(oauth *DriveOAuth) {
	s.driveOAuth = oauth
	if oauth == nil {
		return
	}
	s.connectDrive = func(ctx context.Context, code string) (driveConnectResult, error) {
		return s.connectDriveOAuth(ctx, code)
	}
	s.driveUpload = s.defaultDriveUpload
	// Google's revocation endpoint authenticates with the token itself (no
	// client credentials), so a registered application is enough to revoke.
	s.driveRevoke = func(ctx context.Context, token string) error {
		return driveoauth.New(driveoauth.Config{
			ClientID:     oauth.ClientID,
			ClientSecret: oauth.ClientSecret,
		}).Revoke(ctx, token)
	}
}

// connectDriveOAuth exchanges the authorization code for a refresh token, then
// uses it immediately to resolve the account identity and ensure the GitSafe
// storage folder exists. All of this happens behind the connectDrive seam so
// tests never touch the Google network.
func (s *Server) connectDriveOAuth(ctx context.Context, code string) (driveConnectResult, error) {
	oauth := s.driveOAuth
	client := driveoauth.New(driveoauth.Config{
		ClientID:     oauth.ClientID,
		ClientSecret: oauth.ClientSecret,
		RedirectURL:  oauth.RedirectURL,
	})
	tok, err := client.Exchange(ctx, code)
	if err != nil {
		return driveConnectResult{}, err
	}
	if tok.RefreshToken == "" {
		return driveConnectResult{}, errors.New("drive oauth: no refresh token granted")
	}
	// Use the freshly minted access token to resolve the account and folder.
	src := client.TokenSource(ctx, tok.RefreshToken)
	at, err := src.Token()
	if err != nil {
		return driveConnectResult{}, fmt.Errorf("drive oauth: refresh failed: %w", err)
	}
	svc, err := cloud.NewServiceFromToken(ctx, at.AccessToken)
	if err != nil {
		return driveConnectResult{}, err
	}
	about, err := svc.About.Get().Fields("user").Do()
	if err != nil {
		return driveConnectResult{}, fmt.Errorf("drive oauth: resolve account: %w", err)
	}
	folderID, err := cloud.EnsureGitSafeFolder(ctx, svc)
	if err != nil {
		return driveConnectResult{}, err
	}
	return driveConnectResult{
		RefreshToken:    tok.RefreshToken,
		AccountEmail:    about.User.EmailAddress,
		StorageFolderID: folderID,
	}, nil
}

// handleDriveLogin begins the Drive OAuth flow: bind a signed, single-use
// OAuth state (tied to the signed-in user) to this browser and redirect to
// Google. When the deployment has not configured a Google application, the
// user gets a graceful page.
func (s *Server) handleDriveLogin(w http.ResponseWriter, r *http.Request) {
	if s.driveOAuth == nil {
		s.writeDriveUnavailable(w, "Google Drive sign-in is not available right now.", "This can be enabled by your GitSafe administrator. Please try again later.")
		return
	}
	if s.redirectToCanonicalHost(w, r, "/api/auth/drive") {
		return
	}
	var userID int64
	if sess := s.sessionFromRequest(r); sess != nil {
		userID = sess.userID
	}
	stateVal := s.startOAuthFlow(w, "drive", userID)

	oauth := driveoauth.New(driveoauth.Config{
		ClientID:     s.driveOAuth.ClientID,
		ClientSecret: s.driveOAuth.ClientSecret,
		RedirectURL:  s.driveOAuth.RedirectURL,
	})
	http.Redirect(w, r, oauth.AuthorizeURL(stateVal), http.StatusFound)
}

// handleDriveCallback completes the Drive OAuth flow: validate the single-use
// state (it must belong to the signed-in user), exchange the code for a
// refresh token, resolve the account identity and storage folder, and persist
// the connection (refresh token encrypted in the token store, reference in
// state). Users see friendly, specific messages and never raw OAuth errors or
// secrets.
func (s *Server) handleDriveCallback(w http.ResponseWriter, r *http.Request) {
	if s.driveOAuth == nil {
		s.writeDriveUnavailable(w, "Google Drive sign-in is not available right now.", "This can be enabled by your GitSafe administrator. Please try again later.")
		return
	}
	userID, stateStore, tokenStore, _, err := s.requireUser(r)
	if err != nil {
		s.logger.Warn("drive oauth", "error", "not signed in", "host", r.Host)
		s.renderAuthError(w, "github", "Please sign in with GitHub first",
			"Your GitSafe session has ended. Sign in with GitHub, then connect Google Drive again.")
		return
	}
	st, reason, ok := s.finishOAuthFlow(w, r, "drive")
	if ok && st.UserID != userID {
		ok, reason = false, "state_user_mismatch"
	}
	if !ok {
		s.logger.Warn("drive oauth", "error", "sign-in state rejected", "reason", reason, "host", r.Host)
		s.renderAuthError(w, "drive", "Your Google sign-in timed out or was already used",
			"This can happen if the page was reloaded, the sign-in was started in another browser, or it took longer than 15 minutes. Nothing was changed. Please try again.")
		return
	}

	code := r.URL.Query().Get("code")
	if code == "" {
		if r.URL.Query().Get("error") == "access_denied" {
			s.driveError(w, "Google authorization was cancelled.")
			return
		}
		s.driveError(w, "Google did not return an authorization code. Please try again.")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	result, err := s.connectDrive(ctx, code)
	if err != nil {
		if errors.Is(err, driveoauth.ErrDenied) {
			s.driveError(w, "Google authorization was cancelled.")
			return
		}
		s.logger.Warn("drive oauth callback failed", "error", err)
		s.driveError(w, "Could not finish connecting to Google Drive. Please try again.")
		return
	}

	if err := tokenStore.Set(tokenstore.DriveTokenFor(userID), result.RefreshToken); err != nil {
		s.logger.Error("drive oauth: store refresh token", "cause", err)
		s.driveError(w, "Could not store the access token securely. Please try again.")
		return
	}

	stateStore.SetDriveConnection(state.DriveConnection{
		AccountEmail:    result.AccountEmail,
		ConnectedAt:     time.Now(),
		StorageFolderID: result.StorageFolderID,
		TokenRef:        tokenstore.DriveTokenFor(userID),
	})
	if err := s.saveState(stateStore); err != nil {
		s.logger.Error("drive oauth: persist connection state", "cause", err)
		s.driveError(w, "Could not save the connection. Please try again.")
		return
	}
	s.afterDriveConnected(r.Context(), userID)

	http.Redirect(w, r, "/", http.StatusFound)
}

// afterDriveConnected is called after a successful Drive (re)connection.
func (s *Server) afterDriveConnected(ctx context.Context, userID int64) {
	s.clearDailyReauth(ctx, userID, reauthDrive)
}

// handleDisconnectDrive removes the Drive refresh token and connection. Local
// credential removal is the security boundary and always runs; server-side
// token revocation via Google's API is a best-effort cleanup and a revocation
// failure never blocks the disconnect.
func (s *Server) handleDisconnectDrive(w http.ResponseWriter, r *http.Request) {
	userID, stateStore, tokenStore, sess, err := s.requireUser(r)
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

	var refreshToken string
	if conn, ok := stateStore.DriveConnection(); ok {
		if t, err := tokenStore.Get(conn.TokenRef); err == nil {
			refreshToken = t
		}
		if err := tokenStore.Delete(conn.TokenRef); err != nil {
			s.logger.Error("drive oauth: delete refresh token", "cause", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{
				"error": "could not remove the access token",
			})
			return
		}
		stateStore.ClearDriveConnection()
	}
	if err := s.saveState(stateStore); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "could not persist the disconnect",
		})
		return
	}

	if refreshToken != "" && s.driveRevoke != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		if err := s.driveRevoke(ctx, refreshToken); err != nil {
			s.logger.Warn("drive oauth: token revocation failed (local credential already removed)", "error", err)
		}
	}
	s.disableDaily(r.Context(), userID)
	writeJSON(w, http.StatusOK, map[string]string{"status": "disconnected"})
}

// --- drive connection state helpers ---

// driveConfigured reports whether the deployment has registered a Google OAuth
// application (i.e. connecting Drive is possible).
func (s *Server) driveConfigured() bool {
	return s.driveOAuth != nil
}

// driveConnection returns the persisted Drive connection, if any.
func (s *Server) driveConnection(stateStore StateStore) (state.DriveConnection, bool) {
	if stateStore == nil {
		return state.DriveConnection{}, false
	}
	return stateStore.DriveConnection()
}

// driveConnected reports whether a Drive account is actually connected AND the
// deployment is configured to support it.
func (s *Server) driveConnected(stateStore StateStore) bool {
	if !s.driveConfigured() {
		return false
	}
	_, ok := s.driveConnection(stateStore)
	return ok
}

// driveRefreshToken returns the stored refresh token for the connected Drive
// account, or an error (with a user-facing message) when Drive is not usable.
func (s *Server) driveRefreshToken(stateStore StateStore, tokenStore TokenStore) (string, error) {
	conn, ok := s.driveConnection(stateStore)
	if !ok {
		return "", errDriveNotConnected
	}
	token, err := tokenStore.Get(conn.TokenRef)
	if err != nil {
		return "", fmt.Errorf("Google Drive access token is unavailable: %w", err)
	}
	if token == "" {
		return "", fmt.Errorf("Google Drive access token is unavailable")
	}
	return token, nil
}

// errDriveNotConnected is returned when a backup is requested without a
// connected Google Drive account and is surfaced as a clear user-facing error.
var errDriveNotConnected = errors.New("Google Drive is not connected")

// --- defaults wired by ConfigureDriveOAuth ---

// defaultDriveUpload uploads a completed local bundle into the connected Drive
// account's GitSafe folder, minting a fresh access token from the stored
// refresh token on demand. Every GitSafe backup lives inside the GitSafe
// folder: when the persisted StorageFolderID is empty (a legacy connection
// created before folder resolution) the folder is resolved by name at upload
// time so uploads never fall into the Drive root. When existingFileID still
// exists (and is not trashed) it is replaced in place. Real byte progress is
// reported through onProgress when the caller provides it.
func (s *Server) defaultDriveUpload(ctx context.Context, bundlePath, folderID, existingFileID, refreshToken string, onProgress func(now, total int64), logger *slog.Logger) (string, error) {
	svc, err := s.driveService(ctx, refreshToken)
	if err != nil {
		return "", err
	}
	if folderID == "" {
		folderID, err = cloud.EnsureGitSafeFolder(ctx, svc)
		if err != nil {
			return "", fmt.Errorf("resolve gitsafe folder: %w", err)
		}
	}
	return cloud.UploadOrReplaceFile(ctx, svc, bundlePath, folderID, existingFileID, logger, onProgress)
}

// driveService mints an access token from the stored refresh token and builds
// a Drive client. A refresh token that Google rejects as invalid_grant (the
// user revoked access, or the OAuth app is in "Testing" mode and the token
// expired after 7 days) is reported as errDriveAuth.
func (s *Server) driveService(ctx context.Context, refreshToken string) (*drive.Service, error) {
	if s.driveOAuth == nil {
		return nil, errDriveNotConnected
	}
	client := driveoauth.New(driveoauth.Config{
		ClientID:     s.driveOAuth.ClientID,
		ClientSecret: s.driveOAuth.ClientSecret,
		RedirectURL:  s.driveOAuth.RedirectURL,
	})
	tok, err := client.TokenSource(ctx, refreshToken).Token()
	if err != nil {
		var rerr *oauth2.RetrieveError
		if errors.As(err, &rerr) && (rerr.ErrorCode == "invalid_grant" || rerr.ErrorCode == "unauthorized_client") {
			return nil, fmt.Errorf("%w: %s", errDriveAuth, rerr.ErrorCode)
		}
		return nil, fmt.Errorf("drive oauth: refresh failed: %w", err)
	}
	return cloud.NewServiceFromToken(ctx, tok.AccessToken)
}

// writeDriveUnavailable renders a self-contained, user-facing page explaining
// that Google Drive sign-in cannot be started right now. It must never mention
// environment variables, client ids/secrets, or developer setup steps.
func (s *Server) writeDriveUnavailable(w http.ResponseWriter, title, body string) {
	s.logger.Info("drive oauth unconfigured")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusServiceUnavailable)
	fmt.Fprintf(w, driveUnavailableHTML, title, body)
}

const driveUnavailableHTML = `<!doctype html>
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

// driveError renders the friendly Drive connection error page (with a retry
// button) and logs the message without any token.
func (s *Server) driveError(w http.ResponseWriter, msg string) {
	s.logger.Warn("drive oauth", "error", msg)
	s.renderAuthError(w, "drive", "Google Drive connection failed", msg)
}
