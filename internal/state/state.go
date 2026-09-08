package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Sentinel errors surfaced by the store.
var (
	// ErrCorruptState is returned when an existing state file cannot be parsed
	// as structurally valid state. The existing file is left untouched.
	ErrCorruptState = errors.New("state: state file is corrupt or structurally invalid")
	// ErrUnsupportedVersion is returned when the persisted schema version is
	// missing or not supported by this build.
	ErrUnsupportedVersion = errors.New("state: unsupported schema version")
	// ErrNotFound is returned when a requested entity does not exist.
	ErrNotFound = errors.New("state: not found")
	// ErrDuplicate is returned when adding an entity that already exists.
	ErrDuplicate = errors.New("state: duplicate entity")
)

// interruptedMessage is the fixed, sanitized message applied to jobs that were
// running when GitSafe shut down.
const interruptedMessage = "interrupted by shutdown; no automatic resume"

// Store is a serialized, atomic JSON state store. All access is guarded by a
// mutex so concurrent goroutines cannot corrupt state. Callers mutate in
// memory, then call Save to persist atomically (mirroring the existing
// config.Save pattern).
type Store struct {
	mu   sync.RWMutex
	path string
	doc  document
}

// Open loads the state at path, or initializes a clean empty document when no
// state exists yet. On load, jobs that were not in a terminal state are marked
// interrupted (never auto-resumed; not persisted until the next Save).
//
// Callers should always check for ErrCorruptState/ErrUnsupportedVersion before
// acting, since the existing file is preserved but cannot be used.
func Open(path string) (*Store, error) {
	if path == "" {
		return nil, fmt.Errorf("state: empty path")
	}
	s := &Store{path: path, doc: cleanDocument()}

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// First run: no file, clean initialized state. Not written to disk
			// until the first Save.
			return s, nil
		}
		return nil, fmt.Errorf("state: read %q: %w", path, err)
	}

	if err := s.decode(data); err != nil {
		return nil, err
	}
	s.normalizeInterrupted()
	return s, nil
}

// Path returns the state file location.
func (s *Store) Path() string { return s.path }

func cleanDocument() document {
	return document{
		Version:        CurrentVersion,
		GitHub:         nil,
		Drive:          nil,
		ProtectedRepos: []ProtectedRepo{},
		BackupRecords:  []BackupRecord{},
		BackupJobs:     []BackupJob{},
	}
}

