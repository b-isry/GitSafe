// Package retention implements a deterministic, idempotent, concurrency-aware
// retention and cleanup engine over GitSafe's local backup bundles, backup
// records, backup jobs, orphaned artifacts, and (optionally) Google Drive
// copies.
//
// The engine is deliberately opt-in: with the default policy (all fields zero)
// nothing is ever deleted. It is also conservative: local retention never
// deletes a bundle whose Drive copy is the only remaining copy unless an
// explicit Drive policy removes that copy, orphan cleanup only targets files
// matching GitSafe's protected-bundle naming convention, and any deletion is
// skipped for a repository that currently has an unfinished backup job.
//
// The engine performs the concrete side effects it owns (local file deletion at
// a path it was given, and confirmed Drive deletion) and reports the state-store
// mutations the caller must persist (which records to remove or downgrade, which
// jobs to remove). This keeps the engine pure and testable while the caller owns
// the atomic state-store write.
package retention

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/b-isry/gitsafe/internal/config"
	"github.com/b-isry/gitsafe/internal/state"
)

// Policy is the retention configuration applied by the engine.
type Policy = config.RetentionConfig

// LocalBundle is a bundle file discovered in the output directory.
type LocalBundle struct {
	// Name is the bundle filename (basename).
	Name string
	// Path is the full path to the bundle file.
	Path string
	// ModTime is the file modification time.
	ModTime time.Time
}

// FSOps provides the filesystem operations the engine needs, injectable for
// tests.
type FSOps interface {
	// ListBundles returns every *.bundle file under dir. It should return an
	// empty slice (nil error) when dir is missing or empty.
	ListBundles(dir string) ([]LocalBundle, error)
	// RemoveLocal deletes a local bundle file by full path. It must tolerate an
	// already-deleted file (returning ErrAlreadyDeleted).
	RemoveLocal(path string) error
	// Exists reports whether a named bundle file is present in dir.
	Exists(dir, name string) bool
}

// DriveDeleter removes a Drive file by ID, returning nil only when the provider
// confirms the deletion.
type DriveDeleter func(fileID string) error

// IsActiveRepo reports whether a repository (by its protected-repo ID) has an
// unfinished backup job.
type IsActiveRepo func(repoID string) bool

// IsActiveName reports whether a repository (by its bare name, as derived from a
// bundle filename prefix) has an unfinished backup job.
type IsActiveName func(name string) bool

// Engine runs retention cleanup for one output directory.
type Engine struct {
	Now          func() time.Time
	FS           FSOps
	Drive        DriveDeleter
	IsActiveRepo IsActiveRepo
	IsActiveName IsActiveName

	// PolicyFor resolves the effective retention policy for a single repository.
	// nil means every repository uses the single policy passed to Run (existing
	// behavior preserved exactly). It is the additive seam used to layer
	// per-repository overrides: the resolver is consulted per repository group /
	// per record, and a repository whose resolved policy is fully disabled is
	// simply untouched.
	PolicyFor func(repoID string) Policy
}

// Result summarises one cleanup run and carries the exact state mutations the
// caller must persist atomically afterwards.
type Result struct {
	Inspected int `json:"inspected"`
	// Retained is the number of records that remain in the store after this run.
	Retained int `json:"retained"`
	// LocalDeleted lists bundle filenames removed from disk.
	LocalDeleted []string `json:"localBundles,omitempty"`
	// DriveDeleted lists Drive file IDs removed (confirmed).
	DriveDeleted []string `json:"driveFileIds,omitempty"`
	// OrphanDeleted counts v2-name orphan bundles removed.
	OrphanDeleted int `json:"orphanDeleted"`
	Skipped       int `json:"skipped"`
	Missing       int `json:"missing"`
	// RecordsToRemove lists backup-record IDs the caller should delete from state.
	RecordsToRemove []string `json:"-"`
	// RecordsToDowngrade lists backup-record IDs whose Drive copy was removed;
	// the caller should clear DriveFileID and set Status to bundled.
	RecordsToDowngrade []string `json:"-"`
	// JobsToRemove lists job IDs the caller should delete from state.
	JobsToRemove []string `json:"-"`
	Errors       []string `json:"errors,omitempty"`
}

