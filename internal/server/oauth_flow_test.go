package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/b-isry/gitsafe/internal/githuboauth"
	"github.com/b-isry/gitsafe/internal/providers"
	"github.com/b-isry/gitsafe/internal/state"
	"github.com/b-isry/gitsafe/internal/tokenstore"
)

// devLeaks are developer/operator configuration strings the end-user UI and
// API must never surface.
var devLeaks = []string{
	"GITSAFE_GITHUB_CLIENT_ID",
	"GITSAFE_GITHUB_CLIENT_SECRET",
	"client id",
	"client secret",
	"environment variable",
}

func assertNoDevLeak(t *testing.T, label, body string) {
	t.Helper()
	lower := strings.ToLower(body)
	for _, s := range devLeaks {
		if strings.Contains(lower, strings.ToLower(s)) {
			t.Errorf("%s must not expose %q:\n%s", label, s, body)
		}
	}
}

// TestCloudRepositoriesPageAlwaysOffersConnect verifies the disconnected page
// presents the end-user "Connect GitHub" flow whether or not the deployment has
// configured a GitHub application, and never shows developer setup text.
func TestCloudRepositoriesPageAlwaysOffersConnect(t *testing.T) {
	for _, tc := range []struct {
		name  string
		oauth *GitHubOAuth
	}{
		{"unconfigured", nil},
		{"configured", &GitHubOAuth{ClientID: "id"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newCloudServer(t, &fakeStateStore{}, newFakeTokenStore(), tc.oauth)
			rec := request(t, s, http.MethodGet, "/", nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			body := rec.Body.String()
			for _, want := range []string{
				"Connect with GitHub",
				"hero-card__cta",
				"Back up your repositories",
				"Connect your GitHub account to start backing up your cloud repositories to Google Drive.",
				"Requires read-only access to repository contents",
			} {
				if !strings.Contains(body, want) {
					t.Errorf("disconnected page missing %q", want)
				}
			}
			assertNoDevLeak(t, "disconnected page", body)
			if strings.Contains(body, "Disconnect") {
				t.Errorf("disconnected page must not show a Disconnect action")
			}
		})
	}
}

// TestCloudRepositoriesPageConnected shows the account header and a Disconnect
// action, and still avoids developer configuration text.
func TestCloudRepositoriesPageConnected(t *testing.T) {
	st := &fakeStateStore{}
	st.SetGitHubConnection(state.GitHubConnection{
		GitHubID: 42, Login: "octocat", Name: "Oc ToCat", TokenRef: tokenstore.GitHubToken,
	})
	tk := newFakeTokenStore()
	tk.data[tokenstore.GitHubToken] = "tok"
	s := newCloudServer(t, st, tk, &GitHubOAuth{ClientID: "id"})

	for _, path := range []string{"/", "/api/connections"} {
		rec := request(t, s, http.MethodGet, path, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200", path, rec.Code)
		}
		// Only the HTML page carries the account header and Disconnect button.
		if path == "/" {
			body := rec.Body.String()
			for _, want := range []string{"@octocat", "Disconnect"} {
				if !strings.Contains(body, want) {
					t.Errorf("connected page missing %q", want)
				}
			}
			// The dashboard UI must be present: the loading indicator, the
			// unified repository table with its filters and bulk action, plus
			// the failure/reconnect surfaces the client toggles.
			for _, hook := range []string{
				`id="cloudLoading"`,
				`id="cloudError"`,
				`id="cloudRetryBtn"`,
				`id="reconnectPanel"`,
				`id="repoTable"`,
				`id="repoFilters"`,
				`id="backupAllBtn"`,
			} {
				if !strings.Contains(body, hook) {
					t.Errorf("connected page missing %s", hook)
				}
			}
			if strings.Contains(body, "Connect GitHub") {
				t.Errorf("connected page must not offer connecting again")
			}
			assertNoDevLeak(t, "connected page", body)
		} else {
			assertNoDevLeak(t, "connections api", rec.Body.String())
		}
	}
}

// TestGitHubLoginUnavailableIsGraceful verifies that an unconfigured deployment
// yields a friendly page, not raw JSON or developer instructions.
func TestGitHubLoginUnavailableIsGraceful(t *testing.T) {
	s := newCloudServer(t, &fakeStateStore{}, newFakeTokenStore(), nil)
	rec := request(t, s, http.MethodGet, "/api/auth/github", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(strings.ToLower(body), "sign-in is not available") {
		t.Errorf("unconfigured login must be a graceful page, got:\n%s", body)
	}
	assertNoDevLeak(t, "unconfigured login", body)
}

// TestGitHubCallbackUnavailableIsGraceful covers the same for the callback.
func TestGitHubCallbackUnavailableIsGraceful(t *testing.T) {
	s := newCloudServer(t, &fakeStateStore{}, newFakeTokenStore(), nil)
	rec := request(t, s, http.MethodGet, "/api/auth/github/callback?state=x&code=y", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	assertNoDevLeak(t, "unconfigured callback", rec.Body.String())
}

// TestGitHubLoginRedirectsOnlyToGitHub verifies the OAuth redirect goes to
// GitHub's authorize endpoint only (open-redirect prevention) with a state and
// never leaks the client secret or a client_secret parameter.
func TestGitHubLoginRedirectsOnlyToGitHub(t *testing.T) {
	s := newCloudServer(t, &fakeStateStore{}, newFakeTokenStore(), &GitHubOAuth{
		ClientID: "gh-client", ClientSecret: "supersecret",
		RedirectURL: oauthRedirectURL,
	})
	rec := request(t, s, http.MethodGet, "/api/auth/github", nil)
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	loc := rec.Header().Get("Location")
	u := mustParse(t, loc)
	if u.Host != "github.com" || u.Path != "/login/oauth/authorize" {
		t.Fatalf("redirect not to GitHub authorize: %q", loc)
	}
	q := u.Query()
	if q.Get("client_id") != "gh-client" {
		t.Errorf("client_id = %q", q.Get("client_id"))
	}
	if q.Get("scope") != providers.ScopeRepo {
		t.Errorf("scope = %q, want repo", q.Get("scope"))
	}
	if q.Get("state") == "" {
		t.Errorf("missing OAuth state in authorize URL")
	}
	if q.Get("client_secret") != "" || strings.Contains(loc, "supersecret") {
		t.Errorf("client secret leaked into authorize URL: %q", loc)
	}
}

// TestGitHubLoginStateRandomness ensures each login attempt mints a fresh,
// unpredictable state value.
func TestGitHubLoginStateRandomness(t *testing.T) {
	s := newCloudServer(t, &fakeStateStore{}, newFakeTokenStore(), &GitHubOAuth{ClientID: "id"})
	_, first, _ := loginAndGetState(t, s)
	_, second, _ := loginAndGetState(t, s)
	if len(first) < 64 || len(second) < 64 {
		t.Fatalf("states too short: %d, %d", len(first), len(second))
	}
	if first == second {
		t.Fatalf("state values must be unique per login")
	}
}

// TestGitHubCallbackDenied maps a denied authorization to a friendly message.
func TestGitHubCallbackDenied(t *testing.T) {
	s := newCloudServer(t, &fakeStateStore{}, newFakeTokenStore(), &GitHubOAuth{ClientID: "id"})
	s.connectGitHub = func(ctx context.Context, code string) (string, []string, providers.Identity, error) {
		return "", nil, providers.Identity{}, githuboauth.ErrDenied
	}
	cookie, stateVal, _ := loginAndGetState(t, s)
	rec := request(t, s, http.MethodGet,
		"/api/auth/github/callback?state="+stateVal+"&code=c", cookie)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "GitHub authorization was cancelled.") {
		t.Errorf("denied callback must say cancelled, got:\n%s", body)
	}
	assertNoDevLeak(t, "denied callback", body)
}

// TestGitHubCallbackDeclinedQueryError covers GitHub returning no code with an
// error=access_denied query parameter (user clicked "cancel" on GitHub).
func TestGitHubCallbackDeclinedQueryError(t *testing.T) {
	s := newCloudServer(t, &fakeStateStore{}, newFakeTokenStore(), &GitHubOAuth{ClientID: "id"})
	cookie, stateVal, _ := loginAndGetState(t, s)
	rec := request(t, s, http.MethodGet,
		"/api/auth/github/callback?state="+stateVal+"&error=access_denied", cookie)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "GitHub authorization was cancelled.") {
		t.Errorf("expected cancelled message, got:\n%s", body)
	}
}

// TestGitHubCallbackExchangeFailureFriendly ensures raw OAuth/upstream errors
// are never surfaced to the user.
func TestGitHubCallbackExchangeFailureFriendly(t *testing.T) {
	s := newCloudServer(t, &fakeStateStore{}, newFakeTokenStore(), &GitHubOAuth{ClientID: "id"})
	s.connectGitHub = func(ctx context.Context, code string) (string, []string, providers.Identity, error) {
		return "", nil, providers.Identity{}, errors.New("oauth2: server response missing access_token")
	}
	cookie, stateVal, _ := loginAndGetState(t, s)
	rec := request(t, s, http.MethodGet,
		"/api/auth/github/callback?state="+stateVal+"&code=c", cookie)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Could not finish connecting to GitHub.") {
		t.Errorf("expected friendly retry message, got:\n%s", body)
	}
	if strings.Contains(body, "oauth2") || strings.Contains(body, "access_token") {
		t.Errorf("raw OAuth error leaked to user:\n%s", body)
	}
}

