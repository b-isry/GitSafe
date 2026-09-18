package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/b-isry/gitsafe/internal/providers"
	"github.com/b-isry/gitsafe/internal/state"
	"github.com/b-isry/gitsafe/internal/tokenstore"
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
	if body.Repositories[0].LatestBackup != nil {
		t.Fatalf("expected no latest backup, got %+v", body.Repositories[0].LatestBackup)
	}
}

func TestLatestBackupForNone(t *testing.T) {
	if got := latestBackupFor(nil); got != nil {
		t.Fatalf("expected nil for no records, got %+v", got)
	}
}

func TestLatestBackupForPicksNewest(t *testing.T) {
	base := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	records := []state.BackupRecord{
		{ID: "old", ProtectedRepoID: "p1", FullName: "a/b", CreatedAt: base, BundleSize: 100, Status: "failed"},
		{ID: "new", ProtectedRepoID: "p1", FullName: "a/b", CreatedAt: base.Add(2 * time.Hour), BundleSize: 5000, Status: "uploaded", DriveFileID: "FILE123"},
		{ID: "mid", ProtectedRepoID: "p1", FullName: "a/b", CreatedAt: base.Add(1 * time.Hour), BundleSize: 200, Status: "uploaded"},
	}
	got := latestBackupFor(records)
	if got == nil {
		t.Fatal("expected a view, got nil")
	}
	if got.Status != "uploaded" || got.BundleSizeBytes != 5000 {
		t.Fatalf("unexpected view: %+v", got)
	}
	if got.DriveViewLink != "https://drive.google.com/file/d/FILE123/view" {
		t.Fatalf("driveViewLink = %q", got.DriveViewLink)
	}
	if want := base.Add(2 * time.Hour).Format(time.RFC3339); got.CreatedAt != want {
		t.Fatalf("createdAt = %q, want %q", got.CreatedAt, want)
	}
}

func TestLatestBackupForNoDriveRecord(t *testing.T) {
	base := time.Now()
	got := latestBackupFor([]state.BackupRecord{
		{ID: "r", ProtectedRepoID: "p1", CreatedAt: base, BundleSize: 512, Status: "uploaded"},
	})
	if got == nil {
		t.Fatal("expected a view, got nil")
	}
	if got.DriveViewLink != "" {
		t.Fatalf("expected no drive link for a record without a drive file id, got %q", got.DriveViewLink)
	}
}

func TestListProtectedRepositoriesIncludesLatestBackup(t *testing.T) {
	st := &fakeStateStore{}
	st.protected = []state.ProtectedRepo{
		{ID: "p1", GitHubID: 1, FullName: "a/b", DefaultBranch: "main"},
	}
	st.records = []state.BackupRecord{
		{ID: "r1", ProtectedRepoID: "p1", FullName: "a/b", CreatedAt: time.Now(), BundleSize: 2048, Status: "uploaded", DriveFileID: "DRIVE-1"},
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
	if len(body.Repositories) != 1 {
		t.Fatalf("repositories = %+v", body.Repositories)
	}
	lb := body.Repositories[0].LatestBackup
	if lb == nil {
		t.Fatal("expected a latest backup on the protected-repo view")
	}
	if lb.Status != "uploaded" || lb.BundleSizeBytes != 2048 {
		t.Fatalf("latestBackup = %+v", lb)
	}
	if lb.DriveViewLink != "https://drive.google.com/file/d/DRIVE-1/view" {
		t.Fatalf("driveViewLink = %q", lb.DriveViewLink)
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

func TestPushStaleness(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	threshold := 30

	// A recently-pushed repository is not stale and reports its day count.
	recent := now.Add(-3 * 24 * time.Hour)
	days, stale := pushStaleness(recent, now, threshold)
	if stale || days == nil || *days != 3 {
		t.Fatalf("recent push: stale=%v days=%v (want stale=false days=3)", stale, days)
	}

	// A push older than the threshold is stale.
	old := now.Add(-74 * 24 * time.Hour)
	days, stale = pushStaleness(old, now, threshold)
	if !stale || days == nil || *days != 74 {
		t.Fatalf("old push: stale=%v days=%v (want stale=true days=74)", stale, days)
	}

	// A zero PushedAt means never pushed: stale with a nil (null) day count.
	days, stale = pushStaleness(time.Time{}, now, threshold)
	if !stale || days != nil {
		t.Fatalf("zero PushedAt: stale=%v days=%v (want stale=true days=nil)", stale, days)
	}
}

func TestAPIRepositoriesIncludesStaleness(t *testing.T) {
	st := &fakeStateStore{}
	st.SetGitHubConnection(state.GitHubConnection{Login: "octocat", TokenRef: tokenstore.GitHubToken})
	tk := newFakeTokenStore()
	tk.data[tokenstore.GitHubToken] = "tok"
	s := newCloudServer(t, st, tk, &GitHubOAuth{ClientID: "id"})
	s.app.Config.Days = 30

	now := time.Now()
	s.githubLister = func(ctx context.Context, token string) ([]providers.Repository, error) {
		if token != "tok" {
			t.Fatalf("githubLister token = %q, want the stored token", token)
		}
		return []providers.Repository{
			{ID: 1, FullName: "a/recent", PushedAt: now.Add(-3 * 24 * time.Hour)},
			{ID: 2, FullName: "b/old", PushedAt: now.Add(-74 * 24 * time.Hour)},
			{ID: 3, FullName: "c/never"},
		}, nil
	}

	rec := request(t, s, http.MethodGet, "/api/repositories", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Repositories []map[string]any `json:"repositories"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	byName := map[string]map[string]any{}
	for _, r := range body.Repositories {
		byName[r["fullName"].(string)] = r
	}

	recent := byName["a/recent"]
	if recent["stale"] != false {
		t.Fatalf("recent stale = %v, want false", recent["stale"])
	}
	if d, ok := recent["daysSinceLastPush"].(float64); !ok || d != 3 {
		t.Fatalf("recent daysSinceLastPush = %v, want 3", recent["daysSinceLastPush"])
	}

	old := byName["b/old"]
	if old["stale"] != true {
		t.Fatalf("old stale = %v, want true", old["stale"])
	}
	if d, ok := old["daysSinceLastPush"].(float64); !ok || d != 74 {
		t.Fatalf("old daysSinceLastPush = %v, want 74", old["daysSinceLastPush"])
	}

	never := byName["c/never"]
	if never["stale"] != true {
		t.Fatalf("never stale = %v, want true", never["stale"])
	}
	if v, present := never["daysSinceLastPush"]; !present || v != nil {
		t.Fatalf("never daysSinceLastPush = %v, want null", never["daysSinceLastPush"])
	}
}
