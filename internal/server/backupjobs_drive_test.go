package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/b-isry/gitsafe/internal/state"
	"github.com/b-isry/gitsafe/internal/tokenstore"
)

// envCloudServer builds a server with the Phase 1 cloud wiring attached. When
// enabled, it also configures the Drive OAuth application and seeds a connected
// Drive account (refresh token in the keychain, connection in state).
func envCloudServer(t *testing.T, st *fakeStateStore, enabled bool) (*Server, *fakeTokenStore) {
	t.Helper()
	tk := newFakeTokenStore()
	tk.data[tokenstore.GitHubTokenFor(42)] = "tok"
	st.SetGitHubConnection(state.GitHubConnection{Login: "octocat", TokenRef: tokenstore.GitHubTokenFor(42)})

	s := newCloudServer(t, st, tk, &GitHubOAuth{ClientID: "id"})
	// Stub repo size check to return a small size (10 MB) for testing
	s.repoSize = func(ctx context.Context, fullName string) (int, error) {
		return 10, nil
	}
	if enabled {
		s.ConfigureDriveOAuth(&DriveOAuth{
			ClientID:     "drive-id",
			ClientSecret: "drive-secret",
			RedirectURL:  "http://127.0.0.1:8080/api/auth/drive/callback",
		})
		tk.data[tokenstore.DriveTokenFor(42)] = "drv-refresh"
		st.SetDriveConnection(state.DriveConnection{
			AccountEmail:    "octo@example.com",
			ConnectedAt:     time.Now(),
			StorageFolderID: "folder-1",
			TokenRef:        tokenstore.DriveTokenFor(42),
		})
	}
	return s, tk
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

// TestRunProtectedBackupUploadSuccess verifies the happy path: a connected Drive
// account, a successful upload, the temporary local bundle removed, and exactly
// one "uploaded" record carrying the Drive file ID.
func TestRunProtectedBackupUploadSuccess(t *testing.T) {
	st := &fakeStateStore{}
	repo := seedDriveJob(t, st)
	s, tk := envCloudServer(t, st, true)
	stubDriveBundler(s)
	s.driveUpload = func(ctx context.Context, bundlePath, folderID, refreshToken string, onProgress func(int64, int64), logger *slog.Logger) (string, error) {
		if bundlePath != "/out/acme_alpha.bundle" {
			t.Fatalf("upload bundle = %q", bundlePath)
		}
		if folderID != "folder-1" {
			t.Fatalf("upload folderID = %q, want folder-1", folderID)
		}
		if refreshToken != "drv-refresh" {
			t.Fatalf("upload refreshToken = %q, want drv-refresh", refreshToken)
		}
		return "drive-file-123", nil
	}

	userID := int64(42)
	s.runProtectedBackup(userID, st, tk, "j1", repo)

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
// the job with a clear message and creates NO backup record (a failed backup is
// not an uploaded one).
func TestRunProtectedBackupUploadFailure(t *testing.T) {
	st := &fakeStateStore{}
	repo := seedDriveJob(t, st)
	s, tk := envCloudServer(t, st, true)
	stubDriveBundler(s)
	s.driveUpload = func(ctx context.Context, bundlePath, folderID, refreshToken string, onProgress func(int64, int64), logger *slog.Logger) (string, error) {
		return "", errBoom
	}

	userID := int64(42)
	s.runProtectedBackup(userID, st, tk, "j1", repo)

	job, ok := st.BackupJob("j1")
	if !ok || job.State != state.JobFailed {
		t.Fatalf("job = %+v ok=%v", job, ok)
	}
	if job.Error == "" {
		t.Fatal("expected a drive upload failure message")
	}
	records := st.BackupRecordsForRepo("p1")
	if len(records) != 0 {
		t.Fatalf("expected no record after a failed upload, got %+v", records)
	}
}

// TestRunProtectedBackupDriveNotConnected verifies that without a connected
// Drive account the job fails fast with a clear message and neither the uploader
// nor the bundler is invoked and no record is created.
func TestRunProtectedBackupDriveNotConnected(t *testing.T) {
	st := &fakeStateStore{}
	repo := seedDriveJob(t, st)
	s, tk := envCloudServer(t, st, false)
	uploaded := false
	stubDriveBundler(s)
	s.driveUpload = func(ctx context.Context, bundlePath, folderID, refreshToken string, onProgress func(int64, int64), logger *slog.Logger) (string, error) {
		uploaded = true
		return "never", nil
	}

	userID := int64(42)
	s.runProtectedBackup(userID, st, tk, "j1", repo)

	if uploaded {
		t.Fatal("driveUpload should not be called when Drive is not connected")
	}
	job, ok := st.BackupJob("j1")
	if !ok || job.State != state.JobFailed {
		t.Fatalf("job = %+v ok=%v", job, ok)
	}
	if job.Error == "" {
		t.Fatal("expected a Drive-not-connected failure message")
	}
	if records := st.BackupRecordsForRepo("p1"); len(records) != 0 {
		t.Fatalf("expected no record when Drive is not connected, got %+v", records)
	}
}

// TestRunProtectedBackupReachesUploading verifies the job lifecycle exposes the
// "uploading" state before completion.
func TestRunProtectedBackupReachesUploading(t *testing.T) {
	st := &fakeStateStore{}
	repo := seedDriveJob(t, st)
	s, tk := envCloudServer(t, st, true)
	stubDriveBundler(s)
	s.driveUpload = func(ctx context.Context, bundlePath, folderID, refreshToken string, onProgress func(int64, int64), logger *slog.Logger) (string, error) {
		job, ok := st.BackupJob("j1")
		if !ok || job.State != state.JobUploading {
			t.Fatalf("job during upload = %+v ok=%v", job, ok)
		}
		return "drive-x", nil
	}

	userID := int64(42)
	s.runProtectedBackup(userID, st, tk, "j1", repo)

	job, _ := st.BackupJob("j1")
	if job.State != state.JobCompleted {
		t.Fatalf("job = %+v", job)
	}
}

// TestRunProtectedBackupNoRecordBeforeUploadSuccess verifies the backup record
// is only written after a successful Drive upload, never before.
func TestRunProtectedBackupNoRecordBeforeUploadSuccess(t *testing.T) {
	st := &fakeStateStore{}
	repo := seedDriveJob(t, st)
	s, tk := envCloudServer(t, st, true)
	stubDriveBundler(s)
	s.driveUpload = func(ctx context.Context, bundlePath, folderID, refreshToken string, onProgress func(int64, int64), logger *slog.Logger) (string, error) {
		if records := st.BackupRecordsForRepo("p1"); len(records) != 0 {
			t.Fatalf("record created before upload success: %+v", records)
		}
		return "drive-z", nil
	}

	userID := int64(42)
	s.runProtectedBackup(userID, st, tk, "j1", repo)

	job, ok := st.BackupJob("j1")
	if !ok || job.State != state.JobCompleted {
		t.Fatalf("job = %+v ok=%v", job, ok)
	}
	if records := st.BackupRecordsForRepo("p1"); len(records) != 1 {
		t.Fatalf("records = %+v", records)
	}
}

// TestAPIConnectionsDriveConfigured verifies the connections endpoint reports a
// connected Drive account.
func TestAPIConnectionsDriveConfigured(t *testing.T) {
	s, _ := envCloudServer(t, &fakeStateStore{}, true)
	cookie, _ := authedCookie(t, s, 42)
	rec := request(t, s, http.MethodGet, "/api/connections", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if drive := decodeDrive(t, rec); drive["configured"] != true || drive["connected"] != true {
		t.Fatalf("drive = %+v", drive)
	}
}

// TestAPIConnectionsDriveNotConfigured verifies Drive reports unconfigured when
// the deployment has no Drive OAuth application registered.
func TestAPIConnectionsDriveNotConfigured(t *testing.T) {
	s, _ := envCloudServer(t, &fakeStateStore{}, false)
	cookie, _ := authedCookie(t, s, 42)
	rec := request(t, s, http.MethodGet, "/api/connections", cookie)
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
