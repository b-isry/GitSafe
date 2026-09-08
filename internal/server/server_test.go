package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/b-isry/gitsafe/internal/backup"
	"github.com/b-isry/gitsafe/internal/config"
)

func newTestApp(t *testing.T, root, out string) *Server {
	t.Helper()
	return newTestAppStore(t, root, out, nil)
}

// newTestAppStore is like newTestApp but accepts a BackupStore so tests can
// seed the store (e.g. with an already-running job) for dedupe behavior.
func newTestAppStore(t *testing.T, root, out string, store BackupStore) *Server {
	t.Helper()
	_ = os.MkdirAll(out, 0o755)
	cfg := config.Defaults()
	cfg.RootPath = root
	cfg.OutputPath = out
	cfg.BackupHistory = true
	cfg.Cloud = config.CloudConfig{Enabled: false}

	app := &App{
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config:     cfg,
		Runner:     backup.New(slog.New(slog.NewTextHandler(io.Discard, nil))),
		OutputPath: out,
		Threshold:  cfg.Days,
	}
	if store == nil {
		store = NewBackupStore(slog.New(slog.NewTextHandler(io.Discard, nil)))
	}
	s, err := New(slog.New(slog.NewTextHandler(io.Discard, nil)), app, store, filepath.Join(t.TempDir(), "config.yaml"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.app.Repos(context.Background()) // force initial scan
	return s
}

// newTestAppUnconfigured builds a server that has not been set up yet: the root
// path is empty and no config file exists, matching the first-run experience.
func newTestAppUnconfigured(t *testing.T) *Server {
	t.Helper()
	cfg := config.Defaults()
	cfg.RootPath = ""

	app := &App{
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config:     cfg,
		Runner:     backup.New(slog.New(slog.NewTextHandler(io.Discard, nil))),
		OutputPath: cfg.OutputPath,
		Threshold:  cfg.Days,
	}
	store := NewBackupStore(slog.New(slog.NewTextHandler(io.Discard, nil)))
	s, err := New(slog.New(slog.NewTextHandler(io.Discard, nil)), app, store, filepath.Join(t.TempDir(), "config.yaml"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func TestFirstRunRedirectsToSetup(t *testing.T) {
	s := newTestAppUnconfigured(t)

	for _, path := range []string{"/", "/repositories", "/settings", "/backups"} {
		req := httptest.NewRequest("GET", path, nil)
		rec := httptest.NewRecorder()
		s.Routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusFound {
			t.Errorf("%s: expected 302, got %d", path, rec.Code)
			continue
		}
		if loc := rec.Header().Get("Location"); loc != "/setup" {
			t.Errorf("%s: expected redirect to /setup, got %q", path, loc)
		}
	}
}

func TestFirstRunSetupPage(t *testing.T) {
	s := newTestAppUnconfigured(t)

	req := httptest.NewRequest("GET", "/setup", nil)
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"Choose a folder", "Start scanning", "setupPath"} {
		if !strings.Contains(body, want) {
			t.Errorf("setup page missing %q", want)
		}
	}
}

func TestFirstRunSubmitSuccess(t *testing.T) {
	root := t.TempDir()
	createRepo(t, root, "web-app")
	s := newTestAppUnconfigured(t)
	cfgPath := s.configPath

	rec, resp := doJSON(t, s, http.MethodPost, "/setup", map[string]string{"rootPath": root})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if ok, _ := resp["ok"].(bool); !ok {
		t.Fatalf("expected ok=true, got %v", resp)
	}
	if redirect, _ := resp["redirect"].(string); redirect != "/" {
		t.Fatalf("expected redirect to /, got %q", redirect)
	}

	// Config persisted and applied to the running app.
	loaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("reload written config: %v", err)
	}
	if loaded.RootPath != root {
		t.Errorf("persisted rootPath mismatch: %q", loaded.RootPath)
	}
	if s.app.Config.RootPath != root {
		t.Errorf("app root not applied: %q", s.app.Config.RootPath)
	}

	// The gate now lets pages through and the app has scanned the chosen root.
	req := httptest.NewRequest("GET", "/", nil)
	rec2 := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec2, req)
	if rec2.Code != http.StatusOK {
		t.Fatalf("expected 200 after setup, got %d", rec2.Code)
	}
}

func TestFirstRunSubmitInvalidDir(t *testing.T) {
	root := t.TempDir()
	s := newTestAppUnconfigured(t)
	cfgPath := s.configPath

	rec, resp := doJSON(t, s, http.MethodPost, "/setup", map[string]string{"rootPath": filepath.Join(root, "does-not-exist")})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rec.Code, rec.Body.String())
	}
	if ok, _ := resp["ok"].(bool); ok {
		t.Fatalf("expected ok=false, got %v", resp)
	}
	// No mutation, no file written.
	if s.app.Config.RootPath != "" {
		t.Errorf("config mutated on failed setup: %q", s.app.Config.RootPath)
	}
	if _, err := os.Stat(cfgPath); err == nil {
		t.Errorf("config file should not exist after failed setup")
	}
}

