package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/b-isry/gitsafe/internal/state"
	"github.com/b-isry/gitsafe/internal/tokenstore"
)

// newIsolationServer builds a Server with two fully separate per-user stores
// (users 1 and 2) so isolation tests can prove that a request's data is bound
// strictly to its session's user and that a missing session never falls through
// to any user's store. newCloudServer's canonical user 42 is left seeded with
// throwaway stores; all assertions here use users 1 and 2.
func newIsolationServer(t *testing.T) *Server {
	t.Helper()
	st1 := &fakeStateStore{}
	st1.SetGitHubConnection(state.GitHubConnection{
		GitHubID: 1, Login: "octocat", TokenRef: tokenstore.GitHubToken,
	})
	st1.protected = []state.ProtectedRepo{
		{ID: "u1-p1", GitHubID: 101, FullName: "u1/one", DefaultBranch: "main"},
	}
	tk1 := newFakeTokenStore()
	tk1.data[tokenstore.GitHubToken] = "tok-1"

	st2 := &fakeStateStore{}
	st2.SetGitHubConnection(state.GitHubConnection{
		GitHubID: 2, Login: "octocat2", TokenRef: tokenstore.GitHubToken,
	})
	st2.protected = []state.ProtectedRepo{
		{ID: "u2-p1", GitHubID: 202, FullName: "u2/two", DefaultBranch: "main"},
	}
	tk2 := newFakeTokenStore()
	tk2.data[tokenstore.GitHubToken] = "tok-2"

	s := newCloudServer(t, &fakeStateStore{}, newFakeTokenStore(), &GitHubOAuth{ClientID: "id"})
	s.userStores.stores[1] = &userStores{state: st1, token: tk1}
	s.userStores.stores[2] = &userStores{state: st2, token: tk2}
	return s
}

// TestUnauthenticatedRejectedAcrossAllAPIs pins that every authenticated
// endpoint returns 401 with no session and never leaks another user's data.
func TestUnauthenticatedRejectedAcrossAllAPIs(t *testing.T) {
	s := newIsolationServer(t)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/connections"},
		{http.MethodGet, "/api/repositories"},
		{http.MethodDelete, "/api/connections/github"},
		{http.MethodDelete, "/api/connections/drive"},
		{http.MethodPost, "/api/account/delete"},
		{http.MethodPost, "/api/auth/logout"},
		{http.MethodGet, "/api/protected-repositories"},
		{http.MethodPost, "/api/protected-repositories"},
		{http.MethodDelete, "/api/protected-repositories/u1-p1"},
		{http.MethodPost, "/api/repositories/1/backup"},
		{http.MethodPost, "/api/backups"},
		{http.MethodPost, "/api/protected-repositories/u1-p1/backup"},
		{http.MethodGet, "/api/backup-jobs/j1"},
		{http.MethodGet, "/api/protected-repositories/u1-p1/backup-jobs"},
		{http.MethodGet, "/api/protected-repositories/u1-p1/backups"},
	} {
		rec := request(t, s, tc.method, tc.path, nil)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s status = %d, want 401: %s", tc.method, tc.path, rec.Code, rec.Body.String())
		}
		if body := rec.Body.String(); strings.Contains(body, "octocat") || strings.Contains(body, "u1/one") {
			t.Errorf("%s %s leaked user data to an unauthenticated request: %s", tc.method, tc.path, body)
		}
	}
}

// anonSessionCookie is a cookie whose session was minted by /api/csrf but never
// completed a GitSafe login (userID stays 0). It models a visitor who merely
// fetched the CSRF token, plus any attacker holding a cookie without owning the
// corresponding GitHub authorization.
func anonSessionCookie(t *testing.T, s *Server) *http.Cookie {
	t.Helper()
	rec := request(t, s, http.MethodGet, "/api/csrf", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("csrf status = %d", rec.Code)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName {
			if sess, ok := s.sessions.get(c.Value); ok && sess.userID != 0 {
				t.Fatal("csrf session unexpectedly already authenticated")
			}
			return c
		}
	}
	t.Fatal("no session cookie from /api/csrf")
	return nil
}

