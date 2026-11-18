//go:build postgres

package state

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strconv"
	"testing"
	"time"

	postgresdb "github.com/b-isry/gitsafe/internal/postgres"
)

func openPostgresStateTest(t *testing.T) *sql.DB {
	t.Helper()
	databaseURL := os.Getenv("GITSAFE_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("PostgreSQL integration test requires GITSAFE_TEST_DATABASE_URL and a running PostgreSQL server")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	db, err := postgresdb.Open(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open PostgreSQL: %v", err)
	}
	if err := postgresdb.EnsureSchema(ctx, db); err != nil {
		_ = db.Close()
		t.Fatalf("create PostgreSQL schema: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestPostgresStateStoreRoundTripAndIsolation(t *testing.T) {
	db := openPostgresStateTest(t)
	userANumber := time.Now().UnixNano()
	userBNumber := userANumber + 1
	userA := strconv.FormatInt(userANumber, 10)
	userB := strconv.FormatInt(userBNumber, 10)
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM user_state WHERE user_id = $1 OR user_id = $2`, userA, userB)
	})

	storeA, err := OpenPostgres(db, userANumber)
	if err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM user_state WHERE user_id = $1`, userA).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("opening a user created %d state rows", count)
	}

	connection := GitHubConnection{
		GitHubID:    userANumber,
		Login:       "postgres-user",
		ConnectedAt: time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC),
		TokenRef:    "github.postgres-test",
	}
	storeA.SetGitHubConnection(connection)
	if err := storeA.AddProtectedRepo(ProtectedRepo{ID: "repo-1", GitHubID: 99, FullName: "owner/repo"}); err != nil {
		t.Fatal(err)
	}
	if err := storeA.Save(); err != nil {
		t.Fatal(err)
	}

	var data string
	var updated bool
	if err := db.QueryRow(`SELECT data::text, updated_at IS NOT NULL FROM user_state WHERE user_id = $1`, userA).Scan(&data, &updated); err != nil {
		t.Fatal(err)
	}
	if !updated || len(data) == 0 {
		t.Fatal("state row must contain JSON data and updated_at")
	}

	reloaded, err := OpenPostgres(db, userANumber)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reloaded.GitHubConnection()
	if !ok || got.Login != connection.Login || got.TokenRef != connection.TokenRef {
		t.Fatalf("connection mismatch: %+v ok=%v", got, ok)
	}
	if repos := reloaded.ProtectedRepos(); len(repos) != 1 || repos[0].FullName != "owner/repo" {
		t.Fatalf("protected repos mismatch: %+v", repos)
	}

	storeB, err := OpenPostgres(db, userBNumber)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := storeB.GitHubConnection(); ok {
		t.Fatal("PostgreSQL state leaked across users")
	}
	storeB.SetGitHubConnection(GitHubConnection{GitHubID: userBNumber, Login: "other-user", TokenRef: "other-ref"})
	if err := storeB.Save(); err != nil {
		t.Fatal(err)
	}
	if got, _ := reloaded.GitHubConnection(); got.Login != "postgres-user" {
		t.Fatalf("saving user B changed user A state: %+v", got)
	}
	if err := storeA.Delete(); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM user_state WHERE user_id = $1`, userA).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("Delete left %d state rows", count)
	}
}

func TestPostgresStateStoreRejectsInvalidJSON(t *testing.T) {
	db := openPostgresStateTest(t)
	userIDNumber := time.Now().UnixNano()
	userID := strconv.FormatInt(userIDNumber, 10)
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM user_state WHERE user_id = $1`, userID) })
	_, err := db.Exec(`INSERT INTO user_state (user_id, data) VALUES ($1, $2::jsonb)`, userID, `{"version":1,"unknown":true}`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenPostgres(db, userIDNumber); !errors.Is(err, ErrCorruptState) {
		t.Fatalf("invalid JSONB state error = %v, want ErrCorruptState", err)
	}
}