func TestFirstRunSubmitEmptyPath(t *testing.T) {
	s := newTestAppUnconfigured(t)

	rec, _ := doJSON(t, s, http.MethodPost, "/setup", map[string]string{"rootPath": ""})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestConfiguredAppSkipsSetupGate(t *testing.T) {
	root := t.TempDir()
	createRepo(t, root, "web-app")
	s := newTestApp(t, root, filepath.Join(t.TempDir(), "backups"))

	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "" {
		t.Errorf("unexpected redirect: %q", loc)
	}
}

func doJSON(t *testing.T, s *Server, method, path string, body any) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, rd)
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	var m map[string]any
	if rec.Code >= 200 && rec.Code < 300 && rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &m)
	}
	return rec, m
}

func TestRepositoryDetailPage(t *testing.T) {
	root := t.TempDir()
	out := filepath.Join(t.TempDir(), "backups")
	repoDir := createRepo(t, root, "web-app")

	s := newTestApp(t, root, out)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/repositories/"+pathID(repoDir), nil)
	s.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	for _, want := range []string{"Create backup", "web-app", "Staleness", "Backup history"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("detail page missing %q", want)
		}
	}
}

func TestBackupFlowEndToEnd(t *testing.T) {
	root := t.TempDir()
	out := filepath.Join(t.TempDir(), "backups")
	repoDir := createRepo(t, root, "web-app")

	s := newTestApp(t, root, out)

	rec, resp := doJSON(t, s, http.MethodPost, "/backups", map[string]string{"repoPath": repoDir})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}
	id, _ := resp["id"].(string)
	if id == "" {
		t.Fatalf("expected a job id, got %v", resp)
	}

	// Poll until the job reaches a terminal state.
	deadline := time.Now().Add(15 * time.Second)
	var final map[string]any
	for time.Now().Before(deadline) {
		req := httptest.NewRequest("GET", "/backups/"+id, nil)
		rec2 := httptest.NewRecorder()
		s.Routes().ServeHTTP(rec2, req)
		if rec2.Code != http.StatusOK {
			t.Fatalf("expected 200 on poll, got %d", rec2.Code)
		}
		_ = json.Unmarshal(rec2.Body.Bytes(), &final)
		if state, _ := final["state"].(string); state == "completed" || state == "failed" {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

	state, _ := final["state"].(string)
	if state != "completed" {
		t.Fatalf("expected completed, got %q (job: %v)", state, final)
	}

	bundle, _ := final["bundle"].(map[string]any)
	if bundle == nil || bundle["bundlePath"] == nil || bundle["bundlePath"].(string) == "" {
		t.Fatalf("expected a bundle path in completed job")
	}
	if _, err := os.Stat(bundle["bundlePath"].(string)); err != nil {
		t.Fatalf("bundle file missing on disk: %v", err)
	}
	if bundle["bundleSize"].(float64) == 0 {
		t.Fatalf("expected non-zero bundle size")
	}

	// The terminal result must be reached with the store holding the job.
	if _, ok := s.store.Get(id); !ok {
		t.Fatalf("job missing from store")
	}
}

func TestBackupUnknownRepo(t *testing.T) {
	root := t.TempDir()
	out := filepath.Join(t.TempDir(), "backups")
	s := newTestApp(t, root, out)

	rec, _ := doJSON(t, s, http.MethodPost, "/backups", map[string]string{"repoPath": filepath.Join(root, "missing")})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected accepted (async), got %d", rec.Code)
	}
}

