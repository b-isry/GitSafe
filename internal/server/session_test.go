package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/b-isry/gitsafe/internal/providers"
)

func okGitHubConnect(ctx context.Context, code string) (string, []string, providers.Identity, error) {
	return "tok", []string{providers.ScopeRepo}, providers.Identity{ID: 42, Login: "u", Scopes: []string{providers.ScopeRepo}}, nil
}

func sessionCookieFrom(rec *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName && c.MaxAge > 0 {
			return c
		}
	}
	return nil
}

// TestOAuthSurvivesServerRestart is the regression test for the production
// warning `github oauth error="session expired; start over"`: the flow is
// started on one process and finished on a brand-new one (a Render free
// instance restarting or redeploying in between). With the same key
// material, the signed state cookie must still be accepted.
func TestOAuthSurvivesServerRestart(t *testing.T) {
	t.Setenv(SessionKeyEnv, "test-only-session-key-material")
	first := newCloudServer(t, nil, nil, &GitHubOAuth{ClientID: "id"})
	cookie, stateVal, _ := loginAndGetState(t, first)

	second := newCloudServer(t, nil, nil, &GitHubOAuth{ClientID: "id"})
	second.connectGitHub = okGitHubConnect
	rec := request(t, second, http.MethodGet, "/api/auth/github/callback?state="+stateVal+"&code=c", cookie)
	if rec.Code != http.StatusFound {
		t.Fatalf("callback after restart = %d, want 302: %s", rec.Code, rec.Body.String())
	}
	sc := sessionCookieFrom(rec)
	if sc == nil {
		t.Fatal("no session cookie set after sign-in")
	}
	// The session itself also survives a further restart.
	third := newCloudServer(t, nil, nil, &GitHubOAuth{ClientID: "id"})
	if sess := third.sessionFromRequest(withCookie(sc)); sess == nil || sess.userID != 42 {
		t.Fatalf("session not accepted after restart: %+v", sess)
	}
}

func withCookie(c *http.Cookie) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(c)
	return r
}

