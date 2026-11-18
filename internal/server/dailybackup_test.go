package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/b-isry/gitsafe/internal/providers"
	"github.com/b-isry/gitsafe/internal/state"
)

const testCronSecret = "test-only-cron-secret-0123456789abcdef"

// dailyServer is a cloud server with Drive connected, daily backups enabled
// for user 42, two GitHub repositories, and stubbed git/Drive work.
func dailyServer(t *testing.T) (*Server, *fakeStateStore, *atomic.Int32) {
	t.Helper()
	st := &fakeStateStore{}
	s, _ := envCloudServer(t, st, true)
	s.cronSecret = testCronSecret
	s.daily = newMemoryDailyStore()
	if err := s.daily.SetEnabled(context.Background(), 42, true); err != nil {
		t.Fatal(err)
	}
	s.githubLister = func(ctx context.Context, token string) ([]providers.Repository, error) {
		return []providers.Repository{
			{ID: 1, FullName: "acme/one", DefaultBranch: "main", PushedAt: time.Now()},
			{ID: 2, FullName: "acme/two", DefaultBranch: "main", PushedAt: time.Now()},
		}, nil
	}
	var uploads atomic.Int32
	s.driveUpload = func(ctx context.Context, bundlePath, folderID, existingFileID, refreshToken string, onProgress func(int64, int64), logger *slog.Logger) (string, error) {
		uploads.Add(1)
		return "drive-file", nil
	}
	s.bundleBackup = func(ctx context.Context, fullName, token, output string) (bundleOutcome, error) {
		return bundleOutcome{BundlePath: output + "/x.bundle", BundleName: "x.bundle", SizeBytes: 1, SHA256: "a"}, nil
	}
	return s, st, &uploads
}

func cronRequest(t *testing.T, s *Server, method, secret string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "/api/cron/backup", nil)
	req.RemoteAddr = "203.0.113.9:1234"
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	return rec
}

