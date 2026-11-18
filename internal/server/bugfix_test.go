package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/b-isry/gitsafe/internal/archiver"
	"github.com/b-isry/gitsafe/internal/state"
	"github.com/b-isry/gitsafe/internal/tokenstore"
)

func waitForJobs(t *testing.T, st *fakeStateStore, n int, terminal func(string) bool) []state.BackupJob {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		jobs := st.BackupJobs()
		done := 0
		for _, j := range jobs {
			if terminal(j.State) {
				done++
			}
		}
		if done >= n {
			return jobs
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d/%d jobs finished: %+v", done, n, jobs)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestUserBackupsQueueInsteadOfFailing is the regression test for "Back up
// all" backing up a single repository: with the per-user limit active, extra
// jobs of the same user used to fail at once with "a backup is already running
// for this user". They must now wait their turn and all complete, one at a time.
func TestUserBackupsQueueInsteadOfFailing(t *testing.T) {
	st := &fakeStateStore{}
	const total = 3
	for i := 0; i < total; i++ {
		r := seedProtectedRepo(st, string(rune('a'+i)), "acme/repo-"+string(rune('a'+i)), "main")
		_ = r
	}
	s, tk := envCloudServer(t, st, true)
	s.driveUpload = func(ctx context.Context, bundlePath, folderID, existingFileID, refreshToken string, onProgress func(int64, int64), logger *slog.Logger) (string, error) {
		return "drive-x", nil
	}
	var inFlight, maxSeen atomic.Int32
	s.bundleBackup = func(ctx context.Context, fullName, token, output string) (bundleOutcome, error) {
		n := inFlight.Add(1)
		for {
			m := maxSeen.Load()
			if n <= m || maxSeen.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		inFlight.Add(-1)
		return bundleOutcome{BundlePath: "/out/x.bundle", BundleName: "x.bundle", SizeBytes: 1, SHA256: "a"}, nil
	}
	for i := 0; i < total; i++ {
		if _, err := s.startProtectedBackup(42, st, tk, string(rune('a'+i)), state.DestinationDrive); err != nil {
			t.Fatalf("start %d: %v", i, err)
		}
	}
	jobs := waitForJobs(t, st, total, state.IsTerminalState)
	for _, j := range jobs {
		if j.State != state.JobCompleted {
			t.Fatalf("job %s = %s (%s), want completed", j.ID, j.State, j.Error)
		}
	}
	if maxSeen.Load() != 1 {
		t.Fatalf("one user's backups ran %d at once, want 1", maxSeen.Load())
	}
}

// TestInterruptedJobDoesNotBlockRepository: a job left "interrupted" by a
// restart must not make the repository report "already running" forever.
func TestInterruptedJobDoesNotBlockRepository(t *testing.T) {
	st := &fakeStateStore{}
	seedProtectedRepo(st, "p1", "acme/alpha", "main")
	st.CreateBackupJob(state.BackupJob{ID: "old", ProtectedRepoID: "p1", State: state.JobInterrupted})
	s, tk := envCloudServer(t, st, true)
	s.driveUpload = func(ctx context.Context, bundlePath, folderID, existingFileID, refreshToken string, onProgress func(int64, int64), logger *slog.Logger) (string, error) {
		return "drive-x", nil
	}
	stubDriveBundler(s)
	if _, err := s.startProtectedBackup(42, st, tk, "p1", state.DestinationDrive); err != nil {
		t.Fatalf("start after interrupted job: %v", err)
	}
	waitForJobs(t, st, 2, state.IsTerminalState)
}

// TestRebackupPassesPreviousDriveFile verifies re-backups go through the
// uploader seam with the previous Drive file ID (replace in place), instead of
// a separate untested code path that called Google directly.
func TestRebackupPassesPreviousDriveFile(t *testing.T) {
	st := &fakeStateStore{}
	repo := seedDriveJob(t, st)
	st.AddBackupRecord(state.BackupRecord{ID: "r1", ProtectedRepoID: "p1", DriveFileID: "prev-file", Status: state.BackupStatusUploaded})
	st.AddBackupRecord(state.BackupRecord{ID: "r2", ProtectedRepoID: "p1", Destination: state.DestinationLocal, Status: state.BackupStatusUploaded})
	s, tk := envCloudServer(t, st, true)
	stubDriveBundler(s)
	var gotExisting string
	s.driveUpload = func(ctx context.Context, bundlePath, folderID, existingFileID, refreshToken string, onProgress func(int64, int64), logger *slog.Logger) (string, error) {
		gotExisting = existingFileID
		return "prev-file", nil
	}
	if err := s.runProtectedBackup(42, st, tk, "j1", repo, state.DestinationDrive); err != nil {
		t.Fatal(err)
	}
	if gotExisting != "prev-file" {
		t.Fatalf("existingFileID = %q, want prev-file", gotExisting)
	}
	if _, ok := s.jobProgressFor("j1"); ok {
		t.Fatal("upload progress must be cleared when the job ends")
	}
}

func TestDriveAuthFailureIsClassified(t *testing.T) {
	st := &fakeStateStore{}
	repo := seedDriveJob(t, st)
	s, tk := envCloudServer(t, st, true)
	stubDriveBundler(s)
	s.driveUpload = func(ctx context.Context, bundlePath, folderID, existingFileID, refreshToken string, onProgress func(int64, int64), logger *slog.Logger) (string, error) {
		return "", errDriveAuth
	}
	err := s.runProtectedBackup(42, st, tk, "j1", repo, state.DestinationDrive)
	if !errors.Is(err, errDriveAuth) {
		t.Fatalf("err = %v, want errDriveAuth", err)
	}
	if j, _ := st.BackupJob("j1"); j.State != state.JobFailed {
		t.Fatalf("job = %+v", j)
	}
}

func TestEmptyRepositoryHasFriendlyMessage(t *testing.T) {
	st := &fakeStateStore{}
	repo := seedDriveJob(t, st)
	s, tk := envCloudServer(t, st, true)
	s.bundleBackup = func(ctx context.Context, fullName, token, output string) (bundleOutcome, error) {
		return bundleOutcome{}, archiver.ErrEmptyRepository
	}
	err := s.runProtectedBackup(42, st, tk, "j1", repo, state.DestinationDrive)
	if !errors.Is(err, archiver.ErrEmptyRepository) {
		t.Fatalf("err = %v", err)
	}
	if j, _ := st.BackupJob("j1"); j.Error != "repository is empty; there are no commits to back up" {
		t.Fatalf("job error = %q", j.Error)
	}
}

// TestShutdownMarksJobsInterrupted verifies a backup cut short by shutdown is
// recorded as interrupted (and does not block the repository afterwards).
func TestShutdownMarksJobsInterrupted(t *testing.T) {
	st := &fakeStateStore{}
	seedProtectedRepo(st, "p1", "acme/alpha", "main")
	s, tk := envCloudServer(t, st, true)
	started := make(chan struct{})
	s.bundleBackup = func(ctx context.Context, fullName, token, output string) (bundleOutcome, error) {
		close(started)
		<-ctx.Done()
		return bundleOutcome{}, ctx.Err()
	}
	if _, err := s.startProtectedBackup(42, st, tk, "p1", state.DestinationDrive); err != nil {
		t.Fatal(err)
	}
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	jobs := st.BackupJobs()
	if len(jobs) != 1 || jobs[0].State != state.JobInterrupted {
		t.Fatalf("jobs = %+v, want one interrupted job", jobs)
	}
}

// TestDeleteAccountRemovesTokensAndPersists: account deletion used to leave
// the encrypted tokens in the token store and only clear state in memory.
func TestDeleteAccountRemovesTokensAndPersists(t *testing.T) {
	st := &fakeStateStore{}
	s, tk := envCloudServer(t, st, true)
	cookie, csrf := authedCookie(t, s, 42)
	req := httptest.NewRequest(http.MethodPost, "/api/account/delete", nil)
	req.RemoteAddr = "127.0.0.1:1"
	req.AddCookie(cookie)
	req.Header.Set("X-CSRF-Token", csrf)
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s", rec.Code, rec.Body.String())
	}
	for _, ref := range []string{tokenstore.GitHubTokenFor(42), tokenstore.DriveTokenFor(42)} {
		if _, ok := tk.data[ref]; ok {
			t.Errorf("token %s still stored after account deletion", ref)
		}
	}
	if !st.saved {
		t.Error("cleared state was not persisted")
	}
	if _, ok := st.GitHubConnection(); ok {
		t.Error("github connection survived account deletion")
	}
}

func TestExpiredLocalBundlesAreSwept(t *testing.T) {
	s := newCloudServer(t, &fakeStateStore{}, newFakeTokenStore(), &GitHubOAuth{ClientID: "id"})
	s.localBackupDir = t.TempDir()
	path, err := s.stageBundleForDownload(realBundleFile(t), "x.bundle")
	if err != nil {
		t.Fatal(err)
	}
	s.rememberLocalBundle("j-old", path, "x.bundle")
	s.localHoldMu.Lock()
	h := s.localHold["j-old"]
	h.createdAt = time.Now().Add(-2 * localBundleTTL)
	s.localHold["j-old"] = h
	s.localHoldMu.Unlock()
	s.sweepExpiredLocalBundles()
	if _, ok := s.localBundle("j-old"); ok {
		t.Fatal("expired bundle still held")
	}
}

func realBundleFile(t *testing.T) string {
	t.Helper()
	p := t.TempDir() + "/src.bundle"
	if err := writeFile(p, "data"); err != nil {
		t.Fatal(err)
	}
	return p
}

func writeFile(p, s string) error { return os.WriteFile(p, []byte(s), 0o600) }
