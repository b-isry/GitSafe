package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/b-isry/gitsafe/internal/tokenstore"
)

// --- Drive OAuth flow ---

func TestDriveLoginRedirects(t *testing.T) {
	s := envCloudServer(t, &fakeStateStore{}, true)
	login := request(t, s, http.MethodGet, "/api/auth/drive", nil)
	if login.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", login.Code)
	}
	loc := login.Header().Get("Location")
	if !strings.Contains(loc, "accounts.google.com/o/oauth2/auth") {
		t.Fatalf("Location not a Google authorize URL: %q", loc)
	}
	u := mustParse(t, loc)
	if u.Query().Get("state") == "" {
		t.Fatal("authorize URL missing state parameter")
	}
	if u.Query().Get("access_type") != "offline" || u.Query().Get("prompt") != "consent" {
		t.Fatalf("authorize query missing offline/consent: %q", u.RawQuery)
	}
}

func TestDriveLoginNotConfigured(t *testing.T) {
	s := newCloudServer(t, &fakeStateStore{}, newFakeTokenStore(), &GitHubOAuth{ClientID: "id"})
	rec := request(t, s, http.MethodGet, "/api/auth/drive", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "not available") {
		t.Fatal("expected a friendly unconfigured page")
	}
}

func TestDriveCallbackExchangesAndConnects(t *testing.T) {
	st := &fakeStateStore{}
	s := envCloudServer(t, st, true)
	s.connectDrive = func(ctx context.Context, code string) (driveConnectResult, error) {
		if code != "good-code" {
			t.Fatalf("connectDrive code = %q, want good-code", code)
		}
		return driveConnectResult{
			RefreshToken:    "rt-123",
			AccountEmail:    "test@example.com",
			StorageFolderID: "folder-abc",
		}, nil
	}

	cookie, stateVal := driveLoginSession(t, s)
	callback := requestWithCSRF(t, s, http.MethodGet,
		"/api/auth/drive/callback?code=good-code&state="+url.QueryEscape(stateVal),
		cookie, "")
	if callback.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302: %s", callback.Code, callback.Body.String())
	}
	if callback.Header().Get("Location") != "/" {
		t.Fatalf("Location = %q, want /", callback.Header().Get("Location"))
	}

	tk := s.tokenStore.(*fakeTokenStore)
	if v, err := tk.Get(tokenstore.DriveToken); err != nil || v != "rt-123" {
		t.Fatalf("token store = %q, err=%v", v, err)
	}
	conn, ok := st.DriveConnection()
	if !ok {
		t.Fatal("expected a DriveConnection")
	}
	if conn.AccountEmail != "test@example.com" || conn.StorageFolderID != "folder-abc" {
		t.Fatalf("drive connection = %+v", conn)
	}
}

func TestDriveCallbackInvalidState(t *testing.T) {
	s := envCloudServer(t, &fakeStateStore{}, true)
	cookie, _ := driveLoginSession(t, s)
	rec := requestWithCSRF(t, s, http.MethodGet,
		"/api/auth/drive/callback?code=x&state=wrong", cookie, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "invalid or reused") {
		t.Fatalf("error body = %q", rec.Body.String())
	}
}

func TestDriveCallbackAccessDenied(t *testing.T) {
	s := envCloudServer(t, &fakeStateStore{}, true)
	cookie, stateVal := driveLoginSession(t, s)
	rec := requestWithCSRF(t, s, http.MethodGet,
		"/api/auth/drive/callback?error=access_denied&state="+url.QueryEscape(stateVal),
		cookie, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "cancelled") {
		t.Fatalf("error body = %q", rec.Body.String())
	}
}

