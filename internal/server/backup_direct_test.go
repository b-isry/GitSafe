package server

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/b-isry/gitsafe/internal/providers"
	"github.com/b-isry/gitsafe/internal/state"
	"github.com/b-isry/gitsafe/internal/tokenstore"
)

// postBackup posts to a backup-starting endpoint with the given body (may be
// nil) and session/CSRF credentials.
func postBackup(t *testing.T, s *Server, cookie *http.Cookie, csrf, path string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	var rd *bytes.Reader
	if body == nil {
		rd = bytes.NewReader(nil)
	} else {
		rd = bytes.NewReader(body)
	}
	req := httptest.NewRequest(http.MethodPost, path, rd)
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

func stubDiscovery(s *Server, repos ...providers.Repository) {
	s.githubLister = func(ctx context.Context, token string) ([]providers.Repository, error) {
		return repos, nil
	}
}

func stubQuickBackend(s *Server) {
	s.bundleBackup = func(ctx context.Context, fullName, token, output string) (bundleOutcome, error) {
		return bundleOutcome{BundlePath: "/out/x.bundle", BundleName: "x.bundle", SizeBytes: 5, SHA256: "abc"}, nil
	}
	s.driveUpload = func(ctx context.Context, bundlePath, folderID, refreshToken string, onProgress func(int64, int64), logger *slog.Logger) (string, error) {
		return "drive-file-1", nil
	}
}

// --- latest successful backup semantics ---

// TestLatestBackupForOnlySuccessful verifies that only successful ("uploaded")
// backup records establish a latest backup: a newer failed attempt is ignored.
func TestLatestBackupForOnlySuccessful(t *testing.T) {
	old := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	records := []state.BackupRecord{
		{ID: "failed-newer", ProtectedRepoID: "p1", FullName: "a/b", CreatedAt: old.Add(5 * time.Hour), BundleSize: 1, Status: "failed"},
		{ID: "uploaded-older", ProtectedRepoID: "p1", FullName: "a/b", CreatedAt: old, BundleSize: 9000, Status: "uploaded", DriveFileID: "FILE-OK"},
	}
	got := latestBackupFor(records)
	if got == nil {
		t.Fatal("expected a view from the successful record")
	}
	if got.Status != "uploaded" || got.BundleSizeBytes != 9000 {
		t.Fatalf("unexpected view: %+v", got)
	}
	if want := old.Format(time.RFC3339); got.CreatedAt != want {
		t.Fatalf("createdAt = %q, want %q", got.CreatedAt, want)
	}
	if got.DriveViewLink != "https://drive.google.com/file/d/FILE-OK/view" {
		t.Fatalf("driveViewLink = %q", got.DriveViewLink)
	}
}

// --- /api/repositories backup status ---

// TestAPIRepositoriesBackupStatus verifies the dashboard list derives backup
// status from actual successful backups, not from protection/config state.
func TestAPIRepositoriesBackupStatus(t *testing.T) {
	st := &fakeStateStore{}
	st.SetGitHubConnection(state.GitHubConnection{Login: "octocat", TokenRef: tokenstore.GitHubTokenFor(42)})
	tk := newFakeTokenStore()
	tk.data[tokenstore.GitHubTokenFor(42)] = "tok"
	// Repo 11: previously "protected" record exists, but it NEVER backed up.
	// Repo 22: has a successful backup. Repo 33: has only failed attempts.
	// Repo 44: no internal record at all.
	st.protected = []state.ProtectedRepo{
		{ID: "p11", GitHubID: 11, FullName: "prot/only"},
		{ID: "p22", GitHubID: 22, FullName: "ok/backed"},
		{ID: "p33", GitHubID: 33, FullName: "bad/attempts"},
	}
	st.records = []state.BackupRecord{
		{ID: "r22", ProtectedRepoID: "p22", FullName: "ok/backed", CreatedAt: time.Now().Add(-2 * time.Hour), BundleSize: 4096, Status: "uploaded", DriveFileID: "DRIVE22"},
		{ID: "r33a", ProtectedRepoID: "p33", FullName: "bad/attempts", CreatedAt: time.Now().Add(-1 * time.Hour), BundleSize: 1, Status: "failed"},
		{ID: "r33b", ProtectedRepoID: "p33", FullName: "bad/attempts", CreatedAt: time.Now(), BundleSize: 2, Status: "failed"},
	}
	s := newCloudServer(t, st, tk, &GitHubOAuth{ClientID: "id"})
	stubDiscovery(s,
		providers.Repository{ID: 11, FullName: "prot/only", DefaultBranch: "main"},
		providers.Repository{ID: 22, FullName: "ok/backed", DefaultBranch: "main"},
		providers.Repository{ID: 33, FullName: "bad/attempts", DefaultBranch: "main"},
		providers.Repository{ID: 44, FullName: "fresh/new", DefaultBranch: "main"},
	)

	cookie, _ := authedCookie(t, s, 42)
	rec := request(t, s, http.MethodGet, "/api/repositories", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Repositories []map[string]any `json:"repositories"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	byID := map[int64]map[string]any{}
	for _, r := range body.Repositories {
		byID[int64(r["githubId"].(float64))] = r
	}
	if _, has := byID[11]["protected"]; has {
		t.Error("dashboard list must not expose a 'protected' field")
	}
	if byID[11]["backedUp"] != false {
		t.Errorf("protected-but-never-backed-up repo: backedUp = %v, want false", byID[11]["backedUp"])
	}
	if byID[22]["backedUp"] != true {
		t.Errorf("successfully-backed-up repo: backedUp = %v, want true", byID[22]["backedUp"])
	}
	if byID[33]["backedUp"] != false {
		t.Errorf("failed-only repo: backedUp = %v, want false", byID[33]["backedUp"])
	}
	if byID[44]["backedUp"] != false {
		t.Errorf("fresh repo: backedUp = %v, want false", byID[44]["backedUp"])
	}
	if byID[22]["latestBackup"] == nil {
		t.Fatalf("repo 22 latestBackup: %v", byID[22]["latestBackup"])
	}
	lb := byID[22]["latestBackup"].(map[string]any)
	if lb["driveViewLink"] != "https://drive.google.com/file/d/DRIVE22/view" {
		t.Fatalf("repo 22 driveViewLink = %v", lb["driveViewLink"])
	}
	for _, id := range []int64{11, 33, 44} {
		if byID[id]["latestBackup"] != nil {
			t.Errorf("repo %d latestBackup = %v, want nil", id, byID[id]["latestBackup"])
		}
	}
	// The stale/staleness fields must survive the rework.
	if byID[44]["stale"] != true {
		t.Errorf("fresh repo stale = %v, want true", byID[44]["stale"])
	}
}

// --- POST /api/repositories/{githubId}/backup (direct backup) ---

func TestBackupRepoAcceptedWithoutProtection(t *testing.T) {
	st := &fakeStateStore{}
	s, _ := envCloudServer(t, st, true)
	stubDiscovery(s, providers.Repository{ID: 100, FullName: "acme/alpha", DefaultBranch: "main"})
	stubQuickBackend(s)

	cookie, csrf := authedCookie(t, s, 42)
	rec := postBackup(t, s, cookie, csrf, "/api/repositories/100/backup", nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.ID == "" {
		t.Fatal("expected a job id")
	}
	// The internal record must have been materialized automatically.
	if len(st.protected) != 1 || st.protected[0].GitHubID != 100 {
		t.Fatalf("internal record not auto-created: %+v", st.protected)
	}
	// No protection/backup-all API call was involved; the job started directly.
	jobs := st.BackupJobs()
	if len(jobs) == 0 || jobs[0].FullName != "acme/alpha" {
		t.Fatalf("expected a backup job for acme/alpha, got %+v", jobs)
	}
}

func TestBackupRepoNotFoundOnGitHub(t *testing.T) {
	s, _ := envCloudServer(t, &fakeStateStore{}, true)
	stubDiscovery(s, providers.Repository{ID: 100, FullName: "acme/alpha"})
	cookie, csrf := authedCookie(t, s, 42)
	rec := postBackup(t, s, cookie, csrf, "/api/repositories/999/backup", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestBackupRepoBadID(t *testing.T) {
	s, _ := envCloudServer(t, &fakeStateStore{}, true)
	cookie, csrf := authedCookie(t, s, 42)
	rec := postBackup(t, s, cookie, csrf, "/api/repositories/not-a-number/backup", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestBackupRepoNoSession(t *testing.T) {
	s, _ := envCloudServer(t, &fakeStateStore{}, true)
	rec := postBackup(t, s, nil, "", "/api/repositories/100/backup", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestBackupRepoMissingCSRF(t *testing.T) {
	s, _ := envCloudServer(t, &fakeStateStore{}, true)
	cookie, _ := authedCookie(t, s, 42)
	rec := postBackup(t, s, cookie, "", "/api/repositories/100/backup", nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestBackupRepoNotConfigured(t *testing.T) {
	s := newCloudServer(t, &fakeStateStore{}, newFakeTokenStore(), nil)
	cookie, _ := authedCookie(t, s, 42)
	rec := postBackup(t, s, cookie, "", "/api/repositories/100/backup", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestBackupRepoInFlight(t *testing.T) {
	st := &fakeStateStore{}
	st.protected = []state.ProtectedRepo{{ID: "p1", GitHubID: 100, FullName: "acme/alpha"}}
	st.jobs = []state.BackupJob{{ID: "j1", ProtectedRepoID: "p1", FullName: "acme/alpha", State: state.JobCloning}}
	s, _ := envCloudServer(t, st, true)
	stubDiscovery(s, providers.Repository{ID: 100, FullName: "acme/alpha", DefaultBranch: "main"})
	cookie, csrf := authedCookie(t, s, 42)
	rec := postBackup(t, s, cookie, csrf, "/api/repositories/100/backup", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
}

func TestBackupRepoDriveNotConnected(t *testing.T) {
	st := &fakeStateStore{}
	s := newCloudServer(t, st, newFakeTokenStore(), &GitHubOAuth{ClientID: "id"})
	stubDiscovery(s, providers.Repository{ID: 100, FullName: "acme/alpha", DefaultBranch: "main"})
	cookie, csrf := authedCookie(t, s, 42)
	rec := postBackup(t, s, cookie, csrf, "/api/repositories/100/backup", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (Drive required)", rec.Code)
	}
}

// --- POST /api/backups (back up all) ---

func TestBackupAllStartsJobsForEveryRepo(t *testing.T) {
	st := &fakeStateStore{}
	s, _ := envCloudServer(t, st, true)
	stubDiscovery(s,
		providers.Repository{ID: 1, FullName: "acme/one", DefaultBranch: "main"},
		providers.Repository{ID: 2, FullName: "acme/two", DefaultBranch: "main"},
	)
	stubQuickBackend(s)

	cookie, csrf := authedCookie(t, s, 42)
	rec := postBackup(t, s, cookie, csrf, "/api/backups", nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Jobs []jobView `json:"jobs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Jobs) != 2 {
		t.Fatalf("jobs = %+v, want 2 (no protection required)", body.Jobs)
	}
	// Both repositories were materialized internally and started directly.
	if len(st.protected) != 2 {
		t.Fatalf("internal records = %+v", st.protected)
	}
	if len(st.BackupJobs()) != 2 {
		t.Fatalf("backup jobs = %+v", st.BackupJobs())
	}
}

func TestBackupAllSkipsInFlightRepo(t *testing.T) {
	st := &fakeStateStore{}
	st.protected = []state.ProtectedRepo{{ID: "p1", GitHubID: 1, FullName: "acme/one"}}
	st.jobs = []state.BackupJob{{ID: "j1", ProtectedRepoID: "p1", FullName: "acme/one", State: state.JobCloning}}
	s, _ := envCloudServer(t, st, true)
	stubDiscovery(s,
		providers.Repository{ID: 1, FullName: "acme/one", DefaultBranch: "main"},
		providers.Repository{ID: 2, FullName: "acme/two", DefaultBranch: "main"},
	)
	stubQuickBackend(s)

	cookie, csrf := authedCookie(t, s, 42)
	rec := postBackup(t, s, cookie, csrf, "/api/backups", nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	var body struct {
		Jobs []jobView `json:"jobs"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if len(body.Jobs) != 1 || body.Jobs[0].FullName != "acme/two" {
		t.Fatalf("jobs = %+v, want only the not-in-flight repo", body.Jobs)
	}
}

func TestBackupAllNoSession(t *testing.T) {
	s, _ := envCloudServer(t, &fakeStateStore{}, true)
	rec := postBackup(t, s, nil, "", "/api/backups", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestBackupAllMissingCSRF(t *testing.T) {
	s, _ := envCloudServer(t, &fakeStateStore{}, true)
	cookie, _ := authedCookie(t, s, 42)
	rec := postBackup(t, s, cookie, "", "/api/backups", nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestBackupAllNotConfigured(t *testing.T) {
	s := newCloudServer(t, &fakeStateStore{}, newFakeTokenStore(), nil)
	cookie, _ := authedCookie(t, s, 42)
	rec := postBackup(t, s, cookie, "", "/api/backups", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestBackupAllDriveNotConnected(t *testing.T) {
	st := &fakeStateStore{}
	s := newCloudServer(t, st, newFakeTokenStore(), &GitHubOAuth{ClientID: "id"})
	stubDiscovery(s, providers.Repository{ID: 1, FullName: "acme/one", DefaultBranch: "main"})
	cookie, csrf := authedCookie(t, s, 42)
	rec := postBackup(t, s, cookie, csrf, "/api/backups", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (Drive required)", rec.Code)
	}
}