// decode parses the raw state bytes, strictly. It returns ErrUnsupportedVersion
// for a missing/unknown version and ErrCorruptState for any structural problem.
// The file itself is never modified here.
func (s *Store) decode(data []byte) error {
	// Reject empty file / bare "null".
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return fmt.Errorf("%w: file is empty", ErrCorruptState)
	}
	// The document must be a JSON object. Anything else (null, array, scalar)
	// is structurally invalid, not merely a version problem.
	if trimmed[0] != '{' {
		return fmt.Errorf("%w: top-level value is not an object", ErrCorruptState)
	}

	// First, triage the version independently so we can distinguish a bad
	// version from general corruption.
	var v struct {
		Version *int `json:"version"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&v); err != nil {
		return fmt.Errorf("%w: %v", ErrCorruptState, err)
	}
	if v.Version == nil {
		return fmt.Errorf("%w: missing version field", ErrUnsupportedVersion)
	}
	if *v.Version != CurrentVersion {
		return fmt.Errorf("%w: got %d, supported %d", ErrUnsupportedVersion, *v.Version, CurrentVersion)
	}

	// Full strict decode: unknown fields, wrong types, or trailing garbage all
	// count as structurally invalid.
	var doc document
	dec2 := json.NewDecoder(bytes.NewReader(data))
	dec2.DisallowUnknownFields()
	if err := dec2.Decode(&doc); err != nil {
		return fmt.Errorf("%w: %v", ErrCorruptState, err)
	}
	if err := ensureNoTrailing(dec2); err != nil {
		return fmt.Errorf("%w: %v", ErrCorruptState, err)
	}
	if doc.GitHub == nil {
		doc.GitHub = nil
	}
	if doc.Drive == nil {
		doc.Drive = nil
	}
	if doc.ProtectedRepos == nil {
		doc.ProtectedRepos = []ProtectedRepo{}
	}
	if doc.BackupRecords == nil {
		doc.BackupRecords = []BackupRecord{}
	}
	if doc.BackupJobs == nil {
		doc.BackupJobs = []BackupJob{}
	}
	s.doc = doc
	return nil
}

func ensureNoTrailing(dec *json.Decoder) error {
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("unexpected trailing data after JSON document")
		}
		return err
	}
	return nil
}

// normalizeInterrupted marks any non-terminal job as interrupted. It runs only
// in memory; the caller decides when to persist (next Save).
func (s *Store) normalizeInterrupted() {
	now := time.Now()
	for i := range s.doc.BackupJobs {
		j := &s.doc.BackupJobs[i]
		if IsTerminalState(j.State) {
			continue
		}
		if j.State != JobInterrupted {
			j.State = JobInterrupted
		}
		if j.Error == "" {
			j.Error = interruptedMessage
		}
		if j.FinishedAt.IsZero() {
			j.FinishedAt = now
		}
	}
}

// Document returns a copy of the current state document. Calls that only need
// to read state should prefer the typed accessors below.
func (s *Store) Document() Document {
	s.mu.RLock()
	defer s.mu.RUnlock()
	d := Document{
		Version:        s.doc.Version,
		ProtectedRepos: cloneRepos(s.doc.ProtectedRepos),
		BackupRecords:  cloneRecords(s.doc.BackupRecords),
		BackupJobs:     cloneJobs(s.doc.BackupJobs),
	}
	if s.doc.GitHub != nil {
		g := *s.doc.GitHub
		d.GitHub = &g
	}
	if s.doc.Drive != nil {
		g := *s.doc.Drive
		d.Drive = &g
	}
	return d
}

// Save serializes and atomically replaces the state file. On any failure the
// previous valid file remains intact.
func (s *Store) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked()
}

func (s *Store) saveLocked() error {
	parent := filepath.Dir(s.path)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("state: create directory %q: %w", parent, err)
	}

	data, err := json.MarshalIndent(s.doc, "", "  ")
	if err != nil {
		return fmt.Errorf("state: marshal: %w", err)
	}

	tmp, err := os.CreateTemp(parent, ".gitsafe-state-*.tmp")
	if err != nil {
		return fmt.Errorf("state: create temp file: %w", err)
	}
	tmpName := tmp.Name()
	// Ensure the temp file is cleaned up on any failure so it never becomes a
	// primary state file.
	defer func() {
		if tmp != nil {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("state: write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("state: sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("state: close temp file: %w", err)
	}

	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("state: replace %q: %w", s.path, err)
	}
	tmp = nil // success; temp already renamed away
	return nil
}

// --- Connections ---

func (s *Store) GitHubConnection() (GitHubConnection, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.doc.GitHub == nil {
		return GitHubConnection{}, false
	}
	return *s.doc.GitHub, true
}

func (s *Store) SetGitHubConnection(c GitHubConnection) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cc := c
	s.doc.GitHub = &cc
}

func (s *Store) ClearGitHubConnection() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.doc.GitHub = nil
}

func (s *Store) DriveConnection() (DriveConnection, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.doc.Drive == nil {
		return DriveConnection{}, false
	}
	return *s.doc.Drive, true
}

func (s *Store) SetDriveConnection(c DriveConnection) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cc := c
	s.doc.Drive = &cc
}

func (s *Store) ClearDriveConnection() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.doc.Drive = nil
}

// --- Protected repositories ---

func (s *Store) ProtectedRepos() []ProtectedRepo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneRepos(s.doc.ProtectedRepos)
}

func (s *Store) ProtectedRepo(id string) (ProtectedRepo, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, r := range s.doc.ProtectedRepos {
		if r.ID == id {
			return r, true
		}
	}
	return ProtectedRepo{}, false
}

// AddProtectedRepo appends a new protected repository. It refuses duplicates by
// GitHubID (a repository can only be protected once).
func (s *Store) AddProtectedRepo(r ProtectedRepo) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.doc.ProtectedRepos {
		if existing.GitHubID == r.GitHubID {
			return fmt.Errorf("%w: repository %d already protected", ErrDuplicate, r.GitHubID)
		}
	}
	s.doc.ProtectedRepos = append(s.doc.ProtectedRepos, r)
	return nil
}

func (s *Store) UpdateProtectedRepo(r ProtectedRepo) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.doc.ProtectedRepos {
		if s.doc.ProtectedRepos[i].ID == r.ID {
			s.doc.ProtectedRepos[i] = r
			return nil
		}
	}
	return fmt.Errorf("%w: protected repo %q", ErrNotFound, r.ID)
}

func (s *Store) RemoveProtectedRepo(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.doc.ProtectedRepos {
		if s.doc.ProtectedRepos[i].ID == id {
			s.doc.ProtectedRepos = append(s.doc.ProtectedRepos[:i], s.doc.ProtectedRepos[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("%w: protected repo %q", ErrNotFound, id)
}

// --- Backup records ---

func (s *Store) AddBackupRecord(rec BackupRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.doc.BackupRecords = append(s.doc.BackupRecords, rec)
}

func (s *Store) UpdateBackupRecord(rec BackupRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.doc.BackupRecords {
		if s.doc.BackupRecords[i].ID == rec.ID {
			s.doc.BackupRecords[i] = rec
			return nil
		}
	}
	return fmt.Errorf("%w: backup record %q", ErrNotFound, rec.ID)
}

func (s *Store) RemoveBackupRecord(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.doc.BackupRecords {
		if s.doc.BackupRecords[i].ID == id {
			s.doc.BackupRecords = append(s.doc.BackupRecords[:i], s.doc.BackupRecords[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("%w: backup record %q", ErrNotFound, id)
}

func (s *Store) BackupRecords() []BackupRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneRecords(s.doc.BackupRecords)
}

func (s *Store) BackupRecordsForRepo(protectedRepoID string) []BackupRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []BackupRecord{}
	for _, r := range s.doc.BackupRecords {
		if r.ProtectedRepoID == protectedRepoID {
			out = append(out, r)
		}
	}
	return out
}

// --- Backup jobs ---

func (s *Store) CreateBackupJob(j BackupJob) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.doc.BackupJobs = append(s.doc.BackupJobs, j)
}

func (s *Store) UpdateBackupJob(j BackupJob) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.doc.BackupJobs {
		if s.doc.BackupJobs[i].ID == j.ID {
			s.doc.BackupJobs[i] = j
			return nil
		}
	}
	return fmt.Errorf("%w: backup job %q", ErrNotFound, j.ID)
}

func (s *Store) RemoveBackupJob(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.doc.BackupJobs {
		if s.doc.BackupJobs[i].ID == id {
			s.doc.BackupJobs = append(s.doc.BackupJobs[:i], s.doc.BackupJobs[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("%w: backup job %q", ErrNotFound, id)
}

func (s *Store) BackupJob(id string) (BackupJob, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, j := range s.doc.BackupJobs {
		if j.ID == id {
			return j, true
		}
	}
	return BackupJob{}, false
}

// BackupJobs returns a defensive copy of all persisted backup jobs.
func (s *Store) BackupJobs() []BackupJob {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneJobs(s.doc.BackupJobs)
}

// UnfinishedJobs returns jobs that have not reached a terminal state.
func (s *Store) UnfinishedJobs() []BackupJob {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []BackupJob{}
	for _, j := range s.doc.BackupJobs {
		if !IsTerminalState(j.State) {
			out = append(out, j)
		}
	}
	return out
}

// --- clone helpers (defensive copies for callers) ---

func cloneRepos(in []ProtectedRepo) []ProtectedRepo {
	if in == nil {
		return nil
	}
	out := make([]ProtectedRepo, len(in))
	copy(out, in)
	return out
}

func cloneRecords(in []BackupRecord) []BackupRecord {
	if in == nil {
		return nil
	}
	out := make([]BackupRecord, len(in))
	for i, r := range in {
		out[i] = r
		if r.HeadSHAs != nil {
			m := make(map[string]string, len(r.HeadSHAs))
			for k, v := range r.HeadSHAs {
				m[k] = v
			}
			out[i].HeadSHAs = m
		}
	}
	return out
}

func cloneJobs(in []BackupJob) []BackupJob {
	if in == nil {
		return nil
	}
	out := make([]BackupJob, len(in))
	copy(out, in)
	return out
}
