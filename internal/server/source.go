package server

import "time"

// View models rendered by templates. They are returned by App and are kept
// separate from the storage-oriented types.

type DashboardStats struct {
	ReposScanned int
	Stale        int
	Backups      int
	Attention    int
	LastScan     time.Time
}

type Repository struct {
	ID           string
	Name         string
	Path         string
	Source       string
	LastCommit   time.Time
	StaleDays    int
	Stale        bool
	BackupStatus string
}

type BackupRow struct {
	Repo      string
	Filename  string
	SizeBytes int64
	Created   time.Time
	Uploaded  bool
}

type RepositoryDetail struct {
	Repository Repository
	Threshold  int
	Backups    []BackupRow
}

type Dashboard struct {
	Stats   DashboardStats
	Repos   []Repository
	Backups []BackupRow
}
