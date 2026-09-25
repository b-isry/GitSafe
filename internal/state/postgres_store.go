package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
)

type PostgresStore struct {
	*Store
	db     *sql.DB
	userID string
}

type postgresPersistenceBackend struct {
	db     *sql.DB
	userID string
}

func OpenPostgres(db *sql.DB, userID int64) (*PostgresStore, error) {
	if db == nil {
		return nil, errors.New("state: postgres store requires a database")
	}
	if userID <= 0 {
		return nil, errors.New("state: postgres store requires a positive user ID")
	}
	id := strconv.FormatInt(userID, 10)
	store, err := openStore("", &postgresPersistenceBackend{db: db, userID: id})
	if err != nil {
		return nil, err
	}
	return &PostgresStore{Store: store, db: db, userID: id}, nil
}

func (p *postgresPersistenceBackend) load() ([]byte, bool, error) {
	var data string
	err := p.db.QueryRowContext(context.Background(), `
		SELECT data::text
		FROM user_state
		WHERE user_id = $1`, p.userID).Scan(&data)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("state: read user %s: %w", p.userID, err)
	}
	return []byte(data), true, nil
}

func (p *postgresPersistenceBackend) save(data []byte) error {
	_, err := p.db.ExecContext(context.Background(), `
		INSERT INTO user_state (user_id, data, updated_at)
		VALUES ($1, $2::jsonb, now())
		ON CONFLICT (user_id) DO UPDATE
		SET data = EXCLUDED.data, updated_at = now()`, p.userID, string(data))
	if err != nil {
		return fmt.Errorf("state: save user %s: %w", p.userID, err)
	}
	return nil
}

func (p *PostgresStore) Delete() error {
	_, err := p.db.ExecContext(context.Background(), `
		DELETE FROM user_state
		WHERE user_id = $1`, p.userID)
	if err != nil {
		return fmt.Errorf("state: delete user %s: %w", p.userID, err)
	}
	return nil
}