// TestOAuthStateRejectedWithDifferentKey pins that cookies are really signed.
func TestOAuthStateRejectedWithDifferentKey(t *testing.T) {
	t.Setenv(SessionKeyEnv, "key-one")
	first := newCloudServer(t, nil, nil, &GitHubOAuth{ClientID: "id"})
	cookie, stateVal, _ := loginAndGetState(t, first)
	t.Setenv(SessionKeyEnv, "key-two")
	second := newCloudServer(t, nil, nil, &GitHubOAuth{ClientID: "id"})
	second.connectGitHub = okGitHubConnect
	rec := request(t, second, http.MethodGet, "/api/auth/github/callback?state="+stateVal+"&code=c", cookie)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// TestOAuthMultipleTabs: starting a second sign-in must not break the first.
func TestOAuthMultipleTabs(t *testing.T) {
	s := newCloudServer(t, nil, nil, &GitHubOAuth{ClientID: "id"})
	s.connectGitHub = okGitHubConnect
	c1, st1, _ := loginAndGetState(t, s)
	c2, st2, _ := loginAndGetState(t, s)
	if c1.Name == c2.Name {
		t.Fatal("each flow must have its own state cookie")
	}
	rec := requestCookies(t, s, http.MethodGet, "/api/auth/github/callback?state="+st1+"&code=c", c1, c2)
	if rec.Code != http.StatusFound {
		t.Fatalf("first tab = %d, want 302", rec.Code)
	}
	rec = requestCookies(t, s, http.MethodGet, "/api/auth/github/callback?state="+st2+"&code=c", c2)
	if rec.Code != http.StatusFound {
		t.Fatalf("second tab = %d, want 302", rec.Code)
	}
}

func TestOAuthStateExpires(t *testing.T) {
	s := newCloudServer(t, nil, nil, &GitHubOAuth{ClientID: "id"})
	s.connectGitHub = okGitHubConnect
	cookie, stateVal, _ := loginAndGetState(t, s)
	s.sessions.now = func() time.Time { return time.Now().Add(oauthStateTTL + time.Minute) }
	rec := request(t, s, http.MethodGet, "/api/auth/github/callback?state="+stateVal+"&code=c", cookie)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// TestOAuthFailurePageIsFriendly: the error page is HTML with a retry button
// and no developer details.
func TestOAuthFailurePageIsFriendly(t *testing.T) {
	s := newCloudServer(t, nil, nil, &GitHubOAuth{ClientID: "id"})
	rec := request(t, s, http.MethodGet, "/api/auth/github/callback?state=deadbeefdeadbeefdeadbeef&code=c", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	body := rec.Body.String()
	if !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("content type = %q", rec.Header().Get("Content-Type"))
	}
	if !strings.Contains(body, `href="/api/auth/github"`) || !strings.Contains(body, "Try again") {
		t.Fatalf("missing retry button: %s", body)
	}
	if strings.Contains(body, "session expired; start over") {
		t.Fatal("old terse message still shown")
	}
	assertNoDevLeak(t, "oauth failure page", body)
}

// TestSessionCookieFlags: Secure on https deployments, always HttpOnly+Lax.
func TestSessionCookieFlags(t *testing.T) {
	s := newCloudServer(t, nil, nil, &GitHubOAuth{ClientID: "id"})
	s.sessions.secure = true
	s.connectGitHub = okGitHubConnect
	login := request(t, s, http.MethodGet, "/api/auth/github", nil)
	for _, c := range login.Result().Cookies() {
		if !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode {
			t.Fatalf("cookie %s flags: secure=%v httponly=%v samesite=%v", c.Name, c.Secure, c.HttpOnly, c.SameSite)
		}
		if c.Path != oauthStateCookiePath {
			t.Fatalf("state cookie path = %q", c.Path)
		}
	}
	csrf := request(t, s, http.MethodGet, "/api/csrf", nil)
	sc := sessionCookieFrom(csrf)
	if sc == nil || !sc.Secure || !sc.HttpOnly || sc.SameSite != http.SameSiteLaxMode {
		t.Fatalf("session cookie flags wrong: %+v", sc)
	}
	if newCloudServer(t, nil, nil, nil).sessions.secure {
		t.Fatal("http://127.0.0.1 base URL must not set Secure (browsers would drop it)")
	}
}

// TestSignInRotatesSession: the session id and CSRF token change at sign-in.
func TestSignInRotatesSession(t *testing.T) {
	s := newCloudServer(t, nil, nil, &GitHubOAuth{ClientID: "id"})
	s.connectGitHub = okGitHubConnect
	anon := anonSessionCookie(t, s)
	before, _ := s.sessions.get(anon.Value)
	cookie, stateVal, _ := loginAndGetState(t, s)
	rec := requestCookies(t, s, http.MethodGet, "/api/auth/github/callback?state="+stateVal+"&code=c", anon, cookie)
	after, ok := s.sessions.get(sessionCookieFrom(rec).Value)
	if !ok || after.userID != 42 || after.id == before.id || after.csrf == before.csrf {
		t.Fatalf("session not rotated: before=%+v after=%+v", before, after)
	}
}

func TestTamperedSessionCookieRejected(t *testing.T) {
	s := newCloudServer(t, nil, nil, nil)
	cookie, _ := authedCookie(t, s, 42)
	body, sig, _ := strings.Cut(cookie.Value, ".")
	forged := *cookie
	forged.Value = body + "x." + sig
	if s.sessionFromRequest(withCookie(&forged)) != nil {
		t.Fatal("tampered cookie accepted")
	}
	// A state cookie can never be used as a session cookie.
	_, stateCookie := s.sessions.newOAuthState("github", 42)
	forged.Value = stateCookie
	if s.sessionFromRequest(withCookie(&forged)) != nil {
		t.Fatal("state cookie accepted as a session")
	}
}

func TestLogoutClearsSessionCookie(t *testing.T) {
	s := newCloudServer(t, nil, nil, &GitHubOAuth{ClientID: "id"})
	cookie, csrf := authedCookie(t, s, 42)
	rec := requestWithCSRF(t, s, http.MethodPost, "/api/auth/logout", cookie, csrf)
	if rec.Code != http.StatusOK {
		t.Fatalf("logout = %d", rec.Code)
	}
	cleared := false
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatal("logout must clear the session cookie")
	}
}

// TestLoginRedirectsToCanonicalHost: a flow started on another host would set
// its state cookie where the callback can never see it.
func TestLoginRedirectsToCanonicalHost(t *testing.T) {
	s := newCloudServer(t, nil, nil, &GitHubOAuth{ClientID: "id"})
	s.localOnly = false
	s.app.Config.BaseURL = "https://gitsafe.example.com"
	req := httptest.NewRequest(http.MethodGet, "/api/auth/github", nil)
	req.Host = "www.gitsafe.example.com"
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "https://gitsafe.example.com/api/auth/github" {
		t.Fatalf("got %d %q", rec.Code, rec.Header().Get("Location"))
	}
	req.Host = "gitsafe.example.com"
	rec = httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	if !strings.Contains(rec.Header().Get("Location"), "github.com/login/oauth/authorize") {
		t.Fatalf("canonical host should go to GitHub, got %q", rec.Header().Get("Location"))
	}
}

// TestDriveStateBoundToUser: a Drive state minted for one user cannot be
// completed by another user's session.
func TestDriveStateBoundToUser(t *testing.T) {
	st := &fakeStateStore{}
	s, _ := envCloudServer(t, st, true)
	s.connectDrive = func(ctx context.Context, code string) (driveConnectResult, error) {
		t.Fatal("must not exchange the code")
		return driveConnectResult{}, nil
	}
	cookies, stateVal := driveLoginSession(t, s) // minted for user 42
	other, _ := authedCookie(t, s, 7)
	rec := requestCookies(t, s, http.MethodGet, "/api/auth/drive/callback?code=c&state="+stateVal, other, cookies[1])
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}
