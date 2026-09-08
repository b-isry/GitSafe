package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/b-isry/gitsafe/internal/providers"
	"github.com/b-isry/gitsafe/internal/state"
)

// --- pure validation helper ---

func TestResolveProtectCandidatesAdds(t *testing.T) {
	discovered := []providers.Repository{
		{ID: 1, FullName: "a/one", DefaultBranch: "main"},
		{ID: 2, FullName: "b/two", DefaultBranch: "master"},
	}
	toAdd, already, errs := resolveProtectCandidates(discovered, []int64{1, 2}, nil)
	if len(toAdd) != 2 || len(already) != 0 || len(errs) != 0 {
		t.Fatalf("toAdd=%v already=%v errs=%v", toAdd, already, errs)
	}
	if toAdd[0].GitHubID != 1 || toAdd[0].DefaultBranch != "main" {
		t.Fatalf("toAdd[0] = %+v", toAdd[0])
	}
}

func TestResolveProtectCandidatesAlreadyProtected(t *testing.T) {
	discovered := []providers.Repository{{ID: 7, FullName: "x/y", DefaultBranch: "main"}}
	alreadyProtected := map[int64]bool{7: true}
	toAdd, already, errs := resolveProtectCandidates(discovered, []int64{7}, alreadyProtected)
	if len(toAdd) != 0 || len(errs) != 0 {
		t.Fatalf("toAdd=%v errs=%v", toAdd, errs)
	}
	if len(already) != 1 || already[0] != 7 {
		t.Fatalf("already = %v", already)
	}
}

func TestResolveProtectCandidatesMissingRepo(t *testing.T) {
	discovered := []providers.Repository{{ID: 1, FullName: "a/b"}}
	toAdd, already, errs := resolveProtectCandidates(discovered, []int64{99}, nil)
	if len(toAdd) != 0 || len(already) != 0 {
		t.Fatalf("toAdd=%v already=%v", toAdd, already)
	}
	if errs[99] == "" {
		t.Fatalf("expected error for missing repo 99, got %v", errs)
	}
}

func TestResolveProtectCandidatesDedupesRequest(t *testing.T) {
	discovered := []providers.Repository{{ID: 1, FullName: "a/b"}}
	toAdd, _, _ := resolveProtectCandidates(discovered, []int64{1, 1, 1}, nil)
	if len(toAdd) != 1 {
		t.Fatalf("duplicate ids in request should collapse to one record, got %d", len(toAdd))
	}
}

// --- handler orchestration (no live GitHub) ---

func postProtectRequest(t *testing.T, s *Server, cookie *http.Cookie, csrf string, ids []int64) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"repoIds": ids})
	req := httptest.NewRequest(http.MethodPost, "/api/protected-repositories", bytes.NewReader(body))
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

