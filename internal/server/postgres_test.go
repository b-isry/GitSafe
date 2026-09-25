//go:build postgres

package server

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/b-isry/gitsafe/internal/state"
	"github.com/b-isry/gitsafe/internal/tokenstore"
)

func postgresTestURL(t *testing.T) string {
	t.Helper()
	databaseURL := os.Getenv("GITSAFE_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("PostgreSQL integration test requires GITSAFE_TEST_DATABASE_URL and a running PostgreSQL server")
	}
	return databaseURL
}

func TestNewSelectsPostgresStoresFromDatabaseURL(t *testing.T) {
	databaseURL := postgresTestURL(t)
	t.Setenv(DatabaseURLEnv, databaseURL)
	t.Setenv(TokenKeyEnv, "deployment-key")
	userID := time.Now().UnixNano()

	s, err := New(discardLogger(), newDeployApp(t), filepath.Join(t.TempDir(), "config.yaml"))
	if err != nil {
		t.Fatalf("New with PostgreSQL: %v", err)
	}
	t.Cleanup(func() {
		_, _ = s.db.Exec(`DELETE FROM user_state WHERE user_id = $1`, strconv.FormatInt(userID, 10))
		_, _ = s.db.Exec(`DELETE FROM user_tokens WHERE user_id = $1`, strconv.FormatInt(userID, 10))
		if err := s.Close(); err != nil {
			t.Errorf("close server database: %v", err)
		}
	})

	stores, err := s.userStores.getOrCreate(userID)
	if err != nil {
		t.Fatalf("getOrCreate: %v", err)
	}
	if _, ok := stores.token.(*tokenstore.PostgresTokenStore); !ok {
		t.Fatalf("DATABASE_URL token backend = %T, want *tokenstore.PostgresTokenStore", stores.token)
	}
	if _, ok := stores.state.(*state.PostgresStore); !ok {
		t.Fatalf("DATABASE_URL state backend = %T, want *state.PostgresStore", stores.state)
	}
}

func TestNewPostgresStateProbeFailureRefusesBoot(t *testing.T) {
	t.Setenv(DatabaseURLEnv, postgresTestURL(t))
	t.Setenv(TokenKeyEnv, "deployment-key")
	badState := &fakeStateStore{errors: map[string]error{"save": errors.New("state write failed")}}

	_, err := NewWithOptions(discardLogger(), newDeployApp(t), filepath.Join(t.TempDir(), "config.yaml"), Options{
		TokenStore: func(int64) (TokenStore, error) { return newFakeTokenStore(), nil },
		StateStore: func(int64) (StateStore, error) { return badState, nil },
	})
	if err == nil || !strings.Contains(err.Error(), "state store self-test") || !strings.Contains(err.Error(), "state write failed") {
		t.Fatalf("failed PostgreSQL state probe must refuse boot, got %v", err)
	}
}
