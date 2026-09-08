// Package state provides a persistent JSON state store for the GitSafe v2
// architecture: GitHub/Drive connections, protected repositories, immutable
// backup history, and in-flight backup jobs.
//
// It is a generic persistence layer. It deliberately knows nothing about HTTP
// handlers, GitHub, Google Drive, or the existing backup implementation.
//
// The store is the source of truth for backup history and protection state. It
// NEVER stores secrets: only token references (keys into a separate encrypted
// token store, added in a later phase). No OAuth token, refresh token, or
// tokenized URL is ever serialized here.
package state

import "time"

// CurrentVersion is the schema version this build reads and writes.
const CurrentVersion = 1

// DefaultPath is the conventional location for the state file, intended for
// Phase 1 wiring. Phase 0 does not wire anything to it.
const DefaultPath = ".gitsafe/state.json"

// Backup job states. These are decoupled from the runtime backup package so the
// store does not depend on it.
const (
	JobEnqueued    = "enqueued"
	JobAuthorizing = "authorizing"
	JobCloning     = "cloning"
	JobBundling    = "bundling"
	JobValidating  = "validating"
	JobUploading   = "uploading"
	JobRecording   = "recording"

	JobCompleted   = "completed"
	JobFailed      = "failed"
	JobInterrupted = "interrupted"
)

// IsRunningState reports whether a job state is non-terminal (that is, it
// represents work that was in progress when the store was persisted).
func IsRunningState(state string) bool {
	return state != JobCompleted && state != JobFailed
}

// IsTerminalState reports whether a job state is terminal (completed or failed).
func IsTerminalState(state string) bool {
	return state == JobCompleted || state == JobFailed
}

// Backup record terminal outcomes recorded on BackupRecord.Status.
const (
	// BackupStatusBundled means the bundle was written locally but not uploaded
	// to Drive (Drive disabled/not configured, or an upload that later failed).
	BackupStatusBundled = "bundled"
	// BackupStatusUploaded means the bundle was written locally and uploaded to
	// Drive; BackupRecord.DriveFileID is set.
	BackupStatusUploaded = "uploaded"
	// BackupStatusFailed means the backup or Drive upload failed.
	BackupStatusFailed = "failed"
)

// document is the single, versioned top-level state document persisted to disk.
type document struct {
	Version        int               `json:"version"`
	GitHub         *GitHubConnection `json:"github"`
	Drive          *DriveConnection  `json:"drive"`
	ProtectedRepos []ProtectedRepo   `json:"protectedRepos"`
	BackupRecords  []BackupRecord    `json:"backupRecords"`
	BackupJobs     []BackupJob       `json:"backupJobs"`
}

// Document is the in-memory state document exposed by a Store. It mirrors the
// persisted shape and is safe to read while holding the store's read lock.
type Document struct {
	Version        int
	GitHub         *GitHubConnection
	Drive          *DriveConnection
	ProtectedRepos []ProtectedRepo
	BackupRecords  []BackupRecord
	BackupJobs     []BackupJob
}

// GitHubConnection represents the user's connected GitHub account. It holds a
// token reference only — never the token itself.
type GitHubConnection struct {
	// GitHubID is the immutable GitHub numeric user ID.
	GitHubID int64 `json:"githubId"`
	// Login is the GitHub username (display; may change over time).
	Login string `json:"login"`
	// Name is the user's display name, if any.
	Name string `json:"name,omitempty"`
	// AvatarURL is the user's avatar URL, if any.
	AvatarURL string `json:"avatarUrl,omitempty"`
	// Scopes are the granted OAuth scopes.
	Scopes []string `json:"scopes,omitempty"`
	// ConnectedAt is when the connection was established.
	ConnectedAt time.Time `json:"connectedAt"`
	// TokenRef is a key into the encrypted token store. MUST NOT be the token.
	TokenRef string `json:"tokenRef"`
}