func TestProtectRepositoriesNotConfigured(t *testing.T) {
	s := newCloudServer(t, &fakeStateStore{}, newFakeTokenStore(), nil)
	rec := postProtectRequest(t, s, nil, "", []int64{1})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestProtectRepositoriesNoSession(t *testing.T) {
	s := newCloudServer(t, &fakeStateStore{}, newFakeTokenStore(), &GitHubOAuth{ClientID: "id"})
	rec := postProtectRequest(t, s, nil, "", []int64{1})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestProtectRepositoriesMissingCSRF(t *testing.T) {
	s := newCloudServer(t, &fakeStateStore{}, newFakeTokenStore(), &GitHubOAuth{ClientID: "id"})
	cookie, csrf := csrfCookie(t, s)
	rec := postProtectRequest(t, s, cookie, "", []int64{1})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if csrf == "" {
		t.Fatal("expected a csrf token")
	}
}

func TestProtectRepositoriesMalformedBody(t *testing.T) {
	s := newCloudServer(t, &fakeStateStore{}, newFakeTokenStore(), &GitHubOAuth{ClientID: "id"})
	cookie, csrf := csrfCookie(t, s)
	req := httptest.NewRequest(http.MethodPost, "/api/protected-repositories", bytes.NewReader([]byte(`{not json`)))
	req.RemoteAddr = "127.0.0.1:55555"
	req.AddCookie(cookie)
	req.Header.Set("X-CSRF-Token", csrf)
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestProtectRepositoriesEmpty(t *testing.T) {
	s := newCloudServer(t, &fakeStateStore{}, newFakeTokenStore(), &GitHubOAuth{ClientID: "id"})
	cookie, csrf := csrfCookie(t, s)
	rec := postProtectRequest(t, s, cookie, csrf, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// csrfCookie establishes a session and returns the cookie + CSRF token.
func csrfCookie(t *testing.T, s *Server) (*http.Cookie, string) {
	t.Helper()
	rec := request(t, s, http.MethodGet, "/api/csrf", nil)
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName {
			cookie = c
		}
	}
	return cookie, body["csrfToken"]
}

func TestListProtectedRepositories(t *testing.T) {
	st := &fakeStateStore{}
	st.protected = []state.ProtectedRepo{
		{ID: "p1", GitHubID: 1, FullName: "a/b", DefaultBranch: "main"},
	}
	s := newCloudServer(t, st, newFakeTokenStore(), &GitHubOAuth{ClientID: "id"})
	rec := request(t, s, http.MethodGet, "/api/protected-repositories", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body struct {
		Repositories []protectedRepoView `json:"repositories"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Repositories) != 1 || body.Repositories[0].GitHubID != 1 || body.Repositories[0].FullName != "a/b" {
		t.Fatalf("repositories = %+v", body.Repositories)
	}
}

func TestListProtectedRepositoriesNotConfigured(t *testing.T) {
	s := newCloudServer(t, &fakeStateStore{}, newFakeTokenStore(), nil)
	rec := request(t, s, http.MethodGet, "/api/protected-repositories", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func deleteProtected(t *testing.T, s *Server, cookie *http.Cookie, csrf, id string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, "/api/protected-repositories/"+id, nil)
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

func TestRemoveProtectedRepositorySuccess(t *testing.T) {
	st := &fakeStateStore{}
	st.protected = []state.ProtectedRepo{{ID: "p1", GitHubID: 1, FullName: "a/b"}}
	s := newCloudServer(t, st, newFakeTokenStore(), &GitHubOAuth{ClientID: "id"})
	cookie, csrf := csrfCookie(t, s)
	rec := deleteProtected(t, s, cookie, csrf, "p1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if len(st.protected) != 0 {
		t.Fatalf("expected protection removed, got %+v", st.protected)
	}
	if !st.saved {
		t.Fatal("expected state saved after removal")
	}
}

func TestRemoveProtectedRepositoryUnknown(t *testing.T) {
	st := &fakeStateStore{}
	st.protected = []state.ProtectedRepo{{ID: "p1", GitHubID: 1}}
	s := newCloudServer(t, st, newFakeTokenStore(), &GitHubOAuth{ClientID: "id"})
	cookie, csrf := csrfCookie(t, s)
	rec := deleteProtected(t, s, cookie, csrf, "nope")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestRemoveProtectedRepositoryNoCSRF(t *testing.T) {
	st := &fakeStateStore{}
	st.protected = []state.ProtectedRepo{{ID: "p1", GitHubID: 1}}
	s := newCloudServer(t, st, newFakeTokenStore(), &GitHubOAuth{ClientID: "id"})
	cookie, _ := csrfCookie(t, s)
	rec := deleteProtected(t, s, cookie, "", "p1")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if len(st.protected) != 1 {
		t.Fatalf("protection must not be removed without CSRF")
	}
}

func TestRemoveProtectedRepositoryNotConfigured(t *testing.T) {
	s := newCloudServer(t, &fakeStateStore{}, newFakeTokenStore(), nil)
	cookie, csrf := csrfCookie(t, s)
	rec := deleteProtected(t, s, cookie, csrf, "p1")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}