func waitCronIdle(t *testing.T, s *Server) cronRun {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		s.cron.mu.Lock()
		running, last := s.cron.running, s.cron.last
		var run cronRun
		if last != nil {
			run = *last
		}
		s.cron.mu.Unlock()
		if !running && last != nil {
			return run
		}
		if time.Now().After(deadline) {
			t.Fatal("daily run did not finish")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func dailyResult(t *testing.T, s *Server) DailySettings {
	t.Helper()
	got, err := s.daily.Get(context.Background(), 42)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestCronEndpointDisabledWithoutSecret(t *testing.T) {
	s, _, _ := dailyServer(t)
	s.cronSecret = ""
	if rec := cronRequest(t, s, http.MethodPost, testCronSecret); rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestCronEndpointRequiresSecret(t *testing.T) {
	s, _, uploads := dailyServer(t)
	for _, secret := range []string{"", "wrong", testCronSecret + "x", strings.ToUpper(testCronSecret)} {
		if rec := cronRequest(t, s, http.MethodPost, secret); rec.Code != http.StatusUnauthorized {
			t.Fatalf("secret %q: status = %d, want 401", secret, rec.Code)
		}
	}
	if rec := cronRequest(t, s, http.MethodGet, "nope"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("status endpoint without secret = %d, want 401", rec.Code)
	}
	if uploads.Load() != 0 {
		t.Fatal("unauthorized request triggered work")
	}
}

func TestCronSecretFromEnv(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(new(strings.Builder), nil))
	t.Setenv(CronSecretEnv, "short")
	if cronSecretFromEnv(logger) != "" {
		t.Fatal("short secret must disable the endpoint")
	}
	t.Setenv(CronSecretEnv, testCronSecret)
	if cronSecretFromEnv(logger) != testCronSecret {
		t.Fatal("valid secret not accepted")
	}
}

func TestCronRunBacksUpAndIsResumable(t *testing.T) {
	s, st, uploads := dailyServer(t)
	rec := cronRequest(t, s, http.MethodPost, testCronSecret)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", rec.Code, rec.Body.String())
	}
	run := waitCronIdle(t, s)
	if run.Users != 1 || run.OK != 2 || run.Failed != 0 {
		t.Fatalf("run = %+v", run)
	}
	res := dailyResult(t, s)
	if res.LastStatus != dailyStatusOK || res.LastOK != 2 || res.LastRunAt.IsZero() {
		t.Fatalf("result = %+v", res)
	}
	if len(st.BackupJobs()) != 2 {
		t.Fatalf("jobs = %d, want 2", len(st.BackupJobs()))
	}

	// A second run the same day skips repositories that are already fresh.
	cronRequest(t, s, http.MethodPost, testCronSecret)
	run = waitCronIdle(t, s)
	if run.OK != 0 || run.Skipped != 2 || uploads.Load() != 2 {
		t.Fatalf("second run = %+v uploads=%d", run, uploads.Load())
	}

	// Status endpoint reports the last run.
	status := cronRequest(t, s, http.MethodGet, testCronSecret)
	var body struct {
		Running bool    `json:"running"`
		Run     cronRun `json:"run"`
	}
	if err := json.Unmarshal(status.Body.Bytes(), &body); err != nil || body.Running || body.Run.ID != run.ID {
		t.Fatalf("status = %s (%v)", status.Body.String(), err)
	}
}

func TestCronSecondTriggerWhileRunning(t *testing.T) {
	s, _, _ := dailyServer(t)
	release := make(chan struct{})
	s.bundleBackup = func(ctx context.Context, fullName, token, output string) (bundleOutcome, error) {
		<-release
		return bundleOutcome{BundlePath: output + "/x.bundle", BundleName: "x.bundle", SizeBytes: 1}, nil
	}
	first := cronRequest(t, s, http.MethodPost, testCronSecret)
	second := cronRequest(t, s, http.MethodPost, testCronSecret)
	close(release)
	if first.Code != http.StatusAccepted || second.Code != http.StatusAccepted {
		t.Fatalf("codes = %d, %d", first.Code, second.Code)
	}
	if !strings.Contains(second.Body.String(), "already_running") {
		t.Fatalf("second trigger = %s", second.Body.String())
	}
	waitCronIdle(t, s)
}

func TestCronGitHubRevokedMarksUser(t *testing.T) {
	s, _, uploads := dailyServer(t)
	s.githubLister = func(ctx context.Context, token string) ([]providers.Repository, error) {
		return nil, providers.ErrUnauthorized
	}
	cronRequest(t, s, http.MethodPost, testCronSecret)
	run := waitCronIdle(t, s)
	res := dailyResult(t, s)
	if run.NeedsReauth != 1 || res.NeedsReauth != reauthGitHub || res.LastStatus != dailyStatusNeedsReauth || !res.Enabled {
		t.Fatalf("run=%+v result=%+v", run, res)
	}
	if uploads.Load() != 0 {
		t.Fatal("must not upload for a revoked user")
	}
	// Reconnecting GitHub clears the flag.
	s.afterGitHubConnected(context.Background(), 42)
	if got := dailyResult(t, s); got.NeedsReauth != "" {
		t.Fatalf("flag not cleared: %+v", got)
	}
}

func TestCronDriveRevokedStopsUser(t *testing.T) {
	s, _, uploads := dailyServer(t)
	s.driveUpload = func(ctx context.Context, bundlePath, folderID, existingFileID, refreshToken string, onProgress func(int64, int64), logger *slog.Logger) (string, error) {
		uploads.Add(1)
		return "", errDriveAuth
	}
	cronRequest(t, s, http.MethodPost, testCronSecret)
	waitCronIdle(t, s)
	res := dailyResult(t, s)
	if res.NeedsReauth != reauthDrive || uploads.Load() != 1 {
		t.Fatalf("result=%+v uploads=%d (must stop after the first auth failure)", res, uploads.Load())
	}
}

func TestCronPerRepoFailureIsPartial(t *testing.T) {
	s, _, _ := dailyServer(t)
	s.bundleBackup = func(ctx context.Context, fullName, token, output string) (bundleOutcome, error) {
		if fullName == "acme/one" {
			return bundleOutcome{}, errors.New("clone exploded")
		}
		return bundleOutcome{BundlePath: output + "/x.bundle", BundleName: "x.bundle", SizeBytes: 1}, nil
	}
	cronRequest(t, s, http.MethodPost, testCronSecret)
	waitCronIdle(t, s)
	res := dailyResult(t, s)
	if res.LastStatus != dailyStatusPartial || res.LastOK != 1 || res.LastFailed != 1 || !strings.Contains(res.LastMessage, "acme/one") {
		t.Fatalf("result = %+v", res)
	}
}

func TestCronSkipsUsersWhoOptedOut(t *testing.T) {
	s, _, uploads := dailyServer(t)
	_ = s.daily.SetEnabled(context.Background(), 42, false)
	cronRequest(t, s, http.MethodPost, testCronSecret)
	if run := waitCronIdle(t, s); run.Users != 0 || uploads.Load() != 0 {
		t.Fatalf("run = %+v", run)
	}
}

func TestDailyUpToDate(t *testing.T) {
	now := time.Now()
	st := &fakeStateStore{}
	repo := seedProtectedRepo(st, "p", "acme/x", "main")
	if dailyUpToDate(st, repo.ID, now, now) {
		t.Fatal("never backed up must not be up to date")
	}
	st.AddBackupRecord(state.BackupRecord{ID: "r1", ProtectedRepoID: repo.ID, CreatedAt: now.Add(-48 * time.Hour), DriveFileID: "f"})
	if !dailyUpToDate(st, repo.ID, now.Add(-72*time.Hour), now) {
		t.Fatal("no push since the last backup: should skip")
	}
	if dailyUpToDate(st, repo.ID, now.Add(-time.Hour), now) {
		t.Fatal("pushed after the last backup: must back up")
	}
	if dailyUpToDate(st, repo.ID, time.Time{}, now) {
		t.Fatal("unknown push time: must back up")
	}
	st.AddBackupRecord(state.BackupRecord{ID: "r2", ProtectedRepoID: repo.ID, CreatedAt: now.Add(-time.Hour), DriveFileID: "f"})
	if !dailyUpToDate(st, repo.ID, now, now) {
		t.Fatal("backed up an hour ago: should skip")
	}
}

func TestDailyBackupSettingsAPI(t *testing.T) {
	s, _, _ := dailyServer(t)
	_ = s.daily.SetEnabled(context.Background(), 42, false)
	cookie, csrf := authedCookie(t, s, 42)

	if rec := requestWithCSRF(t, s, http.MethodPut, "/api/daily-backup", cookie, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("PUT without CSRF = %d, want 403", rec.Code)
	}
	put := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPut, "/api/daily-backup", strings.NewReader(body))
		req.RemoteAddr = "127.0.0.1:5555"
		req.AddCookie(cookie)
		req.Header.Set("X-CSRF-Token", csrf)
		rec := httptest.NewRecorder()
		s.Routes().ServeHTTP(rec, req)
		return rec
	}
	if rec := put(`{}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("PUT {} = %d, want 400", rec.Code)
	}
	rec := put(`{"enabled":true}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"enabled":true`) {
		t.Fatalf("enable = %d %s", rec.Code, rec.Body.String())
	}
	get := request(t, s, http.MethodGet, "/api/daily-backup", cookie)
	if !strings.Contains(get.Body.String(), `"available":true`) || !strings.Contains(get.Body.String(), `"enabled":true`) {
		t.Fatalf("GET = %s", get.Body.String())
	}

	// Disconnecting Drive turns daily backups off.
	if rec := requestWithCSRF(t, s, http.MethodDelete, "/api/connections/drive", cookie, csrf); rec.Code != http.StatusOK {
		t.Fatalf("disconnect = %d", rec.Code)
	}
	if dailyResult(t, s).Enabled {
		t.Fatal("daily backups must be disabled after Drive disconnect")
	}
	// ... and cannot be re-enabled without Drive.
	if rec := put(`{"enabled":true}`); rec.Code != http.StatusConflict {
		t.Fatalf("enable without Drive = %d, want 409", rec.Code)
	}
}

func TestDailyBackupUnavailableWithoutCron(t *testing.T) {
	s, _, _ := dailyServer(t)
	s.cronSecret = ""
	cookie, _ := authedCookie(t, s, 42)
	get := request(t, s, http.MethodGet, "/api/daily-backup", cookie)
	if !strings.Contains(get.Body.String(), `"available":false`) {
		t.Fatalf("GET = %s", get.Body.String())
	}
}

func TestAccountDeletionRemovesDailySettings(t *testing.T) {
	s, _, _ := dailyServer(t)
	cookie, csrf := authedCookie(t, s, 42)
	if rec := requestWithCSRF(t, s, http.MethodPost, "/api/account/delete", cookie, csrf); rec.Code != http.StatusOK {
		t.Fatalf("delete = %d %s", rec.Code, rec.Body.String())
	}
	ids, _ := s.daily.ListEnabled(context.Background())
	if len(ids) != 0 {
		t.Fatalf("daily settings survived account deletion: %v", ids)
	}
}