// TestPartialSessionRejected pins that having a session is not enough: the
// session must carry an authenticated user, so a userID=0 session is rejected
// just like having no cookie, and never reaches any user's store.
func TestPartialSessionRejected(t *testing.T) {
	s := newIsolationServer(t)
	cookie := anonSessionCookie(t, s)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/connections"},
		{http.MethodGet, "/api/repositories"},
		{http.MethodGet, "/api/protected-repositories"},
		{http.MethodGet, "/api/protected-repositories/u1-p1/backups"},
		{http.MethodPost, "/api/backups"},
	} {
		rec := request(t, s, tc.method, tc.path, cookie)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s status = %d, want 401: %s", tc.method, tc.path, rec.Code, rec.Body.String())
		}
		if body := rec.Body.String(); strings.Contains(body, "octocat") || strings.Contains(body, "u1/one") {
			t.Errorf("%s %s leaked user data to an unauthenticated session: %s", tc.method, tc.path, body)
		}
	}
}

// TestHomePageBoundToSessionUser verifies the page's connection state follows
// the session's user: signed-out visitors get the public onboarding view, and
// each signed-in user sees only their own account.
func TestHomePageBoundToSessionUser(t *testing.T) {
	s := newIsolationServer(t)

	anon := request(t, s, http.MethodGet, "/", nil)
	if anon.Code != http.StatusOK {
		t.Fatalf("anon / status = %d, want 200", anon.Code)
	}
	if !strings.Contains(anon.Body.String(), "Connect with GitHub") {
		t.Error("signed-out page must offer connecting")
	}
	if strings.Contains(anon.Body.String(), "@octocat") || strings.Contains(anon.Body.String(), `id="cloudLoading"`) {
		t.Error("signed-out page leaked a connected account")
	}

	cookie1, _ := authedCookie(t, s, 1)
	one := request(t, s, http.MethodGet, "/", cookie1)
	if !strings.Contains(one.Body.String(), "@octocat</span>") {
		t.Error("user 1 page missing @octocat")
	}
	if strings.Contains(one.Body.String(), "@octocat2</span>") {
		t.Error("user 1 page leaked user 2's account")
	}

	cookie2, _ := authedCookie(t, s, 2)
	two := request(t, s, http.MethodGet, "/", cookie2)
	if !strings.Contains(two.Body.String(), "@octocat2</span>") {
		t.Error("user 2 page missing @octocat2")
	}
	if strings.Contains(two.Body.String(), "@octocat</span>") {
		t.Error("user 2 page leaked user 1's account")
	}
}

// TestConnectionsAndProtectedReposIsolated verifies API data is scoped to the
// session's user: each user sees exactly their own connections and protected
// repositories, never another user's.
func TestConnectionsAndProtectedReposIsolated(t *testing.T) {
	s := newIsolationServer(t)

	loginFor := func(cookie *http.Cookie) string {
		t.Helper()
		rec := request(t, s, http.MethodGet, "/api/connections", cookie)
		if rec.Code != http.StatusOK {
			t.Fatalf("connections status = %d, want 200", rec.Code)
		}
		var body struct {
			Github struct {
				Login string `json:"login"`
			} `json:"github"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body.Github.Login
	}
	repoIDsFor := func(cookie *http.Cookie) []string {
		t.Helper()
		rec := request(t, s, http.MethodGet, "/api/protected-repositories", cookie)
		if rec.Code != http.StatusOK {
			t.Fatalf("protected-repositories status = %d, want 200", rec.Code)
		}
		var body struct {
			Repositories []struct {
				ID       string `json:"id"`
				FullName string `json:"fullName"`
			} `json:"repositories"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		ids := make([]string, 0, len(body.Repositories))
		for _, r := range body.Repositories {
			ids = append(ids, r.ID+":"+r.FullName)
		}
		return ids
	}

	cookie1, _ := authedCookie(t, s, 1)
	cookie2, _ := authedCookie(t, s, 2)

	if got := loginFor(cookie1); got != "octocat" {
		t.Errorf("user 1 login = %q", got)
	}
	if got := loginFor(cookie2); got != "octocat2" {
		t.Errorf("user 2 login = %q", got)
	}

	if got := repoIDsFor(cookie1); len(got) != 1 || got[0] != "u1-p1:u1/one" {
		t.Errorf("user 1 repos = %v", got)
	}
	if got := repoIDsFor(cookie2); len(got) != 1 || got[0] != "u2-p1:u2/two" {
		t.Errorf("user 2 repos = %v", got)
	}
}
