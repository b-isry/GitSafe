package server

import (
	"log/slog"
	"sync"
	"time"

	"github.com/b-isry/gitsafe/internal/backup"
)

// BackupJob tracks a single backup run in-flight, mirroring the backup package's
// state machine for polling by the UI.
type BackupJob struct {
	ID        string        `json:"id"`
	RepoPath  string        `json:"repoPath"`
	State     backup.State  `json:"state"`
	Bundle    backup.Result `json:"bundle"`
	UpdatedAt time.Time     `json:"updatedAt"`
}

type BackupStore interface {
	Create(id, repoPath string) *BackupJob
	Get(id string) (*BackupJob, bool)
	Update(id string, res backup.Result)
	// ActiveForRepo reports whether a backup for repoPath is currently running
	// (idle, creating, or uploading) so duplicate jobs are not started.
	ActiveForRepo(repoPath string) bool
}

type memoryStore struct {
	mu     sync.RWMutex
	jobs   map[string]*BackupJob
	logger *slog.Logger
}

func NewBackupStore(logger *slog.Logger) BackupStore {
	return &memoryStore{
		jobs:   make(map[string]*BackupJob),
		logger: logger,
	}
}

func (s *memoryStore) Create(id, repoPath string) *BackupJob {
	s.mu.Lock()
	defer s.mu.Unlock()
	job := &BackupJob{
		ID:        id,
		RepoPath:  repoPath,
		State:     backup.StateIdle,
		UpdatedAt: time.Now(),
	}
	s.jobs[id] = job
	return job
}

func (s *memoryStore) Get(id string) (*BackupJob, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	job, ok := s.jobs[id]
	return job, ok
}

func (s *memoryStore) Update(id string, res backup.Result) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if job, ok := s.jobs[id]; ok {
		job.State = res.State
		job.Bundle = res
		job.UpdatedAt = time.Now()
	}
}

func (s *memoryStore) ActiveForRepo(repoPath string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, j := range s.jobs {
		if j.RepoPath != repoPath {
			continue
		}
		switch j.State {
		case backup.StateIdle, backup.StateCreating, backup.StateUpload:
			return true
		}
	}
	return false
}