// TestGitHubCallbackRedirectsInternally verifies the post-connect redirect is a
// site-internal path (open-redirect prevention).
func TestGitHubCallbackRedirectsInternally(t *testing.T) {
	st := &fakeStateStore{}
	tk := newFakeTokenStore()
	s := newCloudServer(t, st, tk, &GitHubOAuth{ClientID: "id"})
	s.connectGitHub = func(ctx context.Context, code string) (string, []string, providers.Identity, error) {
		return "tok", []string{providers.ScopeRepo},
			providers.Identity{ID: 1, Login: "u", Scopes: []string{providers.ScopeRepo}}, nil
	}
	cookie, stateVal, _ := loginAndGetState(t, s)
	rec := request(t, s, http.MethodGet,
		"/api/auth/github/callback?state="+stateVal+"&code=c", cookie)
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if loc != "/" {
		t.Fatalf("callback redirect must be internal, got %q", loc)
	}
}

// TestSecretNeverInAPIResponses pins that credentials are absent from every
// API the browser can reach.
func TestSecretNeverInAPIResponses(t *testing.T) {
	st := &fakeStateStore{}
	st.SetGitHubConnection(state.GitHubConnection{Login: "octocat", TokenRef: tokenstore.GitHubToken})
	tk := newFakeTokenStore()
	tk.data[tokenstore.GitHubToken] = "tok"
	s := newCloudServer(t, st, tk, &GitHubOAuth{ClientID: "gh-client", ClientSecret: "supersecret"})
	s.githubLister = func(ctx context.Context, token string) ([]providers.Repository, error) {
		return []providers.Repository{{ID: 1, FullName: "a/b"}}, nil
	}

	for _, path := range []string{"/api/connections", "/api/repositories", "/api/protected-repositories"} {
		rec := request(t, s, http.MethodGet, path, nil)
		body := rec.Body.String()
		assertNoDevLeak(t, path, body)
		if strings.Contains(body, "supersecret") {
			t.Errorf("%s must not contain the client secret", path)
		}
	}
}

