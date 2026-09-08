package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/b-isry/gitsafe/internal/config"
	"github.com/b-isry/gitsafe/internal/state"
	"github.com/b-isry/gitsafe/internal/tokenstore"
)

func envCloudServer(t *testing.T, st *fakeStateStore, enabled bool) *Server {
	t.Helper()
	tk := newFakeTokenStore()
	tk.data[tokenstore.GitHubToken] = "tok"
	st.SetGitHubConnection(state.GitHubConnection{Login: "octocat", TokenRef: tokenstore.GitHubToken})

	s := newCloudServer(t, st, tk, &GitHubOAuth{ClientID: "id"})
	if enabled {
		s.app.Config.Cloud = config.CloudConfig{Enabled: true, CredentialsFile: "/creds/keys.json"}
	} else {
		s.app.Config.Cloud = config.CloudConfig{Enabled: false, CredentialsFile: ""}
	}
	return s
}

func seedDriveJob(t *testing.T, st *fakeStateStore) state.ProtectedRepo {
	t.Helper()
	repo := seedProtectedRepo(st, "p1", "acme/alpha", "main")
	st.CreateBackupJob(state.BackupJob{ID: "j1", ProtectedRepoID: "p1", FullName: repo.FullName, State: state.JobEnqueued})
	return repo
}

func stubDriveBundler(s *Server) {
	s.bundleBackup = func(ctx context.Context, fullName, token, output string) (bundleOutcome, error) {
		return bundleOutcome{BundlePath: "/out/acme_alpha.bundle", BundleName: "acme_alpha.bundle", SizeBytes: 99, SHA256: "deadbeef"}, nil
	}
}

// TestRunProtectedBackupUploadSuccess verifies the happy path: Drive enabled, a
// successful upload marks the job completed and upgrades the record to
// "uploaded" with the Drive file ID persisted.
func TestRunProtectedBackupUploadSuccess(t *testing.T) {
	st := &fakeStateStore{}
	repo := seedDriveJob(t, st)
	s := envCloudServer(t, st, true)
	stubDriveBundler(s)
	s.driveUpload = func(ctx context.Context, bundlePath string, cfg config.CloudConfig, logger *slog.Logger) (string, error) {
		if bundlePath != "/out/acme_alpha.bundle" {
			t.Fatalf("upload bundle = %q", bundlePath)
		}
		if !cfg.Enabled || cfg.CredentialsFile == "" {
			t.Fatalf("upload cfg = %+v", cfg)
		}
		return "drive-file-123", nil
	}

	s.runProtectedBackup("j1", repo)

	job, ok := st.BackupJob("j1")
	if !ok || job.State != state.JobCompleted {
		t.Fatalf("job = %+v ok=%v", job, ok)
	}
	records := st.BackupRecordsForRepo("p1")
	if len(records) != 1 {
		t.Fatalf("records = %+v", records)
	}
	rec := records[0]
	if rec.Status != state.BackupStatusUploaded {
		t.Fatalf("record status = %q, want uploaded", rec.Status)
	}
	if rec.DriveFileID != "drive-file-123" {
		t.Fatalf("record driveFileID = %q", rec.DriveFileID)
	}
}

// TestRunProtectedBackupUploadFailure verifies that a failed Drive upload fails
// the job while preserving the local bundle record as local-only ("bundled").
func TestRunProtectedBackupUploadFailure(t *testing.T) {
	st := &fakeStateStore{}
	repo := seedDriveJob(t, st)
	s := envCloudServer(t, st, true)
	stubDriveBundler(s)
	s.driveUpload = func(ctx context.Context, bundlePath string, cfg config.CloudConfig, logger *slog.Logger) (string, error) {
		return "", errBoom
	}

	s.runProtectedBackup("j1", repo)

	job, ok := st.BackupJob("j1")
	if !ok || job.State != state.JobFailed {
		t.Fatalf("job = %+v ok=%v", job, ok)
	}
	if job.Error == "" {
		t.Fatal("expected a drive upload failure message")
	}
	records := st.BackupRecordsForRepo("p1")
	if len(records) != 1 {
		t.Fatalf("expected the local record to be preserved, got %+v", records)
	}
	if records[0].Status != state.BackupStatusBundled {
		t.Fatalf("record status = %q, want bundled (local preserved)", records[0].Status)
	}
	if records[0].DriveFileID != "" {
		t.Fatalf("record driveFileID should be empty on failure, got %q", records[0].DriveFileID)
	}
}

