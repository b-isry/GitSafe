package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/b-isry/gitsafe/internal/config"
)

// newTestApp builds a Server without any GitHub/cloud wiring: a fresh,
// non-configured install. root is retained for call-site compatibility (the
// app is GitHub-only and no longer scans local folders); out is the bundle
// output directory.
func newTestApp(t *testing.T, root, out string) *Server {
	t.Helper()
	_ = os.MkdirAll(out, 0o755)
	cfg := config.Defaults()
	cfg.OutputPath = out
	cfg.Cloud = config.CloudConfig{Enabled: false}

	app := &App{
		Config:     cfg,
		OutputPath: out,
	}
	// Inject an in-memory token backend so the construction self-test and any
	// getOrCreate fall-through never touch a live OS keychain during tests.
	s, err := NewWithOptions(slog.New(slog.NewTextHandler(io.Discard, nil)), app, filepath.Join(t.TempDir(), "config.yaml"), Options{
		TokenStore: func(int64) (TokenStore, error) { return newFakeTokenStore(), nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func TestHomePageIsCloudRepositories(t *testing.T) {
	s := newTestApp(t, t.TempDir(), filepath.Join(t.TempDir(), "backups"))

	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"GitSafe", "Back up your repositories", "Connect with GitHub"} {
		if !strings.Contains(body, want) {
			t.Errorf("home page missing %q", want)
		}
	}
	// The end user must never see developer/operator configuration.
	for _, forbidden := range []string{"GITSAFE_GITHUB_CLIENT_SECRET", "GITSAFE_GITHUB_CLIENT_ID", "client id", "client secret", "environment"} {
		if strings.Contains(strings.ToLower(body), strings.ToLower(forbidden)) {
			t.Errorf("home page must not expose %q", forbidden)
		}
	}
}

// TestHomePageHasNoSettingsRoute verifies the Settings page is fully removed:
// the nav offers only the cloud repositories page and /settings is a real 404
// (unknown paths no longer fall through to the landing page).
func TestHomePageHasNoSettingsRoute(t *testing.T) {
	s := newTestApp(t, t.TempDir(), filepath.Join(t.TempDir(), "backups"))

	home := request(t, s, http.MethodGet, "/", nil).Body.String()
	for _, quot := range []string{"Settings", `id="settingsDriveDisconnectBtn"`, `href="/settings"`} {
		if strings.Contains(home, quot) {
			t.Errorf("home page still references the Settings page (%q)", quot)
		}
	}
	for _, meth := range []string{http.MethodGet, http.MethodPost} {
		rec := request(t, s, meth, "/settings", nil)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s /settings status = %d, want 404", meth, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "Connect with GitHub") {
			t.Fatalf("%s /settings must not render the landing page", meth)
		}
	}
}

func TestUnknownPathsAre404(t *testing.T) {
	s := newTestApp(t, t.TempDir(), filepath.Join(t.TempDir(), "backups"))
	for _, p := range []string{"/nope", "/api/nope", "/healthzz"} {
		if rec := request(t, s, http.MethodGet, p, nil); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", p, rec.Code)
		}
	}
}

func TestHealthz(t *testing.T) {
	s := newTestApp(t, t.TempDir(), filepath.Join(t.TempDir(), "backups"))
	for _, m := range []string{http.MethodGet, http.MethodHead} {
		rec := request(t, s, m, "/healthz", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s /healthz = %d", m, rec.Code)
		}
		if m == http.MethodGet && !strings.Contains(rec.Body.String(), `"ok"`) {
			t.Fatalf("healthz body = %q", rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "<html") {
			t.Fatal("healthz must not return the landing page")
		}
	}
}

func TestPrivacyAndTermsRender(t *testing.T) {
	s := newTestApp(t, t.TempDir(), filepath.Join(t.TempDir(), "backups"))
	for path, want := range map[string]string{"/privacy": "<h1>Privacy</h1>", "/terms": "<h1>Terms</h1>"} {
		rec := request(t, s, http.MethodGet, path, nil)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("GET %s = %d %q", path, rec.Code, rec.Body.String())
		}
	}
}

func TestLoopbackOnlyEnforcedForLocalInstalls(t *testing.T) {
	s := newTestApp(t, t.TempDir(), filepath.Join(t.TempDir(), "backups"))
	req := httptest.NewRequest(http.MethodGet, "/api/csrf", nil)
	req.RemoteAddr = "10.1.2.3:4444"
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("local install: non-loopback client got %d, want 403", rec.Code)
	}
	// A public deployment sits behind a proxy: the connecting address is the
	// proxy, so the loopback restriction must not apply.
	s.localOnly = false
	rec = httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("production: proxied client got %d, want 200", rec.Code)
	}
}
