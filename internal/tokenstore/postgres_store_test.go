//go:build postgres

package tokenstore

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"strconv"
	"testing"
	"time"

	postgresdb "github.com/b-isry/gitsafe/internal/postgres"
)

func openPostgresTokenTest(t *testing.T) *sql.DB {
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

func TestPostgresTokenStoreRoundTripAndIsolation(t *testing.T) {
	db := openPostgresTokenTest(t)
	userANumber := time.Now().UnixNano()
	userBNumber := userANumber + 1
	userA := strconv.FormatInt(userANumber, 10)
	userB := strconv.FormatInt(userBNumber, 10)
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM user_tokens WHERE user_id = $1 OR user_id = $2`, userA, userB)
	})

	storeA, err := NewPostgresStore(db, userANumber, "shared-key")
	if err != nil {
		t.Fatal(err)
	}
	storeB, err := NewPostgresStore(db, userBNumber, "shared-key")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storeA.Get("github.primary"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing token = %v, want ErrNotFound", err)
	}
	if err := storeA.Set("github.primary", "token-a"); err != nil {
		t.Fatal(err)
	}
	if err := storeB.Set("github.primary", "token-b"); err != nil {
		t.Fatal(err)
	}
	gotA, err := storeA.Get("github.primary")
	if err != nil || gotA != "token-a" {
		t.Fatalf("user A token = %q, %v", gotA, err)
	}
	gotB, err := storeB.Get("github.primary")
	if err != nil || gotB != "token-b" {
		t.Fatalf("user B token = %q, %v", gotB, err)
	}

	var nonce, ciphertext []byte
	if err := db.QueryRow(`SELECT nonce, ciphertext FROM user_tokens WHERE user_id = $1 AND ref = $2`, userA, "github.primary").Scan(&nonce, &ciphertext); err != nil {
		t.Fatal(err)
	}
	if len(nonce) != tokenNonceSize || len(ciphertext) == 0 {
		t.Fatalf("invalid encrypted columns: nonce=%d ciphertext=%d", len(nonce), len(ciphertext))
	}
	if bytes.Contains(nonce, []byte("token-a")) || bytes.Contains(ciphertext, []byte("token-a")) {
		t.Fatal("token persisted in plaintext")
	}

	if err := storeA.Set("github.primary", "token-a-updated"); err != nil {
		t.Fatal(err)
	}
	if got, err := storeA.Get("github.primary"); err != nil || got != "token-a-updated" {
		t.Fatalf("updated token = %q, %v", got, err)
	}
	wrongKey, err := NewPostgresStore(db, userANumber, "wrong-key")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wrongKey.Get("github.primary"); err == nil {
		t.Fatal("wrong key must fail authentication")
	}
	if err := storeA.Delete("github.primary"); err != nil {
		t.Fatal(err)
	}
	if err := storeA.Delete("github.primary"); err != nil {
		t.Fatalf("delete must be idempotent: %v", err)
	}
	if _, err := storeA.Get("github.primary"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted token = %v, want ErrNotFound", err)
	}
}

func TestPostgresTokenStoreRejectsEmptyArguments(t *testing.T) {
	db := openPostgresTokenTest(t)
	store, err := NewPostgresStore(db, time.Now().UnixNano(), "key")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set("", "value"); err == nil {
		t.Fatal("empty reference must fail")
	}
	if _, err := store.Get(""); err == nil {
		t.Fatal("empty reference must fail")
	}
	if err := store.Delete(""); err == nil {
		t.Fatal("empty reference must fail")
	}
}