// disconnectWithCSRF establishes a session (fetching /api/csrf) and issues a
// DELETE to /api/connections/github carrying the CSRF token.
func disconnectWithCSRF(t *testing.T, s *Server) *httptest.ResponseRecorder {
	t.Helper()
	csrf := request(t, s, http.MethodGet, "/api/csrf", nil)
	var csrfBody map[string]string
	_ = json.Unmarshal(csrf.Body.Bytes(), &csrfBody)
	var cookie *http.Cookie
	for _, c := range csrf.Result().Cookies() {
		if c.Name == sessionCookieName {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("no session cookie from /api/csrf")
	}
	req := httptest.NewRequest(http.MethodDelete, "/api/connections/github", nil)
	req.RemoteAddr = "127.0.0.1:55555"
	req.AddCookie(cookie)
	req.Header.Set("X-CSRF-Token", csrfBody["csrfToken"])
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	return rec
}

// TestGitHubDisconnectRevokesToken verifies the disconnect deletes the local
// credential AND best-effort revokes the token server-side with the token value
// (never as a secret in a response).
func TestGitHubDisconnectRevokesToken(t *testing.T) {
	st := &fakeStateStore{}
	st.SetGitHubConnection(state.GitHubConnection{Login: "octocat", TokenRef: tokenstore.GitHubToken})
	tk := newFakeTokenStore()
	tk.data[tokenstore.GitHubToken] = "tok"
	s := newCloudServer(t, st, tk, &GitHubOAuth{ClientID: "gh-client", ClientSecret: "gh-secret"})

	var revoked string
	s.revokeToken = func(ctx context.Context, token string) error {
		revoked = token
		return nil
	}

	rec := disconnectWithCSRF(t, s)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if revoked != "tok" {
		t.Fatalf("revocation token = %q, want the stored token value", revoked)
	}
	if st.hasGh {
		t.Fatal("expected connection cleared")
	}
	if _, ok := tk.data[tokenstore.GitHubToken]; ok {
		t.Fatal("expected local token removed")
	}
	assertNoDevLeak(t, "disconnect response", rec.Body.String())
}

// TestGitHubDisconnectRevokeFailureStillDisconnects verifies a revocation
// failure never blocks removing the local credential (the security boundary).
func TestGitHubDisconnectRevokeFailureStillDisconnects(t *testing.T) {
	st := &fakeStateStore{}
	st.SetGitHubConnection(state.GitHubConnection{Login: "octocat", TokenRef: tokenstore.GitHubToken})
	tk := newFakeTokenStore()
	tk.data[tokenstore.GitHubToken] = "tok"
	s := newCloudServer(t, st, tk, &GitHubOAuth{ClientID: "id", ClientSecret: "sec"})
	s.revokeToken = func(ctx context.Context, token string) error {
		return errors.New("upstream boom")
	}

	rec := disconnectWithCSRF(t, s)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 even when revocation fails", rec.Code)
	}
	if st.hasGh {
		t.Fatal("expected connection cleared despite revocation failure")
	}
	if _, ok := tk.data[tokenstore.GitHubToken]; ok {
		t.Fatal("expected local token removed despite revocation failure")
	}
}