// TestRunProtectedBackupUploadSkipped verifies Drive disabled completes the job
// locally without invoking the uploader.
func TestRunProtectedBackupUploadSkipped(t *testing.T) {
	st := &fakeStateStore{}
	repo := seedDriveJob(t, st)
	s := envCloudServer(t, st, false)
	stubDriveBundler(s)
	called := false
	s.driveUpload = func(ctx context.Context, bundlePath string, cfg config.CloudConfig, logger *slog.Logger) (string, error) {
		called = true
		return "never", nil
	}

	s.runProtectedBackup("j1", repo)

	if called {
		t.Fatal("driveUpload should not be called when Drive is disabled")
	}
	job, ok := st.BackupJob("j1")
	if !ok || job.State != state.JobCompleted {
		t.Fatalf("job = %+v ok=%v", job, ok)
	}
	records := st.BackupRecordsForRepo("p1")
	if len(records) != 1 || records[0].Status != state.BackupStatusBundled {
		t.Fatalf("records = %+v", records)
	}
}

// TestRunProtectedBackupReachesUploading verifies the job lifecycle exposes the
// "uploading" state before completion.
func TestRunProtectedBackupReachesUploading(t *testing.T) {
	st := &fakeStateStore{}
	repo := seedDriveJob(t, st)
	s := envCloudServer(t, st, true)
	stubDriveBundler(s)
	s.driveUpload = func(ctx context.Context, bundlePath string, cfg config.CloudConfig, logger *slog.Logger) (string, error) {
		job, ok := st.BackupJob("j1")
		if !ok || job.State != state.JobUploading {
			t.Fatalf("job during upload = %+v ok=%v", job, ok)
		}
		return "drive-x", nil
	}

	s.runProtectedBackup("j1", repo)

	job, _ := st.BackupJob("j1")
	if job.State != state.JobCompleted {
		t.Fatalf("job = %+v", job)
	}
}

// TestRunProtectedBackupDriveUploadUpdatesPersistException verifies the server
// tolerates a store unable to update the record (logs, still completes).
func TestRunProtectedBackupDriveUploadUpdateRecordError(t *testing.T) {
	st := &fakeStateStore{}
	st.errors = map[string]error{"updateRecord": errBoom}
	repo := seedDriveJob(t, st)
	s := envCloudServer(t, st, true)
	stubDriveBundler(s)
	s.driveUpload = func(ctx context.Context, bundlePath string, cfg config.CloudConfig, logger *slog.Logger) (string, error) {
		return "drive-y", nil
	}

	s.runProtectedBackup("j1", repo)

	job, ok := st.BackupJob("j1")
	if !ok || job.State != state.JobCompleted {
		t.Fatalf("job = %+v ok=%v", job, ok)
	}
}

// TestAPIConnectionsDriveConfigured verifies the connections endpoint reports
// the Drive configured/connected status.
func TestAPIConnectionsDriveConfigured(t *testing.T) {
	s := envCloudServer(t, &fakeStateStore{}, true)
	rec := request(t, s, http.MethodGet, "/api/connections", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if drive := decodeDrive(t, rec); drive["configured"] != true || drive["connected"] != true {
		t.Fatalf("drive = %+v", drive)
	}
}

// TestAPIConnectionsDriveNotConfigured verifies Drive reports unconfigured when
// cloud settings are off.
func TestAPIConnectionsDriveNotConfigured(t *testing.T) {
	s := envCloudServer(t, &fakeStateStore{}, false)
	rec := request(t, s, http.MethodGet, "/api/connections", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if drive := decodeDrive(t, rec); drive["configured"] != false || drive["connected"] != false {
		t.Fatalf("drive = %+v", drive)
	}
}

func decodeDrive(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode connections: %v", err)
	}
	drive, _ := body["drive"].(map[string]any)
	return drive
}
