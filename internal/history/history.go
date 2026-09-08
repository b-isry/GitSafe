// Package history persists a bounded, newest-first log of completed cleanup
// runs. It is a companion to the retention engine's in-memory run status: the
// last run is observable via status, while every run is observable via history.
//
// The store is deliberately resilient: it never fails startup (a missing,
// corrupt or unreadable file yields a clean start with a loud warning), writes
// are atomic (temp file + fsync + rename), and recording failures are logged —
// cleanup itself must never fail because its history could not be written. All
// callers that already hold the server's cleanup lock can keep that lock: the
// store guards itself with its own mutexes and never participates in any other
// lock ordering.
package history

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// DefaultPath is where the cleanup history file lives relative to the working
// directory (the same .gitsafe dir that holds state.json).
const DefaultPath = ".gitsafe/cleanup-history.json"

// currentVersion is the on-disk schema version for the history document.
const currentVersion = 1

// Entry is one persisted cleanup run.
type Entry struct {
	StartedAt     time.Time `json:"startedAt"`
	FinishedAt    time.Time `json:"finishedAt"`
	Trigger       string    `json:"trigger"`
	Success       bool      `json:"success"`
	Inspected     int       `json:"inspected"`
	Retained      int       `json:"retained"`
	LocalDeleted  int       `json:"localDeleted"`
	DriveDeleted  int       `json:"driveDeleted"`
	OrphanDeleted int       `json:"orphanDeleted"`
	Skipped       int       `json:"skipped"`
	Missing       int       `json:"missing"`
	ErrorCount    int       `json:"errorCount"`
	Errors        []string  `json:"errors,omitempty"`
	// NotifyError records that the delivery of the webhook notification failed.
	// It is intentionally distinct from cleanup Errors: a notification problem
	// must not look like a cleanup failure, but an operator should still see it.
	NotifyError string `json:"notifyError,omitempty"`
}

// LimitFunc returns the current cleanup-history limit from the live
// configuration. A limit <= 0 disables recording. Reading it per operation lets
// a settings save take effect without restarting the store.
type LimitFunc func() int

// Store keeps a bounded, newest-first list of cleanup entries, persisted
// atomically. entries are guarded by mu; physical writes are additionally
// serialized by writeMu so slow disk I/O never blocks a reader.
type Store struct {
	mu      sync.RWMutex
	writeMu sync.Mutex
	path    string
	limit   LimitFunc
	logger  *slog.Logger
	entries []Entry
}

// doc is the on-disk shape.
type doc struct {
	Version int     `json:"version"`
	Entries []Entry `json:"entries"`
}

// Open loads the history at path, or starts clean when the file is missing,
// corrupt, or unreadable. It never returns an error for data problems — those
// are logged (loudly) and the store continues empty — so callers can treat it
// as infallible at startup. Only an internal precondition (empty path) errors.
func Open(path string, limit LimitFunc, logger *slog.Logger) (*Store, error) {
	if path == "" {
		return nil, errors.New("history: empty path")
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	s := &Store{
		path:    path,
		limit:   limit,
		logger:  logger,
		entries: []Entry{},
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			// Permission or I/O problem: keep running clean rather than fail
			// startup, but tell the operator loudly.
			s.logger.Error("cleanup history unreadable; starting with empty history", "path", path, "error", err)
		}
		return s, nil
	}

	loaded, perr := decode(data)
	if perr != nil {
		// Corrupt history is never a startup failure: report loudly and start
		// clean. The corrupt file is left untouched until the first Add
		// atomically replaces it.
		s.logger.Error("cleanup history corrupt; starting with empty history", "path", path, "error", perr)
		return s, nil
	}
	s.entries = loaded
	return s, nil
}

func decode(data []byte) ([]Entry, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, errors.New("not a JSON object")
	}

	var raw struct {
		Version *int    `json:"version"`
		Entries []Entry `json:"entries"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return nil, err
	}
	if err := ensureNoTrailing(dec); err != nil {
		return nil, err
	}
	if raw.Version == nil || *raw.Version != currentVersion {
		return nil, fmt.Errorf("unsupported version %v", raw.Version)
	}

	// Individual entries are validated leniently: a malformed entry is dropped
	// rather than rejecting the whole history, but structural corruption still
	// counts as corrupt.
	entries := []Entry{}
	for _, e := range raw.Entries {
		if e.FinishedAt.IsZero() {
			continue
		}
		entries = append(entries, e)
	}
	return entries, nil
}

func ensureNoTrailing(dec *json.Decoder) error {
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("trailing data after JSON document")
		}
		return err
	}
	return nil
}

// Add records a new entry as the newest and persists it atomically. When the
// configured limit is disabled (<= 0) the entry is dropped and nothing is
// written. Recording failures are returned so the caller can log them, but a
// failure here must never fail the operation that produced the entry.
func (s *Store) Add(e Entry) error {
	if e.FinishedAt.IsZero() {
		e.FinishedAt = time.Now()
	}
	if s.limit() <= 0 {
		return nil
	}

	s.mu.Lock()
	s.entries = append([]Entry{e}, s.entries...)
	if n := s.limit(); n > 0 && len(s.entries) > n {
		s.entries = s.entries[:n]
	}
	s.mu.Unlock()

	return s.persist()
}

// Recent returns up to n newest entries as a defensive copy. A zero limit
// (recording disabled) returns nothing.
func (s *Store) Recent(n int) []Entry {
	if s.limit() <= 0 || n <= 0 {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if n > len(s.entries) {
		n = len(s.entries)
	}
	out := make([]Entry, n)
	copy(out, s.entries[:n])
	for i := range out {
		if out[i].Errors != nil {
			errs := make([]string, len(out[i].Errors))
			copy(errs, out[i].Errors)
			out[i].Errors = errs
		}
	}
	return out
}

// Len returns the number of in-memory entries (regardless of the disabled
// limit, so callers can distinguish "empty" from "disabled").
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.entries)
}

// persist writes the current entries atomically and logs loudly on failure.
func (s *Store) persist() error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	s.mu.RLock()
	docData := doc{Version: currentVersion, Entries: s.entries}
	s.mu.RUnlock()

	data, err := json.MarshalIndent(docData, "", "  ")
	if err != nil {
		s.logger.Error("cleanup history marshal failed", "error", err)
		return fmt.Errorf("history: marshal entries: %w", err)
	}

	parent := filepath.Dir(s.path)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		s.logger.Error("cleanup history directory create failed", "path", parent, "error", err)
		return fmt.Errorf("history: create directory %q: %w", parent, err)
	}

	tmp, err := os.CreateTemp(parent, ".cleanup-history-*.tmp")
	if err != nil {
		s.logger.Error("cleanup history temp file create failed", "path", parent, "error", err)
		return fmt.Errorf("history: create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		if tmp != nil {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		s.logger.Error("cleanup history temp file write failed", "error", err)
		return fmt.Errorf("history: write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		s.logger.Error("cleanup history temp file sync failed", "error", err)
		return fmt.Errorf("history: sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		s.logger.Error("cleanup history temp file close failed", "error", err)
		return fmt.Errorf("history: close temp file: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		s.logger.Error("cleanup history finalize failed", "path", s.path, "error", err)
		return fmt.Errorf("history: replace %q: %w", s.path, err)
	}
	tmp = nil
	return nil
}
