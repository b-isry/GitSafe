package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/b-isry/gitsafe/internal/state"
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
	cookie, _ := authedCookie(t, s, 42)
	rec := postProtectedBackup(t, s, cookie, "", "p1")
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
	cookie, _ := authedCookie(t, s, 42)
	rec := postProtectedBackup(t, s, cookie, "", "p1")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestBackupProtectedRepoUnknown(t *testing.T) {
	st := &fakeStateStore{}
	s := newCloudServer(t, st, newFakeTokenStore(), &GitHubOAuth{ClientID: "id"})
	cookie, csrf := authedCookie(t, s, 42)
	rec := postProtectedBackup(t, s, cookie, csrf, "nope")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestBackupProtectedRepoAccepted(t *testing.T) {
	st := &fakeStateStore{}
	seedProtectedRepo(st, "p1", "acme/alpha", "main")
	s, _ := envCloudServer(t, st, true)
	s.bundleBackup = func(ctx context.Context, fullName, token, output string) (bundleOutcome, error) {
		return bundleOutcome{BundlePath: "x.bundle", BundleName: "x.bundle", SizeBytes: 5, SHA256: "abc"}, nil
	}
	cookie, csrf := authedCookie(t, s, 42)
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
	cookie, csrf := authedCookie(t, s, 42)
	rec := postProtectedBackup(t, s, cookie, csrf, "p1")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
}

func TestBackupJobNotConfigured(t *testing.T) {
	s := newCloudServer(t, &fakeStateStore{}, newFakeTokenStore(), nil)
	cookie, _ := authedCookie(t, s, 42)
	rec := request(t, s, http.MethodGet, "/api/backup-jobs/j1", cookie)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestBackupJobUnknown(t *testing.T) {
	s := newCloudServer(t, &fakeStateStore{}, newFakeTokenStore(), &GitHubOAuth{ClientID: "id"})
	cookie, _ := authedCookie(t, s, 42)
	rec := request(t, s, http.MethodGet, "/api/backup-jobs/nope", cookie)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestProtectedBackupJobsList(t *testing.T) {
	st := &fakeStateStore{}
	seedProtectedRepo(st, "p1", "acme/alpha", "main")
	st.jobs = []state.BackupJob{{ID: "j1", ProtectedRepoID: "p1", State: state.JobCompleted}}
	s := newCloudServer(t, st, newFakeTokenStore(), &GitHubOAuth{ClientID: "id"})
	cookie, _ := authedCookie(t, s, 42)
	rec := request(t, s, http.MethodGet, "/api/protected-repositories/p1/backup-jobs", cookie)
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
	st.records = []state.BackupRecord{{ID: "r1", ProtectedRepoID: "p1", FullName: "acme/alpha", BundleName: "a.bundle", BundleSize: 12, Status: "uploaded"}}
	s := newCloudServer(t, st, newFakeTokenStore(), &GitHubOAuth{ClientID: "id"})
	cookie, _ := authedCookie(t, s, 42)
	rec := request(t, s, http.MethodGet, "/api/protected-repositories/p1/backups", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body struct {
		Backups []backupView `json:"backups"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Backups) != 1 || body.Backups[0].BundleName != "a.bundle" || body.Backups[0].Status != "uploaded" {
		t.Fatalf("backups = %+v", body.Backups)
	}
}

func TestProtectedBackupHistoryUnknownRepo(t *testing.T) {
	s := newCloudServer(t, &fakeStateStore{}, newFakeTokenStore(), &GitHubOAuth{ClientID: "id"})
	cookie, _ := authedCookie(t, s, 42)
	rec := request(t, s, http.MethodGet, "/api/protected-repositories/nope/backups", cookie)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// --- orchestration tests (run synchronously, no goroutine) ---

func TestRunProtectedBackupSuccess(t *testing.T) {
	st := &fakeStateStore{}
	repo := seedProtectedRepo(st, "p1", "acme/alpha", "main")
	st.CreateBackupJob(state.BackupJob{ID: "j1", ProtectedRepoID: "p1", FullName: repo.FullName, State: state.JobEnqueued})

	s, tk := envCloudServer(t, st, true)
	s.bundleBackup = func(ctx context.Context, fullName, token, output string) (bundleOutcome, error) {
		if token != "tok" {
			t.Fatalf("bundler token = %q", token)
		}
		if fullName != "acme/alpha" {
			t.Fatalf("bundler fullName = %q", fullName)
		}
		return bundleOutcome{BundlePath: "/out/acme_alpha.bundle", BundleName: "acme_alpha.bundle", SizeBytes: 99, SHA256: "deadbeef"}, nil
	}
	s.driveUpload = func(ctx context.Context, bundlePath, folderID, refreshToken string, onProgress func(int64, int64), logger *slog.Logger) (string, error) {
		if folderID != "folder-1" || refreshToken != "drv-refresh" {
			t.Fatalf("upload folderID/refreshToken = %q/%q", folderID, refreshToken)
		}
		return "drive-file-123", nil
	}

	userID := int64(42)
	s.runProtectedBackup(userID, st, tk, "j1", repo)

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
	if rec.Status != state.BackupStatusUploaded || rec.DefaultBranch != "main" {
		t.Fatalf("record status/branch = %q/%q", rec.Status, rec.DefaultBranch)
	}
	if rec.DriveFileID != "drive-file-123" {
		t.Fatalf("record driveFileID = %q", rec.DriveFileID)
	}
}

func TestRunProtectedBackupBundlerFailure(t *testing.T) {
	st := &fakeStateStore{}
	repo := seedProtectedRepo(st, "p1", "acme/alpha", "main")
	st.CreateBackupJob(state.BackupJob{ID: "j1", ProtectedRepoID: "p1", FullName: repo.FullName, State: state.JobEnqueued})

	s, tk := envCloudServer(t, st, true)
	s.bundleBackup = func(ctx context.Context, fullName, token, output string) (bundleOutcome, error) {
		return bundleOutcome{}, errBoom
	}
	userID := int64(42)
	s.runProtectedBackup(userID, st, tk, "j1", repo)

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
	userID := int64(42)
	stores, _ := s.userStores.getOrCreate(userID)
	tk := stores.token.(*fakeTokenStore)
	s.runProtectedBackup(userID, st, tk, "j1", repo)

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

// TestJobViewIncludesUploadProgress verifies the live upload byte progress is
// surfaced through the job view and cleared once the upload ends.
func TestJobViewIncludesUploadProgress(t *testing.T) {
	s := newCloudServer(t, &fakeStateStore{}, newFakeTokenStore(), &GitHubOAuth{ClientID: "id"})
	job := state.BackupJob{ID: "j1", State: state.JobUploading}

	s.setJobProgress("j1", jobProgressMetrics{UploadedBytes: 25, TotalBytes: 100})
	v := s.toJobView(job)
	if v.UploadedBytes != 25 || v.TotalBytes != 100 || v.Progress != 25 {
		t.Fatalf("view progress = %+v, want 25/100/25", v)
	}

	// A terminal job (progress cleared) must not report stale bytes.
	s.clearJobProgress("j1")
	if v := s.toJobView(job); v.UploadedBytes != 0 || v.TotalBytes != 0 || v.Progress != 0 {
		t.Fatalf("view after clear = %+v, want no progress", v)
	}
}

// TestJobViewProgressClampsPercent verifies the progress percentage is clamped
// to the 0..100 range even if an uploader over-reports.
func TestJobViewProgressClampsPercent(t *testing.T) {
	s := newCloudServer(t, &fakeStateStore{}, newFakeTokenStore(), &GitHubOAuth{ClientID: "id"})
	job := state.BackupJob{ID: "j1", State: state.JobUploading}

	s.setJobProgress("j1", jobProgressMetrics{UploadedBytes: 200, TotalBytes: 100})
	if v := s.toJobView(job); v.Progress != 100 {
		t.Fatalf("progress = %d, want clamped 100", v.Progress)
	}
	s.setJobProgress("j1", jobProgressMetrics{UploadedBytes: 0, TotalBytes: 100})
	if v := s.toJobView(job); v.Progress != 0 {
		t.Fatalf("progress = %d, want 0", v.Progress)
	}
}
