package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/b-isry/gitsafe/internal/providers"
	"github.com/b-isry/gitsafe/internal/state"
)

func writeBundleInStaging(t *testing.T, name, contents string) bundleBackupFunc {
	t.Helper()
	return func(ctx context.Context, fullName, token, output string) (bundleOutcome, error) {
		path := filepath.Join(output, name)
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			return bundleOutcome{}, err
		}
		return bundleOutcome{
			BundlePath: path,
			BundleName: name,
			SizeBytes:  int64(len(contents)),
			SHA256:     "abc",
		}, nil
	}
}

func waitForJob(t *testing.T, st *fakeStateStore, id string) state.BackupJob {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		job, ok := st.BackupJob(id)
		if ok && (job.State == state.JobCompleted || job.State == state.JobFailed) {
			return job
		}
		time.Sleep(10 * time.Millisecond)
	}
	job, _ := st.BackupJob(id)
	t.Fatalf("job %s did not finish: %+v", id, job)
	return state.BackupJob{}
}

func TestRunLocalBackupWithoutDrive(t *testing.T) {
	st := &fakeStateStore{}
	repo := seedDriveJob(t, st)
	s, tk := envCloudServer(t, st, false)
	dir := t.TempDir()
	s.localBackupDir = dir
	var staging string
	s.driveUpload = func(ctx context.Context, bundlePath, folderID, existingFileID, refreshToken string, onProgress func(int64, int64), logger *slog.Logger) (string, error) {
		t.Error("drive upload must not run for a local backup")
		return "", nil
	}
	s.bundleBackup = func(ctx context.Context, fullName, token, output string) (bundleOutcome, error) {
		staging = output
		return writeBundleInStaging(t, "alpha_20261004_120000_abcd.bundle", "bundle-bytes")(ctx, fullName, token, output)
	}

	_ = s.runProtectedBackup(int64(42), st, tk, "j1", repo, state.DestinationLocal)

	job, ok := st.BackupJob("j1")
	if !ok || job.State != state.JobCompleted {
		t.Fatalf("job = %+v ok=%v", job, ok)
	}
	records := st.BackupRecordsForRepo("p1")
	if len(records) != 1 {
		t.Fatalf("records = %+v", records)
	}
	rec := records[0]
	if rec.Destination != state.DestinationLocal {
		t.Fatalf("destination = %q", rec.Destination)
	}
	if rec.DriveFileID != "" {
		t.Fatalf("local record driveFileID = %q", rec.DriveFileID)
	}
	if rec.Status != state.BackupStatusUploaded {
		t.Fatalf("status = %q", rec.Status)
	}
	if _, err := os.Stat(staging); !os.IsNotExist(err) {
		t.Fatalf("staging directory still present: %v", err)
	}
	view := latestBackupFor(records)
	if view == nil || view.Destination != state.DestinationLocal || view.DriveViewLink != "" {
		t.Fatalf("latest view = %+v", view)
	}
	jobView := s.toJobView(job)
	if jobView.DownloadURL != "/api/backup-jobs/j1/bundle" {
		t.Fatalf("downloadUrl = %q", jobView.DownloadURL)
	}
	cookie, _ := authedCookie(t, s, 42)
	recDL := request(t, s, http.MethodGet, jobView.DownloadURL, cookie)
	if recDL.Code != http.StatusOK {
		t.Fatalf("download status = %d, body %s", recDL.Code, recDL.Body.String())
	}
	if recDL.Body.String() != "bundle-bytes" {
		t.Fatalf("download body = %q", recDL.Body.String())
	}
	if got := recDL.Header().Get("Content-Disposition"); got != `attachment; filename="alpha_20261004_120000_abcd.bundle"` {
		t.Fatalf("content-disposition = %q", got)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("server copy still present after download: %+v", entries)
	}
	again := request(t, s, http.MethodGet, jobView.DownloadURL, cookie)
	if again.Code != http.StatusNotFound {
		t.Fatalf("second download status = %d, want 404", again.Code)
	}
}