func TestSettingsPageShowsValuesWithoutCredentials(t *testing.T) {
	root := t.TempDir()
	out := filepath.Join(t.TempDir(), "backups")
	s := newTestApp(t, root, out)

	// Place a credentials file with sensitive content on disk and point the
	// config at it so the settings page resolves it as "found".
	creds := filepath.Join(root, "creds.json")
	if err := os.WriteFile(creds, []byte(`{"type":"service_account","private_key":"SECRET-KEY-MATERIAL"}`), 0o600); err != nil {
		t.Fatalf("write creds: %v", err)
	}
	s.app.Config.Cloud = config.CloudConfig{Enabled: true, CredentialsFile: creds}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/settings", nil)
	s.Routes().ServeHTTP(rec, req)

	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	for _, want := range []string{"rootPath", "days", "outputPath", "backupHistory", "cloudEnabled", "credentialsFile"} {
		if strings.Contains(body, `id="`+want+`"`) == false {
			t.Errorf("settings page missing field %q", want)
		}
	}
	if !strings.Contains(body, creds) {
		t.Errorf("settings page should show the credentials file path, got:\n%s", body)
	}
	if strings.Contains(body, "SECRET-KEY-MATERIAL") || strings.Contains(body, "service_account") {
		t.Errorf("settings page must not expose raw credential contents")
	}
}

func TestSaveSettingsValid(t *testing.T) {
	root := t.TempDir()
	out := filepath.Join(t.TempDir(), "backups")
	s := newTestApp(t, root, out)

	cfgPath := s.configPath
	newRoot := t.TempDir()
	payload, _ := json.Marshal(map[string]any{
		"rootPath": newRoot, "days": 14, "outputPath": out,
		"backupHistory": false, "cloudEnabled": true, "credentialsFile": "creds.json",
	})
	resp := map[string]any{}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/settings", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	s.Routes().ServeHTTP(rec, req)
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if ok, _ := resp["ok"].(bool); !ok {
		t.Fatalf("expected ok=true, got %v", resp)
	}

	if s.app.Config.RootPath != newRoot {
		t.Errorf("expected app root to be updated, got %q", s.app.Config.RootPath)
	}
	if s.app.Config.Days != 14 {
		t.Errorf("expected days 14, got %d", s.app.Config.Days)
	}
	if s.app.Config.BackupHistory {
		t.Errorf("expected backupHistory false")
	}
	if !s.app.Config.Cloud.Enabled {
		t.Errorf("expected cloud enabled")
	}
	if s.app.OutputPath != out || s.app.Threshold != 14 {
		t.Errorf("derived fields not applied: out=%q threshold=%d", s.app.OutputPath, s.app.Threshold)
	}

	// Persisted to the config file and re-readable.
	if _, err := os.Stat(cfgPath); err != nil {
		t.Fatalf("config file not written: %v", err)
	}
	loaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("reload written config: %v", err)
	}
	if loaded.RootPath != newRoot || loaded.Days != 14 {
		t.Errorf("persisted config mismatch: %+v", loaded)
	}
}