func (r *Result) recordErr(format string, args ...any) {
	r.Errors = append(r.Errors, fmt.Sprintf(format, args...))
}

func (r *Result) markRecordRemoved(id string) {
	if id == "" {
		return
	}
	for _, existing := range r.RecordsToRemove {
		if existing == id {
			return
		}
	}
	r.RecordsToRemove = append(r.RecordsToRemove, id)
}

func (r *Result) markRecordDowngrade(id string) {
	if id == "" {
		return
	}
	for _, existing := range r.RecordsToDowngrade {
		if existing == id {
			return
		}
	}
	r.RecordsToDowngrade = append(r.RecordsToDowngrade, id)
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

// policyDisabled reports whether a fully zeroed policy (which would also make
// the top-level Run gate skip a repo) is in effect.
func policyDisabled(p Policy) bool {
	return p.KeepLocal <= 0 && p.KeepLocalDays <= 0 && p.KeepJobs <= 0 && !p.DriveRetention
}

// resolvePolicy returns the effective policy for a repository: the per-repo
// override value when the seam is installed, otherwise the global policy.
func (e *Engine) resolvePolicy(p Policy, repoID string) Policy {
	if e.PolicyFor == nil {
		return p
	}
	return e.PolicyFor(repoID)
}

// Run executes retention cleanup over the recorded histories and the local
// output directory under the given policy, returning a Result. records and jobs
// are snapshots supplied by the caller; the caller persists the mutations the
// Result describes (see Result fields) and must do so atomically afterwards.
//
// When PolicyFor is nil (the default) the behavior is exactly the single-policy
// run. When it is set, each repository is evaluated under its own resolved
// policy; repositories with a fully-disabled effective policy are untouched.
// Global-only aspects (orphan cleanup and record reconciliation) stay governed
// by the global policy so an override can never widen what a disabled install
// deletes.
func (e *Engine) Run(p Policy, outputDir string, records []state.BackupRecord, jobs []state.BackupJob) Result {
	res := Result{Inspected: len(records)}
	globalDisabled := policyDisabled(p)

	// Local/lifecycle cleanup runs when the global policy is enabled OR when a
	// per-repo override could enable it for some repository.
	if !globalDisabled || e.PolicyFor != nil {
		e.retainJobs(p, jobs, &res)
		e.retainLocalRecords(p, outputDir, records, &res)
		e.retainDrive(p, records, &res)
	}

	// Global-only aspects.
	if !globalDisabled {
		e.cleanupOrphans(p, outputDir, records, &res)
		e.reconcileRecords(outputDir, records, &res)
	}

	res.Retained = res.Inspected - len(res.RecordsToRemove)
	if res.Retained < 0 {
		res.Retained = 0
	}
	return res
}

// retainJobs keeps the newest KeepJobs terminal jobs per repo, recording the
// excess job IDs to remove. Non-terminal (active) jobs are never considered.
func (e *Engine) retainJobs(p Policy, jobs []state.BackupJob, res *Result) {
	byRepo := map[string][]state.BackupJob{}
	for _, j := range jobs {
		if !state.IsTerminalState(j.State) {
			continue
		}
		byRepo[j.ProtectedRepoID] = append(byRepo[j.ProtectedRepoID], j)
	}
	for repoID, group := range byRepo {
		pol := e.resolvePolicy(p, repoID)
		if pol.KeepJobs <= 0 {
			continue
		}
		sort.Slice(group, func(i, j int) bool { return jobTime(group[i]).Before(jobTime(group[j])) })
		if len(group) <= pol.KeepJobs {
			continue
		}
		for _, j := range group[:len(group)-pol.KeepJobs] {
			res.JobsToRemove = append(res.JobsToRemove, j.ID)
		}
	}
}

func jobTime(j state.BackupJob) time.Time {
	if !j.FinishedAt.IsZero() {
		return j.FinishedAt
	}
	return j.StartedAt
}

// retainLocalRecords decides, per record, whether to delete its local bundle
// (and remove the record when no Drive copy remains). Records for an active repo
// are skipped.
func (e *Engine) retainLocalRecords(p Policy, outputDir string, records []state.BackupRecord, res *Result) {
	grouped := map[string][]state.BackupRecord{}
	for _, rec := range records {
		grouped[rec.ProtectedRepoID] = append(grouped[rec.ProtectedRepoID], rec)
	}
	for repoID, group := range grouped {
		pol := e.resolvePolicy(p, repoID)
		if policyDisabled(pol) {
			continue
		}
		sort.Slice(group, func(i, j int) bool { return group[i].CreatedAt.Before(group[j].CreatedAt) })
		for i, rec := range group {
			byCount := pol.KeepLocal > 0 && i < len(group)-pol.KeepLocal
			byAge := pol.KeepLocalDays > 0 && !rec.CreatedAt.IsZero() &&
				e.now().Sub(rec.CreatedAt) > time.Duration(pol.KeepLocalDays)*24*time.Hour
			if !byCount && !byAge || rec.BundleName == "" {
				continue
			}
			if e.IsActiveRepo != nil && e.IsActiveRepo(repoID) {
				res.Skipped++
				continue
			}
			e.deleteRecordedLocal(rec, outputDir, res)
		}
	}
}

// deleteRecordedLocal removes a recorded bundle's local file. If the record has
// no Drive copy it is marked for removal too; if a Drive copy remains, the
// record is kept to represent that remote backup.
func (e *Engine) deleteRecordedLocal(rec state.BackupRecord, outputDir string, res *Result) {
	if !validRecordedName(rec.BundleName) {
		res.Skipped++
		res.recordErr("skip invalid recorded bundle name %q", rec.BundleName)
		return
	}
	if e.removeLocalIfPresent(filepath.Join(outputDir, rec.BundleName), res) {
		res.LocalDeleted = append(res.LocalDeleted, rec.BundleName)
	}
	if rec.DriveFileID == "" {
		res.markRecordRemoved(rec.ID)
	}
}

// retainDrive removes Drive copies older than KeepDriveDays under each record's
// effective policy. Deletion is reported only when the provider confirms
// success; failures are counted as skipped.
func (e *Engine) retainDrive(p Policy, records []state.BackupRecord, res *Result) {
	if e.Drive == nil {
		return
	}
	for _, rec := range records {
		pol := e.resolvePolicy(p, rec.ProtectedRepoID)
		if !pol.DriveRetention || pol.KeepDriveDays <= 0 ||
			rec.DriveFileID == "" || rec.CreatedAt.IsZero() ||
			e.now().Sub(rec.CreatedAt) <= time.Duration(pol.KeepDriveDays)*24*time.Hour {
			continue
		}
		if e.IsActiveRepo != nil && e.IsActiveRepo(rec.ProtectedRepoID) {
			res.Skipped++
			continue
		}
		if !validDriveFileID(rec.DriveFileID) {
			res.Skipped++
			res.recordErr("skip invalid drive file id %q", rec.DriveFileID)
			continue
		}
		if err := e.Drive(rec.DriveFileID); err != nil {
			res.Skipped++
			res.recordErr("delete drive file %q: %v", rec.DriveFileID, err)
			continue
		}
		res.DriveDeleted = append(res.DriveDeleted, rec.DriveFileID)
		res.markRecordDowngrade(rec.ID)
	}
}

// v2BundlePattern matches the GitSafe protected-bundle filenames produced by
// archiver.BundleRemoteRepo: <prefix>_<YYYYMMDD_HHMMSS>_<8 hex>.bundle. Only
// files matching this exact convention are ever considered for orphan cleanup,
// so legacy/unknown bundles and unrelated files are never touched.
var v2BundlePattern = regexp.MustCompile(`^(.+)_\d{8}_\d{6}_[0-9a-f]{8}\.bundle$`)

// recordedBundlePattern matches every bundle filename GitSafe has ever recorded
// (both the legacy local flow <repo>_<YYYYMMDD_HHMMSS>.bundle and the protected
// v2 form). It is used as a hard ownership check before any recorded path is
// deleted or even probed, so a corrupt or malicious state entry containing a
// path ("../…", absolutes, drive-rooted names) can never make cleanup reach a
// file outside the output directory: such entries are reported and skipped.
var recordedBundlePattern = regexp.MustCompile(`^.+_\d{8}_\d{6}(_[0-9a-f]{8})?\.bundle$`)

// validRecordedName reports whether a stored BundleName is a plain basename that
// matches GitSafe's bundle naming convention. Any other value is suspicious and
// must never be fed to the filesystem.
func validRecordedName(name string) bool {
	return name != "" && name != "." && name != ".." && filepath.Base(name) == name && recordedBundlePattern.MatchString(name)
}

// driveFileIDPattern matches the opaque base64url identifiers Google Drive uses
// for file IDs (typically 33 characters, [A-Za-z0-9_-]). Drive IDs recorded by
// GitSafe always come from the provider itself; anything outside this character
// class is a corrupt or malicious value and must never reach the delete API.
var driveFileIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{10,100}$`)

func validDriveFileID(id string) bool {
	return driveFileIDPattern.MatchString(id)
}

// cleanupOrphans removes bundle files matching the v2 naming convention that are
// not referenced by any record and whose owning repo has no unfinished job.
func (e *Engine) cleanupOrphans(p Policy, outputDir string, records []state.BackupRecord, res *Result) {
	referenced := map[string]bool{}
	for _, rec := range records {
		if rec.BundleName != "" {
			referenced[rec.BundleName] = true
		}
	}
	bundles, err := e.FS.ListBundles(outputDir)
	if err != nil {
		res.recordErr("list bundles in %q: %v", outputDir, err)
		return
	}
	for _, b := range bundles {
		if !v2BundlePattern.MatchString(b.Name) || referenced[b.Name] {
			continue
		}
		name := strings.SplitN(b.Name, "_", 2)[0]
		if e.IsActiveName != nil && e.IsActiveName(name) {
			res.Skipped++
			continue
		}
		if e.removeLocalIfPresent(b.Path, res) {
			res.OrphanDeleted++
			res.LocalDeleted = append(res.LocalDeleted, b.Name)
		}
	}
}

// reconcileRecords marks for removal records that have no Drive copy and whose
// local bundle is no longer present (they represent nothing that still exists).
// Records whose stored BundleName is not a valid basename are left untouched.
func (e *Engine) reconcileRecords(outputDir string, records []state.BackupRecord, res *Result) {
	for _, rec := range records {
		if rec.DriveFileID != "" {
			continue
		}
		if rec.BundleName == "" {
			res.markRecordRemoved(rec.ID)
			continue
		}
		if !validRecordedName(rec.BundleName) {
			continue
		}
		if !e.FS.Exists(outputDir, rec.BundleName) {
			res.markRecordRemoved(rec.ID)
		}
	}
}

func (e *Engine) removeLocalIfPresent(path string, res *Result) bool {
	err := e.FS.RemoveLocal(path)
	if err == nil {
		return true
	}
	if errors.Is(err, ErrAlreadyDeleted) {
		res.Missing++
		return false
	}
	res.recordErr("remove bundle %q: %v", path, err)
	res.Skipped++
	return false
}

// ErrAlreadyDeleted signals a file that was already gone; it is returned by
// FSOps.RemoveLocal so the engine can treat it as "missing" rather than failed.
var ErrAlreadyDeleted = errors.New("file already deleted")
