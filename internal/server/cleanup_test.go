package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/b-isry/gitsafe/internal/config"
	"github.com/b-isry/gitsafe/internal/state"
)

var discardLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

// seedRecord adds a backup record to the server's configured state store.
func seedRecord(t *testing.T, s *Server, rec state.BackupRecord) {
	t.Helper()
	if s.stateStore == nil {
		t.Fatalf("stateStore not configured")
	}
	s.stateStore.AddBackupRecord(rec)
}

func seedJob(t *testing.T, s *Server, j state.BackupJob) {
	t.Helper()
	if s.stateStore == nil {
		t.Fatalf("stateStore not configured")
	}
	s.stateStore.CreateBackupJob(j)
}

// writeBundle creates a real .bundle file in the server's output dir.
func writeBundle(t *testing.T, s *Server, name string) {
	t.Helper()
	dir := s.app.OutputPath
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte("bundle-data"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func getRecord(s *Server, id string) (state.BackupRecord, bool) {
	for _, rec := range s.stateStore.BackupRecords() {
		if rec.ID == id {
			return rec, true
		}
	}
	return state.BackupRecord{}, false
}

func retentionRecord(id, repo, fullName, bundle string, daysOld int, driveID, status string) state.BackupRecord {
	when := time.Now().Add(-time.Duration(daysOld) * 24 * time.Hour)
	return state.BackupRecord{
		ID:              id,
		ProtectedRepoID: repo,
		FullName:        fullName,
		CreatedAt:       when,
		MirroredAt:      when,
		DefaultBranch:   "main",
		BundleName:      bundle,
		DriveFileID:     driveID,
		Status:          status,
	}
}

func v2Bundle(repo string, n int) string {
	// <prefix>_<YYYYMMDD_HHMMSS>_<8hex>.bundle (mirrors archiver.BundleRemoteRepo)
	return fmt.Sprintf("%s_20260904_120000_%08x.bundle", repo, n)
}

func TestRetentionGetReturnsPolicyAndLastCleanup(t *testing.T) {
	s := newCloudServer(t, &fakeStateStore{}, newFakeTokenStore(), nil)
	rec := request(t, s, http.MethodGet, "/api/retention", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body struct {
		Retention   retentionView  `json:"retention"`
		LastCleanup map[string]any `json:"lastCleanup"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Retention.KeepLocal != 0 || body.Retention.DriveConfigured {
		t.Fatalf("unexpected default retention view: %+v", body.Retention)
	}
}

func TestRetentionPutRequiresAuthAndCSRF(t *testing.T) {
	s := newCloudServer(t, &fakeStateStore{}, newFakeTokenStore(), nil)

	// No session → 401.
	req := httptest.NewRequest(http.MethodPut, "/api/retention", bytes.NewReader([]byte(`{"keepLocal":2}`)))
	req.RemoteAddr = "127.0.0.1:55555"
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no-session status = %d, want 401", rec.Code)
	}

	// Session but no CSRF → 403.
	cookie, _ := csrfCookie(t, s)
	req = httptest.NewRequest(http.MethodPut, "/api/retention", bytes.NewReader([]byte(`{"keepLocal":2}`)))
	req.RemoteAddr = "127.0.0.1:55555"
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("no-csrf status = %d, want 403", rec.Code)
	}
}

func TestRetentionPutUpdatesAndPersistsPolicy(t *testing.T) {
	s := newCloudServer(t, &fakeStateStore{}, newFakeTokenStore(), nil)
	cookie, csrf := csrfCookie(t, s)
	req := httptest.NewRequest(http.MethodPut, "/api/retention",
		bytes.NewReader([]byte(`{"keepLocal":3,"keepJobs":1,"keepLocalDays":30,"keepDriveDays":60,"driveRetentionEnabled":true}`)))
	req.RemoteAddr = "127.0.0.1:55555"
	req.AddCookie(cookie)
	req.Header.Set("X-CSRF-Token", csrf)
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	loaded := s.app.ConfigSnapshot()
	if loaded.Retention.KeepLocal != 3 || loaded.Retention.KeepJobs != 1 ||
		loaded.Retention.KeepLocalDays != 30 || loaded.Retention.KeepDriveDays != 60 ||
		!loaded.Retention.DriveRetention {
		t.Fatalf("policy not applied to config: %+v", loaded.Retention)
	}
}

func TestRetentionPutRejectsNegative(t *testing.T) {
	s := newCloudServer(t, &fakeStateStore{}, newFakeTokenStore(), nil)
	cookie, csrf := csrfCookie(t, s)
	req := httptest.NewRequest(http.MethodPut, "/api/retention",
		bytes.NewReader([]byte(`{"keepLocal":-1}`)))
	req.RemoteAddr = "127.0.0.1:55555"
	req.AddCookie(cookie)
	req.Header.Set("X-CSRF-Token", csrf)
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
}

func TestRetentionPutRejectsNegativeOverride(t *testing.T) {
	s := newCloudServer(t, &fakeStateStore{}, newFakeTokenStore(), nil)
	cookie, csrf := csrfCookie(t, s)
	req := httptest.NewRequest(http.MethodPut, "/api/retention",
		bytes.NewReader([]byte(`{"overrides":[{"repositoryId":5,"keepJobs":-2}]}`)))
	req.RemoteAddr = "127.0.0.1:55555"
	req.AddCookie(cookie)
	req.Header.Set("X-CSRF-Token", csrf)
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}

	req2 := httptest.NewRequest(http.MethodPut, "/api/retention",
		bytes.NewReader([]byte(`{"overrides":[{"repositoryId":0,"keepJobs":2}]}`)))
	req2.RemoteAddr = "127.0.0.1:55555"
	req2.AddCookie(cookie)
	req2.Header.Set("X-CSRF-Token", csrf)
	rec2 := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 for missing repo id; body=%s", rec2.Code, rec2.Body.String())
	}
}

func TestRetentionPutRejectsTooManyOverrides(t *testing.T) {
	s := newCloudServer(t, &fakeStateStore{}, newFakeTokenStore(), nil)
	cookie, csrf := csrfCookie(t, s)
	body := `{"overrides":[`
	for i := 0; i <= maxRetentionOverrides; i++ {
		if i > 0 {
			body += ","
		}
		body += `{"repositoryId":` + fmt.Sprint(i+1) + `}`
	}
	body += `]}`
	req := httptest.NewRequest(http.MethodPut, "/api/retention", bytes.NewBufferString(body))
	req.RemoteAddr = "127.0.0.1:55555"
	req.AddCookie(cookie)
	req.Header.Set("X-CSRF-Token", csrf)
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
}

func TestRetentionPutRejectsHugeHistoryLimit(t *testing.T) {
	s := newCloudServer(t, &fakeStateStore{}, newFakeTokenStore(), nil)
	cookie, csrf := csrfCookie(t, s)
	req := httptest.NewRequest(http.MethodPut, "/api/retention",
		bytes.NewReader([]byte(`{"cleanupHistoryLimit":99999999}`)))
	req.RemoteAddr = "127.0.0.1:55555"
	req.AddCookie(cookie)
	req.Header.Set("X-CSRF-Token", csrf)
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
}

func TestRetentionPutPersistsOverridesAndNotifications(t *testing.T) {
	s := newCloudServer(t, &fakeStateStore{}, newFakeTokenStore(), nil)
	cookie, csrf := csrfCookie(t, s)

	put := func(body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPut, "/api/retention", bytes.NewReader([]byte(body)))
		req.RemoteAddr = "127.0.0.1:55555"
		req.AddCookie(cookie)
		req.Header.Set("X-CSRF-Token", csrf)
		rec := httptest.NewRecorder()
		s.Routes().ServeHTTP(rec, req)
		return rec
	}

	keepAll := 999
	rec := put(`{"keepLocalDays":1,"notifications":{"enabled":true,"webhookUrl":"https://hooks.example.com/x","webhookAuthHeader":"X-Token","webhookAuthToken":"secret","webhookTimeoutSeconds":5},"overrides":[{"repositoryId":100,"keepLocalDays":` + fmt.Sprint(keepAll) + `}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	loaded := s.app.ConfigSnapshot()
	if !loaded.Notifications.Enabled || loaded.Notifications.WebhookURL != "https://hooks.example.com/x" ||
		loaded.Notifications.WebhookAuthHeader != "X-Token" || loaded.Notifications.WebhookAuthToken != "secret" ||
		loaded.Notifications.WebhookTimeoutSeconds != 5 {
		t.Fatalf("notifications not persisted: %+v", loaded.Notifications)
	}
	if len(loaded.RepositoryOverrides) != 1 {
		t.Fatalf("overrides = %+v, want 1 entry", loaded.RepositoryOverrides)
	}
	ov := loaded.RepositoryOverrides[0]
	if ov.RepositoryID != 100 || ov.KeepLocalDays == nil || *ov.KeepLocalDays != keepAll {
		t.Fatalf("override not persisted: %+v", ov)
	}

	// The saved auth token must never be readable back through the API.
	get := request(t, s, http.MethodGet, "/api/retention", nil)
	var view struct {
		Notifications struct {
			Enabled               bool   `json:"enabled"`
			WebhookURL            string `json:"webhookUrl"`
			WebhookAuthConfigured bool   `json:"webhookAuthConfigured"`
		} `json:"notifications"`
		Overrides []overrideView `json:"overrides"`
	}
	if err := json.Unmarshal(get.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if get.Body.String() == "" || !view.Notifications.WebhookAuthConfigured {
		t.Fatalf("expected auth-configured flag; body=%s", get.Body.String())
	}
	if view.Notifications.WebhookURL != "https://hooks.example.com/x" {
		t.Fatalf("webhook URL not returned: %+v", view.Notifications)
	}
	if len(view.Overrides) != 1 || view.Overrides[0].RepositoryID != 100 {
		t.Fatalf("overrides not returned: %+v", view.Overrides)
	}
}

func TestRetentionCleanupHandlerRequiresAuth(t *testing.T) {
	s := newCloudServer(t, &fakeStateStore{}, newFakeTokenStore(), nil)
	req := httptest.NewRequest(http.MethodPost, "/api/retention/cleanup", nil)
	req.RemoteAddr = "127.0.0.1:55555"
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestRunCleanupDeletesEligibleLocalRecords(t *testing.T) {
	s := newCloudServer(t, &fakeStateStore{}, newFakeTokenStore(), nil)
	// 3 local-only records + files; KeepLocal=2 keeps newest 2.
	bundles := []string{v2Bundle("repo", 0), v2Bundle("repo", 1), v2Bundle("repo", 2)}
	for i, b := range bundles {
		writeBundle(t, s, b)
		seedRecord(t, s, retentionRecord("id"+string(rune('a'+i)), "p1", "org/repo", b, i, "", state.BackupStatusBundled))
	}

	s.app.Config.Retention = config.RetentionConfig{KeepLocal: 2}
	res := s.runCleanup(context.Background())
	if len(res.RecordsToRemove) != 1 {
		t.Fatalf("RecordsToRemove = %+v, want 1", res.RecordsToRemove)
	}
	if len(res.LocalDeleted) != 1 {
		t.Fatalf("LocalDeleted = %+v, want 1", res.LocalDeleted)
	}
	// Oldest record is idc (daysOld=2), its bundle removed.
	if _, err := os.Stat(filepath.Join(s.app.OutputPath, bundles[2])); !os.IsNotExist(err) {
		t.Fatalf("oldest bundle should be gone, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(s.app.OutputPath, bundles[0])); err != nil {
		t.Fatalf("newest bundle should remain: %v", err)
	}
	if _, ok := getRecord(s, "idc"); ok {
		t.Fatal("record idc should be removed from store")
	}
	if _, ok := getRecord(s, "ida"); !ok {
		t.Fatal("record ida should remain")
	}
}

func TestRunCleanupRespectsActiveJob(t *testing.T) {
	s := newCloudServer(t, &fakeStateStore{}, newFakeTokenStore(), nil)
	bundle := v2Bundle("repo", 0)
	writeBundle(t, s, bundle)
	seedRecord(t, s, retentionRecord("rec1", "p1", "org/repo", bundle, 100, "", state.BackupStatusBundled))
	seedJob(t, s, state.BackupJob{ID: "job1", ProtectedRepoID: "p1", FullName: "org/repo", State: state.JobCloning, StartedAt: time.Now()})

	s.app.Config.Retention = config.RetentionConfig{KeepLocalDays: 1}
	res := s.runCleanup(context.Background())
	if len(res.RecordsToRemove) != 0 || res.Skipped == 0 {
		t.Fatalf("active repo should be skipped: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(s.app.OutputPath, bundle)); err != nil {
		t.Fatalf("active repo bundle must not be deleted: %v", err)
	}
}

func TestRunCleanupKeepsUploadedRecordOnDriveConfirm(t *testing.T) {
	s := newCloudServer(t, &fakeStateStore{}, newFakeTokenStore(), nil)
	bundle := v2Bundle("repo", 0)
	writeBundle(t, s, bundle)
	seedRecord(t, s, retentionRecord("up1", "p1", "org/repo", bundle, 100, "drive-abc-xyz-abcdefgh", state.BackupStatusUploaded))

	var deleted []string
	s.driveDelete = func(ctx context.Context, fileID string, cfg config.CloudConfig, l *slog.Logger) error {
		deleted = append(deleted, fileID)
		return nil
	}
	s.app.Config.Retention = config.RetentionConfig{DriveRetention: true, KeepDriveDays: 30}

	res := s.runCleanup(context.Background())
	if len(res.DriveDeleted) != 1 || res.DriveDeleted[0] != "drive-abc-xyz-abcdefgh" {
		t.Fatalf("DriveDeleted = %+v", res.DriveDeleted)
	}
	if len(res.RecordsToDowngrade) != 1 {
		t.Fatalf("RecordsToDowngrade = %+v", res.RecordsToDowngrade)
	}
	rec, ok := getRecord(s, "up1")
	if !ok {
		t.Fatal("uploaded record should be kept")
	}
	if rec.DriveFileID != "" || rec.Status != state.BackupStatusBundled {
		t.Fatalf("record not downgraded: %+v", rec)
	}
}

func TestRunCleanupDriveFailureNotReportedDeleted(t *testing.T) {
	s := newCloudServer(t, &fakeStateStore{}, newFakeTokenStore(), nil)
	bundle := v2Bundle("repo", 0)
	writeBundle(t, s, bundle)
	seedRecord(t, s, retentionRecord("up1", "p1", "org/repo", bundle, 100, "drive-fail", state.BackupStatusUploaded))

	s.driveDelete = func(ctx context.Context, fileID string, cfg config.CloudConfig, l *slog.Logger) error {
		return errors.New("provider error")
	}
	s.app.Config.Retention = config.RetentionConfig{DriveRetention: true, KeepDriveDays: 30}

	res := s.runCleanup(context.Background())
	if len(res.DriveDeleted) != 0 || len(res.RecordsToDowngrade) != 0 || res.Skipped == 0 {
		t.Fatalf("failed drive delete should not be reported: %+v", res)
	}
	rec, _ := getRecord(s, "up1")
	if rec.Status != state.BackupStatusUploaded || rec.DriveFileID != "drive-fail" {
		t.Fatalf("record should remain uploaded: %+v", rec)
	}
}

func TestRunCleanupDisabledPolicyDeletesNothing(t *testing.T) {
	s := newCloudServer(t, &fakeStateStore{}, newFakeTokenStore(), nil)
	bundle := v2Bundle("repo", 0)
	writeBundle(t, s, bundle)
	seedRecord(t, s, retentionRecord("rec1", "p1", "org/repo", bundle, 100, "", state.BackupStatusBundled))

	s.app.Config.Retention = config.RetentionConfig{}
	res := s.runCleanup(context.Background())
	if len(res.RecordsToRemove) != 0 || len(res.LocalDeleted) != 0 || res.OrphanDeleted != 0 {
		t.Fatalf("disabled policy must delete nothing: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(s.app.OutputPath, bundle)); err != nil {
		t.Fatalf("bundle must remain under disabled policy: %v", err)
	}
}

func TestRetentionNotifyTestRequiresAuthAndCSRF(t *testing.T) {
	s := newCloudServer(t, &fakeStateStore{}, newFakeTokenStore(), nil)

	// No session → 401.
	req := httptest.NewRequest(http.MethodPost, "/api/retention/notify-test", nil)
	req.RemoteAddr = "127.0.0.1:55555"
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no-session status = %d, want 401", rec.Code)
	}

	// Session but no CSRF → 403.
	cookie, _ := csrfCookie(t, s)
	req = httptest.NewRequest(http.MethodPost, "/api/retention/notify-test", nil)
	req.RemoteAddr = "127.0.0.1:55555"
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("no-csrf status = %d, want 403", rec.Code)
	}
}

func TestRetentionNotifyTestDisabled(t *testing.T) {
	s := newCloudServer(t, &fakeStateStore{}, newFakeTokenStore(), nil)
	s.app.Config.Notifications = config.NotificationConfig{Enabled: false}

	cookie, csrf := csrfCookie(t, s)
	req := httptest.NewRequest(http.MethodPost, "/api/retention/notify-test", nil)
	req.RemoteAddr = "127.0.0.1:55555"
	req.AddCookie(cookie)
	req.Header.Set("X-CSRF-Token", csrf)
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
}

func TestRetentionNotifyTestDelivers(t *testing.T) {
	var (
		mu       sync.Mutex
		received []map[string]any
	)
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		received = append(received, body)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer webhook.Close()

	s := newCloudServer(t, &fakeStateStore{}, newFakeTokenStore(), nil)
	s.app.Config.Notifications = config.NotificationConfig{
		Enabled:         true,
		Type:            "webhook",
		WebhookURL:      webhook.URL,
		NotifyOnSuccess: true,
	}

	cookie, csrf := csrfCookie(t, s)
	req := httptest.NewRequest(http.MethodPost, "/api/retention/notify-test", nil)
	req.RemoteAddr = "127.0.0.1:55555"
	req.AddCookie(cookie)
	req.Header.Set("X-CSRF-Token", csrf)
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 1 {
		t.Fatalf("webhook received %d payloads, want 1", len(received))
	}
	if got := received[0]["trigger"]; got != "test" {
		t.Errorf("trigger = %v, want test", got)
	}
	if got := received[0]["type"]; got != "cleanup" {
		t.Errorf("type = %v, want cleanup", got)
	}
}

func TestRunCleanupAppliesPerRepoOverride(t *testing.T) {
	st := &fakeStateStore{}
	st.protected = []state.ProtectedRepo{
		{ID: "p1", GitHubID: 100, FullName: "org/keepme"},
		{ID: "p2", GitHubID: 200, FullName: "org/deleteable"},
	}
	s := newCloudServer(t, st, newFakeTokenStore(), nil)
	keepBundle := v2Bundle("keepme", 0)
	deleteBundle := v2Bundle("deleteable", 0)
	writeBundle(t, s, keepBundle)
	writeBundle(t, s, deleteBundle)
	seedRecord(t, s, retentionRecord("idkeep", "p1", "org/keepme", keepBundle, 60, "", state.BackupStatusBundled))
	seedRecord(t, s, retentionRecord("iddelete", "p2", "org/deleteable", deleteBundle, 60, "", state.BackupStatusBundled))

	// Global policy deletes local records older than 1 day; a per-repo override
	// keeps everything for the protected repository with GitHub ID 100.
	keepEverything := 999
	s.app.Config.Retention = config.RetentionConfig{KeepLocalDays: 1}
	s.app.Config.RepositoryOverrides = []config.RepositoryRetentionOverride{
		{RepositoryID: 100, KeepLocalDays: &keepEverything},
	}

	res := s.runCleanup(context.Background())
	if len(res.RecordsToRemove) != 1 || res.RecordsToRemove[0] != "iddelete" {
		t.Fatalf("RecordsToRemove = %+v, want [iddelete]", res.RecordsToRemove)
	}
	if _, ok := getRecord(s, "idkeep"); !ok {
		t.Fatal("record under an override should be retained")
	}
	if _, ok := getRecord(s, "iddelete"); ok {
		t.Fatal("record without an override should be removed")
	}
}

func TestRunCleanupConcurrentSerialized(t *testing.T) {
	s := newCloudServer(t, &fakeStateStore{}, newFakeTokenStore(), nil)
	for i := 0; i < 5; i++ {
		b := v2Bundle("repo", i)
		writeBundle(t, s, b)
		seedRecord(t, s, retentionRecord(string(rune('a'+i)), "p1", "org/repo", b, i, "", state.BackupStatusBundled))
	}
	s.app.Config.Retention = config.RetentionConfig{KeepLocal: 2}

	var wg sync.WaitGroup
	var mu sync.Mutex
	ran := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.runCleanup(context.Background())
			mu.Lock()
			ran++
			mu.Unlock()
		}()
	}
	wg.Wait()
	if ran != 8 {
		t.Fatalf("expected 8 cleanup runs, got %d", ran)
	}
	// After serialized runs, remaining store should be consistent.
	if n := len(s.stateStore.BackupRecords()); n < 2 || n > 5 {
		t.Fatalf("unexpected record count after concurrent cleanup: %d", n)
	}
}
