package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/b-isry/gitsafe/internal/state"
	"github.com/b-isry/gitsafe/internal/tokenstore"
)

// --- handler tests ---

func postProtectedBackup(t *testing.T, s *Server, cookie *http.Cookie, csrf, id string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/protected-repositories/"+id+"/backup", nil)
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

func seedProtectedRepo(st *fakeStateStore, id, fullName, branch string) state.ProtectedRepo {
	repo := state.ProtectedRepo{ID: id, GitHubID: 100, FullName: fullName, DefaultBranch: branch}
	st.protected = append(st.protected, repo)
	return repo
}

func TestBackupProtectedRepoNotConfigured(t *testing.T) {
	s := newCloudServer(t, &fakeStateStore{}, newFakeTokenStore(), nil)
	rec := postProtectedBackup(t, s, nil, "", "p1")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestBackupProtectedRepoNoSession(t *testing.T) {
	s := newCloudServer(t, &fakeStateStore{}, newFakeTokenStore(), &GitHubOAuth{ClientID: "id"})
	rec := postProtectedBackup(t, s, nil, "", "p1")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestBackupProtectedRepoMissingCSRF(t *testing.T) {
	s := newCloudServer(t, &fakeStateStore{}, newFakeTokenStore(), &GitHubOAuth{ClientID: "id"})
	cookie, _ := csrfCookie(t, s)
	rec := postProtectedBackup(t, s, cookie, "", "p1")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestBackupProtectedRepoUnknown(t *testing.T) {
	st := &fakeStateStore{}
	s := newCloudServer(t, st, newFakeTokenStore(), &GitHubOAuth{ClientID: "id"})
	cookie, csrf := csrfCookie(t, s)
	rec := postProtectedBackup(t, s, cookie, csrf, "nope")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestBackupProtectedRepoAccepted(t *testing.T) {
	st := &fakeStateStore{}
	seedProtectedRepo(st, "p1", "acme/alpha", "main")
	tk := newFakeTokenStore()
	tk.data[tokenstore.GitHubToken] = "tok"
	st.SetGitHubConnection(state.GitHubConnection{Login: "octocat", TokenRef: tokenstore.GitHubToken})

	s := newCloudServer(t, st, tk, &GitHubOAuth{ClientID: "id"})
	s.bundleBackup = func(ctx context.Context, fullName, token, output string) (bundleOutcome, error) {
		return bundleOutcome{BundlePath: "x.bundle", BundleName: "x.bundle", SizeBytes: 5, SHA256: "abc"}, nil
	}
	cookie, csrf := csrfCookie(t, s)
	rec := postProtectedBackup(t, s, cookie, csrf, "p1")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	var body struct {
		ID              string `json:"id"`
		ProtectedRepoID string `json:"protectedRepoId"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.ID == "" || body.ProtectedRepoID != "p1" {
		t.Fatalf("job response = %+v", body)
	}
}

func TestBackupProtectedRepoDuplicate(t *testing.T) {
	st := &fakeStateStore{}
	seedProtectedRepo(st, "p1", "acme/alpha", "main")
	st.jobs = []state.BackupJob{{ID: "j1", ProtectedRepoID: "p1", FullName: "acme/alpha", State: state.JobCloning}}
	s := newCloudServer(t, st, newFakeTokenStore(), &GitHubOAuth{ClientID: "id"})
	cookie, csrf := csrfCookie(t, s)
	rec := postProtectedBackup(t, s, cookie, csrf, "p1")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
}

func TestBackupJobNotConfigured(t *testing.T) {
	s := newCloudServer(t, &fakeStateStore{}, newFakeTokenStore(), nil)
	rec := request(t, s, http.MethodGet, "/api/backup-jobs/j1", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestBackupJobUnknown(t *testing.T) {
	s := newCloudServer(t, &fakeStateStore{}, newFakeTokenStore(), &GitHubOAuth{ClientID: "id"})
	rec := request(t, s, http.MethodGet, "/api/backup-jobs/nope", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestProtectedBackupJobsList(t *testing.T) {
	st := &fakeStateStore{}
	seedProtectedRepo(st, "p1", "acme/alpha", "main")
	st.jobs = []state.BackupJob{{ID: "j1", ProtectedRepoID: "p1", State: state.JobCompleted}}
	s := newCloudServer(t, st, newFakeTokenStore(), &GitHubOAuth{ClientID: "id"})
	rec := request(t, s, http.MethodGet, "/api/protected-repositories/p1/backup-jobs", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body struct {
		Jobs []jobView `json:"jobs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Jobs) != 1 || body.Jobs[0].ID != "j1" {
		t.Fatalf("jobs = %+v", body.Jobs)
	}
}

func TestProtectedBackupHistory(t *testing.T) {
	st := &fakeStateStore{}
	seedProtectedRepo(st, "p1", "acme/alpha", "main")
	st.records = []state.BackupRecord{{ID: "r1", ProtectedRepoID: "p1", FullName: "acme/alpha", BundleName: "a.bundle", BundleSize: 12, Status: "bundled"}}
	s := newCloudServer(t, st, newFakeTokenStore(), &GitHubOAuth{ClientID: "id"})
	rec := request(t, s, http.MethodGet, "/api/protected-repositories/p1/backups", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body struct {
		Backups []backupView `json:"backups"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Backups) != 1 || body.Backups[0].BundleName != "a.bundle" || body.Backups[0].Status != "bundled" {
		t.Fatalf("backups = %+v", body.Backups)
	}
}

func TestProtectedBackupHistoryUnknownRepo(t *testing.T) {
	s := newCloudServer(t, &fakeStateStore{}, newFakeTokenStore(), &GitHubOAuth{ClientID: "id"})
	rec := request(t, s, http.MethodGet, "/api/protected-repositories/nope/backups", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// --- orchestration tests (run synchronously, no goroutine) ---

func TestRunProtectedBackupSuccess(t *testing.T) {
	st := &fakeStateStore{}
	repo := seedProtectedRepo(st, "p1", "acme/alpha", "main")
	st.CreateBackupJob(state.BackupJob{ID: "j1", ProtectedRepoID: "p1", FullName: repo.FullName, State: state.JobEnqueued})
	tk := newFakeTokenStore()
	tk.data[tokenstore.GitHubToken] = "tok"
	st.SetGitHubConnection(state.GitHubConnection{Login: "octocat", TokenRef: tokenstore.GitHubToken})

	s := newCloudServer(t, st, tk, &GitHubOAuth{ClientID: "id"})
	s.bundleBackup = func(ctx context.Context, fullName, token, output string) (bundleOutcome, error) {
		if token != "tok" {
			t.Fatalf("bundler token = %q", token)
		}
		if fullName != "acme/alpha" {
			t.Fatalf("bundler fullName = %q", fullName)
		}
		return bundleOutcome{BundlePath: "/out/acme_alpha.bundle", BundleName: "acme_alpha.bundle", SizeBytes: 99, SHA256: "deadbeef"}, nil
	}

	s.runProtectedBackup("j1", repo)

	done := false
	for _, j := range st.BackupJobs() {
		if j.ID == "j1" && j.State == state.JobCompleted {
			done = true
		}
	}
	if !done {
		t.Fatalf("job did not complete: %+v", st.BackupJobs())
	}
	records := st.BackupRecordsForRepo("p1")
	if len(records) != 1 {
		t.Fatalf("records = %+v", records)
	}
	rec := records[0]
	if rec.BundleName != "acme_alpha.bundle" || rec.BundleSize != 99 || rec.BundleSHA256 != "deadbeef" {
		t.Fatalf("record = %+v", rec)
	}
	if rec.Status != "bundled" || rec.DefaultBranch != "main" {
		t.Fatalf("record status/branch = %q/%q", rec.Status, rec.DefaultBranch)
	}
}

func TestRunProtectedBackupBundlerFailure(t *testing.T) {
	st := &fakeStateStore{}
	repo := seedProtectedRepo(st, "p1", "acme/alpha", "main")
	st.CreateBackupJob(state.BackupJob{ID: "j1", ProtectedRepoID: "p1", FullName: repo.FullName, State: state.JobEnqueued})
	tk := newFakeTokenStore()
	tk.data[tokenstore.GitHubToken] = "tok"
	st.SetGitHubConnection(state.GitHubConnection{Login: "octocat", TokenRef: tokenstore.GitHubToken})

	s := newCloudServer(t, st, tk, &GitHubOAuth{ClientID: "id"})
	s.bundleBackup = func(ctx context.Context, fullName, token, output string) (bundleOutcome, error) {
		return bundleOutcome{}, errBoom
	}
	s.runProtectedBackup("j1", repo)

	job, ok := st.BackupJob("j1")
	if !ok || job.State != state.JobFailed {
		t.Fatalf("job = %+v ok=%v", job, ok)
	}
	if job.Error == "" {
		t.Fatal("expected a failure message")
	}
	if len(st.BackupRecords()) != 0 {
		t.Fatalf("no record should be written on failure, got %+v", st.BackupRecords())
	}
}

func TestRunProtectedBackupDisconnected(t *testing.T) {
	st := &fakeStateStore{}
	repo := seedProtectedRepo(st, "p1", "acme/alpha", "main")
	st.CreateBackupJob(state.BackupJob{ID: "j1", ProtectedRepoID: "p1", FullName: repo.FullName, State: state.JobEnqueued})

	s := newCloudServer(t, st, newFakeTokenStore(), &GitHubOAuth{ClientID: "id"})
	s.runProtectedBackup("j1", repo)

	job, ok := st.BackupJob("j1")
	if !ok || job.State != state.JobFailed {
		t.Fatalf("job = %+v ok=%v", job, ok)
	}
	if len(st.BackupRecords()) != 0 {
		t.Fatalf("no record should be written when disconnected")
	}
}

// errBoom is a sentinel used by bundler-failure tests.
var errBoom = &boomError{}

type boomError struct{}

func (e *boomError) Error() string { return "boom" }