func TestLocalBundleDownloadRejectedForOtherUser(t *testing.T) {
	st := &fakeStateStore{}
	repo := seedDriveJob(t, st)
	s, tk := envCloudServer(t, st, false)
	s.localBackupDir = t.TempDir()
	s.bundleBackup = writeBundleInStaging(t, "alpha.bundle", "secret-bytes")
	_ = s.runProtectedBackup(int64(42), st, tk, "j1", repo, state.DestinationLocal)

	other := &fakeStateStore{}
	s.userStores.stores[7] = &userStores{state: other, token: newFakeTokenStore()}
	cookie, _ := authedCookie(t, s, 7)
	rec := request(t, s, http.MethodGet, "/api/backup-jobs/j1/bundle", cookie)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if _, ok := s.localBundle("j1"); !ok {
		t.Fatal("other user's request removed the bundle")
	}
}

func TestLocalBackupCreationFailureLeavesNoPermanentBundle(t *testing.T) {
	st := &fakeStateStore{}
	repo := seedDriveJob(t, st)
	s, tk := envCloudServer(t, st, false)
	dir := t.TempDir()
	s.localBackupDir = dir
	var staging string
	s.bundleBackup = func(ctx context.Context, fullName, token, output string) (bundleOutcome, error) {
		staging = output
		if err := os.WriteFile(filepath.Join(output, "partial.bundle"), []byte("partial"), 0o644); err != nil {
			t.Fatal(err)
		}
		return bundleOutcome{}, errBoom
	}

	_ = s.runProtectedBackup(int64(42), st, tk, "j1", repo, state.DestinationLocal)

	job, _ := st.BackupJob("j1")
	if job.State != state.JobFailed {
		t.Fatalf("job = %+v", job)
	}
	if len(st.BackupRecords()) != 0 {
		t.Fatalf("record written after failure: %+v", st.BackupRecords())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("permanent dir not empty: %+v", entries)
	}
	if _, err := os.Stat(staging); !os.IsNotExist(err) {
		t.Fatalf("staging directory still present: %v", err)
	}
}

func TestLocalBackupSaveFailureWritesNoRecord(t *testing.T) {
	st := &fakeStateStore{}
	repo := seedDriveJob(t, st)
	s, tk := envCloudServer(t, st, false)
	parent := t.TempDir()
	blocker := filepath.Join(parent, "blocked")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.localBackupDir = filepath.Join(blocker, "gitsafe")
	s.bundleBackup = writeBundleInStaging(t, "alpha.bundle", "bytes")

	_ = s.runProtectedBackup(int64(42), st, tk, "j1", repo, state.DestinationLocal)

	job, _ := st.BackupJob("j1")
	if job.State != state.JobFailed {
		t.Fatalf("job = %+v", job)
	}
	if job.Error == "" {
		t.Fatal("expected a local save error")
	}
	if len(st.BackupRecords()) != 0 {
		t.Fatalf("record written after save failure: %+v", st.BackupRecords())
	}
}