func TestDriveCallbackExchangeError(t *testing.T) {
	s := envCloudServer(t, &fakeStateStore{}, true)
	s.connectDrive = func(ctx context.Context, code string) (driveConnectResult, error) {
		return driveConnectResult{}, errBoom
	}
	cookie, stateVal := driveLoginSession(t, s)
	rec := requestWithCSRF(t, s, http.MethodGet,
		"/api/auth/drive/callback?code=bad&state="+url.QueryEscape(stateVal),
		cookie, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "try again") {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

// --- Drive disconnect ---

func TestDriveDisconnectRemovesTokenAndConnection(t *testing.T) {
	st := &fakeStateStore{}
	s := envCloudServer(t, st, true)
	revoked := ""
	s.driveRevoke = func(ctx context.Context, token string) error {
		revoked = token
		return nil
	}
	cookie, csrf := csrfCookie(t, s)
	rec := requestWithCSRF(t, s, http.MethodDelete, "/api/connections/drive", cookie, csrf)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["status"] != "disconnected" {
		t.Fatalf("body = %+v", body)
	}
	tk := s.tokenStore.(*fakeTokenStore)
	if _, err := tk.Get(tokenstore.DriveToken); err == nil {
		t.Fatal("token should have been deleted")
	}
	if _, ok := st.DriveConnection(); ok {
		t.Fatal("connection should have been cleared")
	}
	if revoked != "drv-refresh" {
		t.Fatalf("revoked = %q, want drv-refresh", revoked)
	}
}

func TestDriveDisconnectNoCSRF(t *testing.T) {
	s := envCloudServer(t, &fakeStateStore{}, true)
	s.driveRevoke = func(ctx context.Context, token string) error {
		t.Fatal("revoke must not run without CSRF")
		return nil
	}
	cookie, _ := csrfCookie(t, s)
	rec := requestWithCSRF(t, s, http.MethodDelete, "/api/connections/drive", cookie, "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if _, ok := s.stateStore.DriveConnection(); !ok {
		t.Fatal("connection must survive a rejected disconnect")
	}
}

// --- Drive-only backup enforcement ---

func TestBackupProtectedRepoDriveNotConnected(t *testing.T) {
	st := &fakeStateStore{}
	seedProtectedRepo(st, "p1", "acme/alpha", "main")
	s := envCloudServer(t, st, false)
	cookie, csrf := csrfCookie(t, s)
	rec := postProtectedBackup(t, s, cookie, csrf, "p1")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	errMsg, _ := body["error"].(string)
	if !strings.Contains(errMsg, "Drive") {
		t.Fatalf("error body = %q", errMsg)
	}
	if len(st.BackupJobs()) != 0 {
		t.Fatalf("no job should be created without Drive, got %+v", st.BackupJobs())
	}
}

// --- helpers ---

// driveLoginSession starts a Drive login and captures the session cookie and
// OAuth state value so a callback can be simulated without hitting Google.
func driveLoginSession(t *testing.T, s *Server) (*http.Cookie, string) {
	t.Helper()
	login := request(t, s, http.MethodGet, "/api/auth/drive", nil)
	if login.Code != http.StatusFound {
		t.Fatalf("drive login status = %d, want 302", login.Code)
	}
	u := mustParse(t, login.Header().Get("Location"))
	stateVal := u.Query().Get("state")
	if stateVal == "" {
		t.Fatal("no state in authorize URL")
	}
	var cookie *http.Cookie
	for _, c := range login.Result().Cookies() {
		if c.Name == sessionCookieName {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("no session cookie set")
	}
	return cookie, stateVal
}

// requestWithCSRF is like request() but includes an X-CSRF-Token header.
func requestWithCSRF(t *testing.T, s *Server, method, path string, cookie *http.Cookie, csrf string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	req.RemoteAddr = "127.0.0.1:55555"
	if cookie != nil {
		req.AddCookie(cookie)
	}
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	return rec
}

// TestDriveOAuthFromEnvCallbackURL verifies the Drive OAuth callback URL is
// constructed from the provided base URL.
func TestDriveOAuthFromEnvCallbackURL(t *testing.T) {
	t.Setenv(driveClientIDEnv, "")
	t.Setenv(driveClientSecretEnv, "")

	t.Run("defaults to local when empty", func(t *testing.T) {
		t.Setenv(driveClientIDEnv, "")
		t.Setenv(driveClientSecretEnv, "secret")
		// Pass a clientID in the argument since env var is empty
		oauth, ok := DriveOAuthFromEnv("test-client", "http://127.0.0.1:8080")
		if !ok {
			t.Fatal("expected configured")
		}
		if oauth.RedirectURL != "http://127.0.0.1:8080/api/auth/drive/callback" {
			t.Fatalf("RedirectURL = %q, want %q", oauth.RedirectURL, "http://127.0.0.1:8080/api/auth/drive/callback")
		}
	})

	t.Run("uses production base URL", func(t *testing.T) {
		t.Setenv(driveClientIDEnv, "prod-client")
		t.Setenv(driveClientSecretEnv, "prod-secret")
		oauth, ok := DriveOAuthFromEnv("", "https://gitsafe.onrender.com")
		if !ok {
			t.Fatal("expected configured")
		}
		if oauth.RedirectURL != "https://gitsafe.onrender.com/api/auth/drive/callback" {
			t.Fatalf("RedirectURL = %q, want %q", oauth.RedirectURL, "https://gitsafe.onrender.com/api/auth/drive/callback")
		}
		// Ensure no accidental 127.0.0.1
		if strings.Contains(oauth.RedirectURL, "127.0.0.1") {
			t.Fatalf("RedirectURL must not contain 127.0.0.1: %q", oauth.RedirectURL)
		}
	})

	t.Run("strips trailing slash from base URL", func(t *testing.T) {
		t.Setenv(driveClientIDEnv, "client")
		t.Setenv(driveClientSecretEnv, "secret")
		oauth, ok := DriveOAuthFromEnv("", "https://example.com/")
		if !ok {
			t.Fatal("expected configured")
		}
		if oauth.RedirectURL != "https://example.com/api/auth/drive/callback" {
			t.Fatalf("RedirectURL = %q, want %q", oauth.RedirectURL, "https://example.com/api/auth/drive/callback")
		}
	})

	t.Run("env takes precedence for client id", func(t *testing.T) {
		t.Setenv(driveClientIDEnv, "env-client")
		t.Setenv(driveClientSecretEnv, "env-secret")
		oauth, ok := DriveOAuthFromEnv("config-client", "http://127.0.0.1:8080")
		if !ok {
			t.Fatal("expected configured")
		}
		if oauth.ClientID != "env-client" {
			t.Fatalf("ClientID = %q, want env-client", oauth.ClientID)
		}
	})

	t.Run("requires client secret from env", func(t *testing.T) {
		t.Setenv(driveClientIDEnv, "client")
		t.Setenv(driveClientSecretEnv, "")
		if _, ok := DriveOAuthFromEnv("client", "http://127.0.0.1:8080"); ok {
			t.Fatal("missing secret must yield unconfigured")
		}
	})
}