func TestSaveSettingsInvalidRootPath(t *testing.T) {
	root := t.TempDir()
	out := filepath.Join(t.TempDir(), "backups")
	s := newTestApp(t, root, out)

	invalidRoot := filepath.Join(root, "does-not-exist")
	payload, _ := json.Marshal(map[string]any{
		"rootPath": invalidRoot, "days": 60, "outputPath": out,
		"backupHistory": true, "cloudEnabled": false, "credentialsFile": "creds.json",
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/settings", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	s.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	errs, _ := resp["errors"].(map[string]any)
	if _, ok := errs["rootPath"]; !ok {
		t.Errorf("expected a rootPath field error, got %v", resp)
	}
	// No change should have been applied.
	if s.app.Config.RootPath != root {
		t.Errorf("config mutated on failed save: %q", s.app.Config.RootPath)
	}
}

func TestSaveSettingsCloudRequiresCredentials(t *testing.T) {
	root := t.TempDir()
	out := filepath.Join(t.TempDir(), "backups")
	s := newTestApp(t, root, out)

	payload, _ := json.Marshal(map[string]any{
		"rootPath": root, "days": 60, "outputPath": out,
		"backupHistory": true, "cloudEnabled": true, "credentialsFile": "",
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/settings", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	s.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	errs, _ := resp["errors"].(map[string]any)
	if _, ok := errs["credentialsFile"]; !ok {
		t.Errorf("expected a credentialsFile field error, got %v", resp)
	}
}

func TestSaveSettingsInvalidJSON(t *testing.T) {
	root := t.TempDir()
	out := filepath.Join(t.TempDir(), "backups")
	s := newTestApp(t, root, out)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/settings", bytes.NewBufferString("not-json"))
	req.Header.Set("Content-Type", "application/json")
	s.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestSaveSettingsOversizedBodyRejected(t *testing.T) {
	root := t.TempDir()
	out := filepath.Join(t.TempDir(), "backups")
	s := newTestApp(t, root, out)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/settings", bytes.NewReader(make([]byte, maxRequestBody+1)))
	req.Header.Set("Content-Type", "application/json")
	s.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for oversized body, got %d", rec.Code)
	}
}

func TestListFoldersUnconfiguredReachable(t *testing.T) {
	// The folder browser is used during first-run setup, before a root is
	// configured. Its API must not be redirected to /setup by the root gate.
	s := newTestAppUnconfigured(t)
	req := httptest.NewRequest("GET", "/api/folders", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 while unconfigured, got %d", rec.Code)
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.Contains(ct, "application/json") {
		t.Errorf("expected JSON, got %q", ct)
	}
}

func TestListFolders(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "apps")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	out := filepath.Join(t.TempDir(), "backups")
	s := newTestApp(t, root, out)

	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/api/folders?path="+url.QueryEscape(path), nil)
		req.RemoteAddr = "127.0.0.1:1234"
		rec := httptest.NewRecorder()
		s.Routes().ServeHTTP(rec, req)
		return rec
	}

	rec := get(root)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var res foldersResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res.Path != root {
		t.Errorf("path mismatch: %q", res.Path)
	}
	if len(res.Entries) == 0 {
		t.Fatalf("expected entries for %q, got none", root)
	}
	found := false
	for _, e := range res.Entries {
		if e.Name == "notes.txt" {
			t.Errorf("must not list files, got %q", e.Name)
		}
		if e.Name == "apps" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected an entry for %q, got %v", "apps", res.Entries)
	}

	// A missing folder surfaces an error but stays 200 with no entries.
	rec = get(filepath.Join(root, "missing"))
	var missing foldersResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &missing)
	if missing.ErrorMsg == "" {
		t.Errorf("expected error for missing folder")
	}
	if len(missing.Entries) != 0 {
		t.Errorf("expected no entries for missing folder, got %v", missing.Entries)
	}

	// Top level offers drives on Windows, "/" elsewhere.
	rec = get("")
	var top foldersResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &top)
	if !top.IsTop {
		t.Errorf("expected isTop at the top level")
	}
	if len(top.Entries) == 0 {
		t.Errorf("expected entries at the top level")
	}

	// Non-loopback clients are forbidden.
	req := httptest.NewRequest("GET", "/api/folders?path="+url.QueryEscape(root), nil)
	req.RemoteAddr = "198.51.100.1:1234"
	rec2 := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec2, req)
	if rec2.Code != http.StatusForbidden {
		t.Errorf("expected 403 for remote client, got %d", rec2.Code)
	}
}