func TestLocalBundleDownloadRequiresSession(t *testing.T) {
	s, _ := envCloudServer(t, &fakeStateStore{}, false)
	rec := request(t, s, http.MethodGet, "/api/backup-jobs/j1/bundle", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestDriveBackupStillRemovesStagingBundle(t *testing.T) {
	st := &fakeStateStore{}
	repo := seedDriveJob(t, st)
	s, tk := envCloudServer(t, st, true)
	bundle := filepath.Join(t.TempDir(), "repo.bundle")
	if err := os.WriteFile(bundle, []byte("drive-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.bundleBackup = func(ctx context.Context, fullName, token, output string) (bundleOutcome, error) {
		return bundleOutcome{BundlePath: bundle, BundleName: "repo.bundle", SizeBytes: 11, SHA256: "abc"}, nil
	}
	s.driveUpload = func(ctx context.Context, bundlePath, folderID, existingFileID, refreshToken string, onProgress func(int64, int64), logger *slog.Logger) (string, error) {
		if bundlePath != bundle {
			t.Fatalf("upload path = %q", bundlePath)
		}
		return "drive-file-1", nil
	}

	_ = s.runProtectedBackup(int64(42), st, tk, "j1", repo, state.DestinationDrive)

	job, _ := st.BackupJob("j1")
	if job.State != state.JobCompleted {
		t.Fatalf("job = %+v", job)
	}
	if _, err := os.Stat(bundle); !os.IsNotExist(err) {
		t.Fatalf("drive staging bundle still present: %v", err)
	}
	rec := st.BackupRecordsForRepo("p1")
	if len(rec) != 1 || rec[0].DriveFileID == "" || rec[0].Destination != state.DestinationDrive {
		t.Fatalf("drive record = %+v", rec)
	}
}

func TestBackupRepoLocalAcceptedWithoutDrive(t *testing.T) {
	st := &fakeStateStore{}
	s, _ := envCloudServer(t, st, false)
	dir := t.TempDir()
	s.localBackupDir = dir
	stubDiscovery(s, providers.Repository{ID: 100, FullName: "acme/alpha", DefaultBranch: "main"})
	s.bundleBackup = writeBundleInStaging(t, "alpha.bundle", "local")

	cookie, csrf := authedCookie(t, s, 42)
	rec := postBackup(t, s, cookie, csrf, "/api/repositories/100/backup", []byte(`{"destination":"local"}`))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	job := waitForJob(t, st, body.ID)
	if job.State != state.JobCompleted {
		t.Fatalf("job = %+v", job)
	}
	got := request(t, s, http.MethodGet, "/api/backup-jobs/"+body.ID+"/bundle", cookie)
	if got.Code != http.StatusOK || got.Body.String() != "local" {
		t.Fatalf("download = %d %q", got.Code, got.Body.String())
	}
}

func TestBackupRepoDriveDestinationStillRequiresDrive(t *testing.T) {
	st := &fakeStateStore{}
	s, _ := envCloudServer(t, st, false)
	stubDiscovery(s, providers.Repository{ID: 100, FullName: "acme/alpha", DefaultBranch: "main"})
	cookie, csrf := authedCookie(t, s, 42)
	rec := postBackup(t, s, cookie, csrf, "/api/repositories/100/backup", []byte(`{"destination":"drive"}`))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body.String())
	}
}

func TestBackupRepoRejectsUnknownDestination(t *testing.T) {
	st := &fakeStateStore{}
	s, _ := envCloudServer(t, st, true)
	stubDiscovery(s, providers.Repository{ID: 100, FullName: "acme/alpha", DefaultBranch: "main"})
	cookie, csrf := authedCookie(t, s, 42)
	rec := postBackup(t, s, cookie, csrf, "/api/repositories/100/backup", []byte(`{"destination":"s3"}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

func TestBackupAllLocalWithoutDrive(t *testing.T) {
	st := &fakeStateStore{}
	s, _ := envCloudServer(t, st, false)
	s.localBackupDir = t.TempDir()
	stubDiscovery(s, providers.Repository{ID: 1, FullName: "acme/one", DefaultBranch: "main"})
	s.bundleBackup = writeBundleInStaging(t, "one.bundle", "one")
	cookie, csrf := authedCookie(t, s, 42)
	rec := postBackup(t, s, cookie, csrf, "/api/backups", []byte(`{"destination":"local"}`))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Jobs []jobView `json:"jobs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Jobs) != 1 {
		t.Fatalf("jobs = %+v", body.Jobs)
	}
	job := waitForJob(t, st, body.Jobs[0].ID)
	if job.State != state.JobCompleted {
		t.Fatalf("job = %+v", job)
	}
	got := request(t, s, http.MethodGet, "/api/backup-jobs/"+body.Jobs[0].ID+"/bundle", cookie)
	if got.Code != http.StatusOK || got.Body.String() != "one" {
		t.Fatalf("download = %d %q", got.Code, got.Body.String())
	}
}
