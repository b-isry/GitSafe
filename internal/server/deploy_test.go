package server

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/b-isry/gitsafe/internal/config"
	"github.com/b-isry/gitsafe/internal/state"
	"github.com/b-isry/gitsafe/internal/tokenstore"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newDeployApp(t *testing.T) *App {
	t.Helper()
	cfg := config.Defaults()
	cfg.OutputPath = t.TempDir()
	cfg.Cloud = config.CloudConfig{Enabled: false}
	return &App{Config: cfg, OutputPath: cfg.OutputPath}
}

func TestIsLoopbackHost(t *testing.T) {
	cases := []struct {
		url  string
		want bool
	}{
		{"http://127.0.0.1:8080", true},
		{"http://localhost:8080", true},
		{"https://localhost", true},
		{"http://[::1]:8080", true},
		{"http://127.0.0.2:80", true},
		{"http://192.168.1.10", false},
		{"https://gitsafe.onrender.com", false},
		{"https://example.com", false},
		{"not a url ://", false},
		{"", false},
	}
	for _, c := range cases {
		if got := IsLoopbackHost(c.url); got != c.want {
			t.Errorf("IsLoopbackHost(%q) = %v, want %v", c.url, got, c.want)
		}
	}
}

// TestTokenStoreFactoryFromEnvFileStore verifies that a set GITSAFE_TOKEN_KEY
// selects the encrypted file store rooted at GITSAFE_DATA_DIR, and that the
// selected backend actually persists a token to disk.
func TestTokenStoreFactoryFromEnvFileStore(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tokens")
	t.Setenv(TokenKeyEnv, "deployment-key")
	t.Setenv(TokenDataDirEnv, dir)

	factory, err := tokenStoreFactoryFromEnv(nil, t.TempDir())
	if err != nil {
		t.Fatalf("tokenStoreFactoryFromEnv: %v", err)
	}
	st, err := factory(42)
	if err != nil {
		t.Fatalf("factory(): %v", err)
	}
	if err := st.Set("github.42", "tok"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := st.Get("github.42")
	if err != nil || got != "tok" {
		t.Fatalf("Get = %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "github.42.json")); err != nil {
		t.Fatalf("token file not written to data dir: %v", err)
	}
}

// TestTokenStoreFactoryFromEnvKeyringWhenNoKey verifies that without a
// GITSAFE_TOKEN_KEY the OS keyring is selected (local development). Constructing
// a KeyringStore touches nothing on the OS; only Set/Get/Delete would.
func TestTokenStoreFactoryFromEnvKeyringWhenNoKey(t *testing.T) {
	t.Setenv(TokenKeyEnv, "")
	t.Setenv(TokenDataDirEnv, "")
	factory, err := tokenStoreFactoryFromEnv(nil, t.TempDir())
	if err != nil {
		t.Fatalf("tokenStoreFactoryFromEnv: %v", err)
	}
	st, err := factory(42)
	if err != nil {
		t.Fatalf("factory(): %v", err)
	}
	if _, ok := st.(*tokenstore.KeyringStore); !ok {
		t.Fatalf("no key must select the OS keyring, got %T", st)
	}
}

func TestNewUsesFileStoresWhenDatabaseURLUnset(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tokens")
	t.Setenv(DatabaseURLEnv, "")
	t.Setenv(TokenKeyEnv, "deployment-key")
	t.Setenv(TokenDataDirEnv, dir)

	s, err := New(discardLogger(), newDeployApp(t), filepath.Join(t.TempDir(), "config.yaml"))
	if err != nil {
		t.Fatalf("New with file-store env: %v", err)
	}
	st, err := s.userStores.tokenFactory(7)
	if err != nil {
		t.Fatalf("tokenFactory(): %v", err)
	}
	if _, ok := st.(*tokenstore.FileStore); !ok {
		t.Fatalf("New must select the file store when key set, got %T", st)
	}
	stateStore, err := s.userStores.stateFactory(7)
	if err != nil {
		t.Fatalf("stateFactory(): %v", err)
	}
	if _, ok := stateStore.(*state.Store); !ok {
		t.Fatalf("New must select the file state store without DATABASE_URL, got %T", stateStore)
	}
	if err := st.Set("github.7", "persisted"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "github.7.json")); err != nil {
		t.Fatalf("token file not written: %v", err)
	}
}

// errTokenStore models a backend whose Set always fails (for example a headless
// container whose keyring backend cannot spawn dbus-launch).
type errTokenStore struct{}

func (errTokenStore) Set(ref, value string) error { return errors.New("backend boom") }
func (errTokenStore) Get(ref string) (string, error) {
	return "", tokenstore.ErrNotFound
}
func (errTokenStore) Delete(ref string) error { return nil }

// droppingTokenStore models a backend that accepts writes but never returns
// them: the probe must catch it too, since Get failing at login time is the
// exact production failure mode being guarded against.
type droppingTokenStore struct{}

func (droppingTokenStore) Set(ref, value string) error { return nil }
func (droppingTokenStore) Get(ref string) (string, error) {
	return "", tokenstore.ErrNotFound
}
func (droppingTokenStore) Delete(ref string) error { return nil }

// TestNewRefusesBootWhenProbeFails is the required proof that server.New
// returns an error when the startup Set/Get/Delete self-test fails for ANY
// reason, not only a missing key: a backend whose Set always errors must
// refuse the boot rather than starting and failing at the first login.
func TestNewRefusesBootWhenProbeFails(t *testing.T) {
	cases := []struct {
		name    string
		factory func(int64) (TokenStore, error)
	}{
		{"set always fails", func(int64) (TokenStore, error) { return errTokenStore{}, nil }},
		{"get always fails", func(int64) (TokenStore, error) { return droppingTokenStore{}, nil }},
		{"factory errors", func(int64) (TokenStore, error) { return nil, errors.New("no backend") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewWithOptions(discardLogger(), newDeployApp(t), filepath.Join(t.TempDir(), "config.yaml"), Options{
				TokenStore: tc.factory,
			})
			if err == nil {
				t.Fatalf("NewWithOptions must refuse to boot when probe fails (%s)", tc.name)
			}
			if !strings.Contains(err.Error(), "token store self-test") {
				t.Fatalf("error must cite the self-test, got: %v", err)
			}
		})
	}
}

// TestNewRefusesBootOnSetFailurePinsMessage additionally pins that a backend
// whose Set fails surfaces the wrapped backend error, so operators can tell
// the store was unreachable (not merely misconfigured).
func TestNewRefusesBootOnSetFailurePinsMessage(t *testing.T) {
	_, err := NewWithOptions(discardLogger(), newDeployApp(t), filepath.Join(t.TempDir(), "config.yaml"), Options{
		TokenStore: func(int64) (TokenStore, error) { return errTokenStore{}, nil },
	})
	if err == nil {
		t.Fatal("expected boot refusal")
	}
	if got := err.Error(); !strings.Contains(got, "backend boom") {
		t.Fatalf("unexpected probe error: %v", got)
	}
}

func TestNewRefusesProductionWithoutDatabaseURL(t *testing.T) {
	app := newDeployApp(t)
	app.Config.BaseURL = "https://gitsafe.example.com"
	t.Setenv(DatabaseURLEnv, "")
	t.Setenv(TokenKeyEnv, "deployment-key")

	_, err := NewWithOptions(discardLogger(), app, filepath.Join(t.TempDir(), "config.yaml"), Options{
		TokenStore: func(int64) (TokenStore, error) { return newFakeTokenStore(), nil },
		StateStore: func(int64) (StateStore, error) { return &fakeStateStore{}, nil },
	})
	if err == nil || !strings.Contains(err.Error(), DatabaseURLEnv) {
		t.Fatalf("production boot without DATABASE_URL must fail clearly, got %v", err)
	}
}

func TestNewRefusesProductionWithoutTokenKeyOrDatabaseURL(t *testing.T) {
	app := newDeployApp(t)
	app.Config.BaseURL = "https://gitsafe.example.com"
	t.Setenv(DatabaseURLEnv, "")
	t.Setenv(TokenKeyEnv, "")

	_, err := NewWithOptions(discardLogger(), app, filepath.Join(t.TempDir(), "config.yaml"), Options{
		TokenStore: func(int64) (TokenStore, error) { return newFakeTokenStore(), nil },
		StateStore: func(int64) (StateStore, error) { return &fakeStateStore{}, nil },
	})
	if err == nil {
		t.Fatal("production boot must fail")
	}
	for _, env := range []string{DatabaseURLEnv, TokenKeyEnv} {
		if !strings.Contains(err.Error(), env) {
			t.Fatalf("boot error %q does not name %s", err, env)
		}
	}
}

func TestNewRequiresTokenKeyForPostgresOnLoopback(t *testing.T) {
	t.Setenv(DatabaseURLEnv, "postgresql://example.invalid/gitsafe")
	t.Setenv(TokenKeyEnv, "")

	_, err := New(discardLogger(), newDeployApp(t), filepath.Join(t.TempDir(), "config.yaml"))
	if err == nil || !strings.Contains(err.Error(), TokenKeyEnv) {
		t.Fatalf("PostgreSQL boot without token key must fail clearly, got %v", err)
	}
}

func TestNewRefusesBootWhenDatabaseUnreachable(t *testing.T) {
	t.Setenv(DatabaseURLEnv, "postgresql://postgres:postgres@127.0.0.1:1/gitsafe?connect_timeout=1")
	t.Setenv(TokenKeyEnv, "deployment-key")

	_, err := New(discardLogger(), newDeployApp(t), filepath.Join(t.TempDir(), "config.yaml"))
	if err == nil || !strings.Contains(err.Error(), "database unreachable") {
		t.Fatalf("unreachable database must prevent boot, got %v", err)
	}
}
