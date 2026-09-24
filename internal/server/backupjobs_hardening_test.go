package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/b-isry/gitsafe/internal/state"
)

// TestRunProtectedBackupSanitizesJobError verifies that a bundler failure whose
// error string embeds a tokenized clone URL does NOT leak that secret into the
// persisted job.Error or the API view.
func TestRunProtectedBackupSanitizesJobError(t *testing.T) {
	st := &fakeStateStore{}
	repo := seedDriveJob(t, st)
	s, tk := envCloudServer(t, st, true) // Drive connected: backup flow reaches the bundler
	s.driveUpload = func(ctx context.Context, bundlePath, folderID, refreshToken string, onProgress func(int64, int64), logger *slog.Logger) (string, error) {
		return "drive-x", nil
	}
	s.bundleBackup = func(ctx context.Context, fullName, token, output string) (bundleOutcome, error) {
		return bundleOutcome{}, fmt.Errorf("mirror clone \"https://x-access-token:SUPERSECRET@github.com/%s.git\": boom", fullName)
	}

	userID := int64(42)
	s.runProtectedBackup(userID, st, tk, "j1", repo)

	job, ok := st.BackupJob("j1")
	if !ok || job.State != state.JobFailed {
		t.Fatalf("job = %+v ok=%v", job, ok)
	}
	if strings.Contains(job.Error, "SUPERSECRET") {
		t.Fatalf("job.Error leaked the token: %q", job.Error)
	}
	if !strings.Contains(job.Error, "boom") {
		t.Fatalf("job.Error lost the useful message: %q", job.Error)
	}
	// The API view must also be clean.
	v := s.toJobView(job)
	if strings.Contains(v.Error, "SUPERSECRET") {
		t.Fatalf("job view leaked the token: %q", v.Error)
	}
}

// TestStartProtectedBackupRemovesOrphanJobOnSaveFailure verifies that when the
// initial state persistence fails, the freshly-created job is removed so a
// retry is not blocked by an orphaned non-terminal job.
func TestStartProtectedBackupRemovesOrphanJobOnSaveFailure(t *testing.T) {
	st := &fakeStateStore{}
	seedProtectedRepo(st, "p1", "acme/alpha", "main")
	s, tk := envCloudServer(t, st, true)
	st.errors = map[string]error{"save": errBoom}
	s.bundleBackup = func(ctx context.Context, fullName, token, output string) (bundleOutcome, error) {
		return bundleOutcome{}, nil
	}

	if _, err := s.startProtectedBackup(int64(42), st, tk, "p1"); err == nil {
		t.Fatal("expected startProtectedBackup to fail on save error")
	}
	if len(st.BackupJobs()) != 0 {
		t.Fatalf("orphaned job not removed: %+v", st.BackupJobs())
	}
}

// TestProtectedBackupConcurrentTriggers verifies that concurrent duplicate
// backup requests for the same repository create exactly one job.
func TestProtectedBackupConcurrentTriggers(t *testing.T) {
	st := &fakeStateStore{}
	seedProtectedRepo(st, "p1", "acme/alpha", "main")
	s, tk := envCloudServer(t, st, true)
	s.driveUpload = func(ctx context.Context, bundlePath, folderID, refreshToken string, onProgress func(int64, int64), logger *slog.Logger) (string, error) {
		return "drive-x", nil
	}
	// Block the spawned job goroutine in the "cloning" phase so the job stays
	// non-terminal while all 16 triggers run, making the dedup assertions
	// deterministic.
	release := make(chan struct{})
	s.bundleBackup = func(ctx context.Context, fullName, token, output string) (bundleOutcome, error) {
		<-release
		return bundleOutcome{BundlePath: "/out/x.bundle", BundleName: "x.bundle", SizeBytes: 1, SHA256: "a"}, nil
	}

	const n = 16
	var wg sync.WaitGroup
	start := make(chan struct{})
	var mu sync.Mutex
	results := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := s.startProtectedBackup(int64(42), st, tk, "p1")
			mu.Lock()
			results[i] = err
			mu.Unlock()
		}(i)
	}
	close(start)
	wg.Wait()
	close(release)

	success := 0
	for _, err := range results {
		if err == nil {
			success++
		} else if err != errJobInFlight {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if success != 1 {
		t.Fatalf("expected exactly 1 accepted job, got %d", success)
	}
	if len(st.BackupJobs()) != 1 {
		t.Fatalf("expected exactly 1 created job, got %d", len(st.BackupJobs()))
	}
}

// TestProtectedBackupHistoryListWhileRunning verifies reading history/jobs while
// an async job runs does not panic or corrupt state (safety under -race).
func TestProtectedBackupHistoryListWhileRunning(t *testing.T) {
	st := &fakeStateStore{}
	seedProtectedRepo(st, "p1", "acme/alpha", "main")
	s, tk := envCloudServer(t, st, true)
	s.driveUpload = func(ctx context.Context, bundlePath, folderID, refreshToken string, onProgress func(int64, int64), logger *slog.Logger) (string, error) {
		return "drive-x", nil
	}
	s.bundleBackup = func(ctx context.Context, fullName, token, output string) (bundleOutcome, error) {
		return bundleOutcome{BundlePath: "/out/x.bundle", BundleName: "x.bundle", SizeBytes: 1, SHA256: "a"}, nil
	}

	job, err := s.startProtectedBackup(int64(42), st, tk, "p1")
	if err != nil {
		t.Fatal(err)
	}
	cookie, _ := authedCookie(t, s, 42)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := request(t, s, http.MethodGet, "/api/protected-repositories/p1/backup-jobs", cookie)
			if req.Code != http.StatusOK {
				t.Errorf("jobs list status = %d", req.Code)
			}
			req2 := request(t, s, http.MethodGet, "/api/backup-jobs/"+job.ID, cookie)
			if req2.Code != http.StatusOK {
				t.Errorf("job status = %d", req2.Code)
			}
		}()
	}
	wg.Wait()
}