func TestScanStatusAndScanNow(t *testing.T) {
	root := t.TempDir()
	createRepo(t, root, "web-app")
	out := filepath.Join(t.TempDir(), "backups")
	s := newTestApp(t, root, out)

	req := httptest.NewRequest("GET", "/api/status", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var st map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &st)
	if rootPath, _ := st["rootPath"].(string); rootPath != root {
		t.Errorf("rootPath mismatch: %q", rootPath)
	}
	if _, ok := st["scanning"].(bool); !ok {
		t.Errorf("expected a boolean scanning, got %v", st["scanning"])
	}
	if ls, _ := st["lastScan"].(string); ls == "" {
		t.Errorf("expected lastScan after the forced scan")
	}
	if _, ok := st["skipped"]; !ok {
		t.Errorf("expected a skipped field")
	}

	// Scan now is reachable and reports a scanning state (already false for a
	// tiny root: the coalesced scan completes before the response is written).
	req2 := httptest.NewRequest("POST", "/api/scan", nil)
	req2.RemoteAddr = "127.0.0.1:1234"
	rec2 := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec2.Code)
	}
	var res map[string]any
	_ = json.Unmarshal(rec2.Body.Bytes(), &res)
	if _, ok := res["scanning"].(bool); !ok {
		t.Errorf("expected a boolean scanning, got %v", res)
	}

	// Non-loopback clients are forbidden.
	req3 := httptest.NewRequest("GET", "/api/status", nil)
	req3.RemoteAddr = "198.51.100.1:1234"
	rec3 := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusForbidden {
		t.Errorf("expected 403 for remote client, got %d", rec3.Code)
	}
}

func TestBatchBackupDedupeDeterministic(t *testing.T) {
	root := t.TempDir()
	out := filepath.Join(t.TempDir(), "backups")
	repoDir := createRepo(t, root, "web-app")

	// Seed the store with an active (idle) job for the same repository so the
	// dedupe path is exercised deterministically, without racing the runner.
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := NewBackupStore(logger)
	store.Create("existing", repoDir)
	s := newTestAppStore(t, root, out, store)

	// Single-path request for an already-running repo → 409.
	body, _ := json.Marshal(map[string]string{"repoPath": repoDir})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/backups", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	s.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", rec.Code, rec.Body.String())
	}
	var errBody map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &errBody); err != nil {
		t.Fatalf("decode 409 body: %v", err)
	}
	if errBody["error"] == "" {
		t.Errorf("expected an error message, got %v", errBody)
	}

	// Batch request mixing the running repo, a duplicate, blanks, and a new one.
	rec2, resp2 := doJSON(t, s, http.MethodPost, "/backups", map[string]any{
		"repoPaths": []string{repoDir, repoDir, "", "   ", filepath.Join(root, "other")},
	})
	if rec2.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec2.Code, rec2.Body.String())
	}
	jobs, _ := resp2["jobs"].([]any)
	already, _ := resp2["alreadyRunning"].([]any)
	if len(jobs) != 1 {
		t.Fatalf("expected exactly one new job, got %d (jobs=%v already=%v)", len(jobs), jobs, already)
	}
	if len(already) != 1 || already[0] != repoDir {
		t.Errorf("expected %q already running, got %v", repoDir, already)
	}
	job, _ := jobs[0].(map[string]any)
	if id, _ := job["id"].(string); id == "" || id == "existing" {
		t.Errorf("unexpected job id %q", id)
	}
	if jp, _ := job["repoPath"].(string); jp != filepath.Join(root, "other") {
		t.Errorf("unexpected job repo path %q", jp)
	}
}

func TestBatchBackupDuplicatesCollapsed(t *testing.T) {
	root := t.TempDir()
	out := filepath.Join(t.TempDir(), "backups")
	repoDir := createRepo(t, root, "web-app")
	s := newTestApp(t, root, out)

	rec, resp := doJSON(t, s, http.MethodPost, "/backups", map[string]any{
		"repoPaths": []string{repoDir, repoDir, "", "  "},
	})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}
	jobs, _ := resp["jobs"].([]any)
	already, _ := resp["alreadyRunning"].([]any)
	if len(jobs) != 1 {
		t.Fatalf("expected exactly one job from duplicate paths, got %d", len(jobs))
	}
	if len(already) != 0 {
		t.Errorf("expected no already-running entries, got %v", already)
	}
}

func createRepo(t *testing.T, root, name string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "t@example.com")
	runGit(t, dir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "init")
	return dir
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v (%s)", args, err, string(b))
	}
}
