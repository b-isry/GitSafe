package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

var schema = []string{
	`CREATE TABLE IF NOT EXISTS user_tokens (
		user_id text NOT NULL,
		ref text NOT NULL,
		nonce bytea NOT NULL,
		ciphertext bytea NOT NULL,
		PRIMARY KEY (user_id, ref)
	)`,
	`CREATE TABLE IF NOT EXISTS user_state (
		user_id text PRIMARY KEY,
		data jsonb NOT NULL,
		updated_at timestamptz NOT NULL DEFAULT now()
	)`,
}

func Open(ctx context.Context, databaseURL string) (*sql.DB, error) {
	if databaseURL == "" {
		return nil, errors.New("postgres: DATABASE_URL is empty")
	}
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return nil, fmt.Errorf("postgres: open database: %w", err)
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxIdleTime(5 * time.Minute)
	db.SetConnMaxLifetime(30 * time.Minute)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("postgres: database unreachable: %w", err)
	}
	return db, nil
}

func EnsureSchema(ctx context.Context, db *sql.DB) error {
	if db == nil {
		return errors.New("postgres: nil database")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("postgres: begin schema setup: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(1835655459, 1702260581)`); err != nil {
		return fmt.Errorf("postgres: lock schema setup: %w", err)
	}
	for _, statement := range schema {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("postgres: create schema: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("postgres: commit schema setup: %w", err)
	}
	return nil
}
