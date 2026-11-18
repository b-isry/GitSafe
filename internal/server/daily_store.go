package server

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Daily backup status values.
const (
	dailyStatusOK          = "ok"
	dailyStatusPartial     = "partial"
	dailyStatusFailed      = "failed"
	dailyStatusNeedsReauth = "needs_reauth"
)

// Values of DailySettings.NeedsReauth.
const (
	reauthGitHub = "github"
	reauthDrive  = "drive"
)

// DailySettings is a user's opt-in for unattended daily backups plus the
// outcome of the most recent run.
type DailySettings struct {
	UserID      int64     `json:"-"`
	Enabled     bool      `json:"enabled"`
	NeedsReauth string    `json:"needsReauth,omitempty"` // "", "github" or "drive"
	LastRunAt   time.Time `json:"lastRunAt"`
	LastStatus  string    `json:"lastStatus,omitempty"`
	LastMessage string    `json:"lastMessage,omitempty"`
	LastOK      int       `json:"lastOk"`
	LastFailed  int       `json:"lastFailed"`
	LastSkipped int       `json:"lastSkipped"`
}

// DailyStore persists daily-backup settings. It is the only place the server
// can enumerate users (the per-user state stores are keyed by user), which is
// why the cron runner needs it. The OAuth tokens themselves stay in the
// encrypted token store.
type DailyStore interface {
	Get(ctx context.Context, userID int64) (DailySettings, error)
	// SetEnabled turns daily backups on or off. Turning them on clears any
	// pending re-authorization flag.
	SetEnabled(ctx context.Context, userID int64, enabled bool) error
	// ClearNeedsReauth clears the flag when it equals provider.
	ClearNeedsReauth(ctx context.Context, userID int64, provider string) error
	ListEnabled(ctx context.Context) ([]int64, error)
	// RecordRun stores the outcome of a run (and NeedsReauth) for a user.
	RecordRun(ctx context.Context, s DailySettings) error
	Delete(ctx context.Context, userID int64) error
}

// --- in-memory (local development and tests) ---

type memoryDailyStore struct {
	mu   sync.Mutex
	rows map[int64]DailySettings
}

func newMemoryDailyStore() *memoryDailyStore {
	return &memoryDailyStore{rows: map[int64]DailySettings{}}
}

func (m *memoryDailyStore) Get(_ context.Context, userID int64) (DailySettings, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.rows[userID]
	s.UserID = userID
	return s, nil
}

func (m *memoryDailyStore) SetEnabled(_ context.Context, userID int64, enabled bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.rows[userID]
	s.UserID, s.Enabled = userID, enabled
	if enabled {
		s.NeedsReauth = ""
	}
	m.rows[userID] = s
	return nil
}

func (m *memoryDailyStore) ClearNeedsReauth(_ context.Context, userID int64, provider string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.rows[userID]; ok && s.NeedsReauth == provider {
		s.NeedsReauth = ""
		m.rows[userID] = s
	}
	return nil
}

func (m *memoryDailyStore) ListEnabled(_ context.Context) ([]int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var ids []int64
	for id, s := range m.rows {
		if s.Enabled {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids, nil
}

func (m *memoryDailyStore) RecordRun(_ context.Context, in DailySettings) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.rows[in.UserID]
	enabled := s.Enabled
	s = in
	s.Enabled = enabled // a run never changes the opt-in itself
	m.rows[in.UserID] = s
	return nil
}

func (m *memoryDailyStore) Delete(_ context.Context, userID int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.rows, userID)
	return nil
}

// --- PostgreSQL ---

// postgresDailyStore uses the daily_backup_settings table created by
// internal/postgres.EnsureSchema.
type postgresDailyStore struct {
	db *sql.DB
}

func newPostgresDailyStore(db *sql.DB) *postgresDailyStore { return &postgresDailyStore{db: db} }

const dailyQueryTimeout = 10 * time.Second

func uid(userID int64) string { return strconv.FormatInt(userID, 10) }

func (p *postgresDailyStore) Get(ctx context.Context, userID int64) (DailySettings, error) {
	ctx, cancel := context.WithTimeout(ctx, dailyQueryTimeout)
	defer cancel()
	s := DailySettings{UserID: userID}
	var lastRun sql.NullTime
	err := p.db.QueryRowContext(ctx, `SELECT enabled, needs_reauth, last_run_at, last_status, last_message,
		last_ok, last_failed, last_skipped FROM daily_backup_settings WHERE user_id = $1`, uid(userID)).
		Scan(&s.Enabled, &s.NeedsReauth, &lastRun, &s.LastStatus, &s.LastMessage, &s.LastOK, &s.LastFailed, &s.LastSkipped)
	if errors.Is(err, sql.ErrNoRows) {
		return s, nil
	}
	if err != nil {
		return DailySettings{}, err
	}
	if lastRun.Valid {
		s.LastRunAt = lastRun.Time
	}
	return s, nil
}

func (p *postgresDailyStore) SetEnabled(ctx context.Context, userID int64, enabled bool) error {
	ctx, cancel := context.WithTimeout(ctx, dailyQueryTimeout)
	defer cancel()
	_, err := p.db.ExecContext(ctx, `INSERT INTO daily_backup_settings (user_id, enabled, updated_at)
		VALUES ($1, $2, now())
		ON CONFLICT (user_id) DO UPDATE SET enabled = EXCLUDED.enabled,
			needs_reauth = CASE WHEN EXCLUDED.enabled THEN '' ELSE daily_backup_settings.needs_reauth END,
			updated_at = now()`, uid(userID), enabled)
	return err
}

func (p *postgresDailyStore) ClearNeedsReauth(ctx context.Context, userID int64, provider string) error {
	ctx, cancel := context.WithTimeout(ctx, dailyQueryTimeout)
	defer cancel()
	_, err := p.db.ExecContext(ctx, `UPDATE daily_backup_settings SET needs_reauth = '', updated_at = now()
		WHERE user_id = $1 AND needs_reauth = $2`, uid(userID), provider)
	return err
}

func (p *postgresDailyStore) ListEnabled(ctx context.Context) ([]int64, error) {
	ctx, cancel := context.WithTimeout(ctx, dailyQueryTimeout)
	defer cancel()
	rows, err := p.db.QueryContext(ctx, `SELECT user_id FROM daily_backup_settings WHERE enabled ORDER BY user_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id <= 0 {
			continue
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (p *postgresDailyStore) RecordRun(ctx context.Context, s DailySettings) error {
	ctx, cancel := context.WithTimeout(ctx, dailyQueryTimeout)
	defer cancel()
	_, err := p.db.ExecContext(ctx, `UPDATE daily_backup_settings SET needs_reauth = $2, last_run_at = $3,
		last_status = $4, last_message = $5, last_ok = $6, last_failed = $7, last_skipped = $8, updated_at = now()
		WHERE user_id = $1`,
		uid(s.UserID), s.NeedsReauth, s.LastRunAt, s.LastStatus, s.LastMessage, s.LastOK, s.LastFailed, s.LastSkipped)
	return err
}

func (p *postgresDailyStore) Delete(ctx context.Context, userID int64) error {
	ctx, cancel := context.WithTimeout(ctx, dailyQueryTimeout)
	defer cancel()
	_, err := p.db.ExecContext(ctx, `DELETE FROM daily_backup_settings WHERE user_id = $1`, uid(userID))
	return err
}