// TestGitHubDisconnectWithoutRevoker verifies a deployment without a client
// secret still disconnects cleanly (revocation is conditional on having the
// deployment credentials to revoke with).
func TestGitHubDisconnectWithoutRevoker(t *testing.T) {
	st := &fakeStateStore{}
	st.SetGitHubConnection(state.GitHubConnection{Login: "octocat", TokenRef: tokenstore.GitHubToken})
	tk := newFakeTokenStore()
	tk.data[tokenstore.GitHubToken] = "tok"
	s := newCloudServer(t, st, tk, &GitHubOAuth{ClientID: "id"}) // no secret → no revoker
	if s.revokeToken != nil {
		t.Fatal("revokeToken should not be wired without a client secret")
	}

	rec := disconnectWithCSRF(t, s)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if st.hasGh {
		t.Fatal("expected connection cleared")
	}
	if _, ok := tk.data[tokenstore.GitHubToken]; ok {
		t.Fatal("expected local token removed")
	}
}

// TestGitHubOAuthFromEnvClientID verifies the client id can come from the
// environment (deployment-level) rather than only the config file.
func TestGitHubOAuthFromEnvClientID(t *testing.T) {
	t.Setenv(githubClientIDEnv, "")
	t.Setenv(githubClientSecretEnv, "")
	if gh, ok := GitHubOAuthFromEnv("", oauthRedirectURL); ok || gh != nil {
		t.Fatalf("no credentials must yield unconfigured, got %+v", gh)
	}

	t.Setenv(githubClientIDEnv, "env-client")
	t.Setenv(githubClientSecretEnv, "env-secret")
	if gh, ok := GitHubOAuthFromEnv("", oauthRedirectURL); !ok || gh.ClientID != "env-client" {
		t.Fatalf("client id from env not used: %+v", gh)
	}

	t.Setenv(githubClientIDEnv, "")
	t.Setenv(githubClientSecretEnv, "env-secret")
	gh, ok := GitHubOAuthFromEnv("cfg-client", oauthRedirectURL)
	if !ok || gh.ClientID != "cfg-client" {
		t.Fatalf("config client id should be the fallback: %+v", gh)
	}
	if gh.ClientSecret != "env-secret" {
		t.Fatalf("client secret not read from env: %+v", gh)
	}
}
