package tokenstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
)

type PostgresTokenStore struct {
	db     *sql.DB
	userID string
	key    [32]byte
}

var _ Store = (*PostgresTokenStore)(nil)

func NewPostgresStore(db *sql.DB, userID int64, keyMaterial string) (*PostgresTokenStore, error) {
	if db == nil {
		return nil, errors.New("tokenstore: postgres store requires a database")
	}
	if userID <= 0 {
		return nil, errors.New("tokenstore: postgres store requires a positive user ID")
	}
	key, err := deriveTokenKey(keyMaterial)
	if err != nil {
		return nil, fmt.Errorf("tokenstore: postgres store: %w", err)
	}
	return &PostgresTokenStore{
		db:     db,
		userID: strconv.FormatInt(userID, 10),
		key:    key,
	}, nil
}

func (p *PostgresTokenStore) Set(ref, value string) error {
	if ref == "" {
		return errors.New("tokenstore: empty reference")
	}
	nonce, ciphertext, err := encryptToken(p.key, value)
	if err != nil {
		return fmt.Errorf("tokenstore: encrypt %q: %w", ref, err)
	}
	_, err = p.db.ExecContext(context.Background(), `
		INSERT INTO user_tokens (user_id, ref, nonce, ciphertext)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (user_id, ref) DO UPDATE
		SET nonce = EXCLUDED.nonce, ciphertext = EXCLUDED.ciphertext`, p.userID, ref, nonce, ciphertext)
	if err != nil {
		return fmt.Errorf("tokenstore: set %q: %w", ref, err)
	}
	return nil
}

func (p *PostgresTokenStore) Get(ref string) (string, error) {
	if ref == "" {
		return "", errors.New("tokenstore: empty reference")
	}
	var nonce, ciphertext []byte
	err := p.db.QueryRowContext(context.Background(), `
		SELECT nonce, ciphertext
		FROM user_tokens
		WHERE user_id = $1 AND ref = $2`, p.userID, ref).Scan(&nonce, &ciphertext)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("tokenstore: get %q: %w", ref, err)
	}
	value, err := decryptToken(p.key, nonce, ciphertext)
	if err != nil {
		return "", fmt.Errorf("tokenstore: decrypt %q: %w", ref, err)
	}
	return value, nil
}

func (p *PostgresTokenStore) Delete(ref string) error {
	if ref == "" {
		return errors.New("tokenstore: empty reference")
	}
	_, err := p.db.ExecContext(context.Background(), `
		DELETE FROM user_tokens
		WHERE user_id = $1 AND ref = $2`, p.userID, ref)
	if err != nil {
		return fmt.Errorf("tokenstore: delete %q: %w", ref, err)
	}
	return nil
}