// DriveConnection represents the connected Google Drive account. It holds a
// token reference only — never the token itself.
type DriveConnection struct {
	// AccountEmail identifies the connected Google account.
	AccountEmail string `json:"accountEmail"`
	// ConnectedAt is when the connection was established.
	ConnectedAt time.Time `json:"connectedAt"`
	// StorageFolderID is the GitSafe folder created in the user's Drive.
	StorageFolderID string `json:"storageFolderId"`
	// TokenRef is a key into the encrypted token store. MUST NOT be the token.
	TokenRef string `json:"tokenRef"`
}

// ProtectedRepo is a repository the user has chosen to protect. It is
// identified by GitHub repository identity (githubId + fullName). A local
// filesystem path is NEVER part of this model.
type ProtectedRepo struct {
	// ID is a local UUID identifying this protected-repository row.
	ID string `json:"id"`
	// GitHubID is the immutable GitHub numeric repository ID.
	GitHubID int64 `json:"githubId"`
	// FullName is the current owner/name, refreshed on rename by discovery.
	FullName string `json:"fullName"`
	// DefaultBranch is the repository's default branch at protect time.
	DefaultBranch string `json:"defaultBranch"`
	// AddedAt is when the repository was protected.
	AddedAt time.Time `json:"addedAt"`
}

// BackupRecord is an immutable record of a single completed backup. It must not
// contain a local repository path, tokens, or a tokenized clone URL.
type BackupRecord struct {
	// ID is a local UUID for this record.
	ID string `json:"id"`
	// ProtectedRepoID references the protected repository this history belongs to.
	ProtectedRepoID string `json:"protectedRepoId"`
	// FullName is a snapshot of the repository name at backup time.
	FullName string `json:"fullName"`
	// CreatedAt is when the backup record was created.
	CreatedAt time.Time `json:"createdAt"`
	// MirroredAt is the GitHub repository state the backup represents.
	MirroredAt time.Time `json:"mirroredAt"`
	// HeadSHAs maps branch/ref names to their SHAs at backup time.
	HeadSHAs map[string]string `json:"headSHAs,omitempty"`
	// DefaultBranch is the default branch of the repository at backup time.
	DefaultBranch string `json:"defaultBranch"`
	// DefaultHead is the SHA of the default branch at backup time.
	DefaultHead string `json:"defaultHead"`
	// BranchCount is the number of branches captured in the bundle.
	BranchCount int `json:"branchCount"`
	// TagCount is the number of tags captured in the bundle.
	TagCount int `json:"tagCount"`
	// BundleName is the portable bundle filename.
	BundleName string `json:"bundleName"`
	// BundleSize is the bundle size in bytes.
	BundleSize int64 `json:"bundleSize"`
	// BundleSHA256 is the content hash of the bundle, for integrity checks.
	BundleSHA256 string `json:"bundleSHA256"`
	// DriveFileID points to the bundle stored in the user's Drive.
	DriveFileID string `json:"driveFileId"`
	// Status is the terminal backup outcome ("uploaded" or "failed").
	Status string `json:"status"`
	// GitVersion is the git toolchain version used to create the bundle.
	GitVersion string `json:"gitVersion"`
}

// BackupJob tracks an in-flight or previously-run backup. Only enough that is
// safe and sufficient to detect work interrupted by a shutdown is persisted.
// The temporary staging directory and any credentials/URLs are runtime-only and
// NEVER stored here.
type BackupJob struct {
	// ID is the job identifier used for polling.
	ID string `json:"id"`
	// ProtectedRepoID references the protected repository being backed up.
	ProtectedRepoID string `json:"protectedRepoId"`
	// FullName is the repository name for this job.
	FullName string `json:"fullName"`
	// State is the job lifecycle state.
	State string `json:"state"`
	// StartedAt is when the job started.
	StartedAt time.Time `json:"startedAt"`
	// FinishedAt is when the job reached a terminal state, if it did.
	FinishedAt time.Time `json:"finishedAt,omitempty"`
	// Error is a sanitized error message for failed/interrupted jobs.
	Error string `json:"error,omitempty"`
}
