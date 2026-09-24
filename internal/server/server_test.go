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
		TokenStore: func() (TokenStore, error) { return newFakeTokenStore(), nil },
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
// the route no longer serves Settings UI and the nav offers only the cloud
// repositories page. Unknown paths fall through to the app's catch-all "/"
// handler, so /settings must render the same cloud page with no settings form.
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
		if rec.Code != http.StatusOK {
			t.Fatalf("%s /settings status = %d, want 200 (catch-all)", meth, rec.Code)
		}
		if body := rec.Body.String(); !strings.Contains(body, "GitSafe") {
			t.Fatalf("%s /settings did not fall through to the cloud page", meth)
		}
	}
}
