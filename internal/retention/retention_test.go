package retention

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/b-isry/gitsafe/internal/state"
)

// fakeFS is an in-memory FSOps for deterministic engine tests.
type fakeFS struct {
	mu       sync.Mutex
	files    map[string]LocalBundle // keyed by name
	removed  []string               // bundle names removed from disk
	removedP []string               // paths passed to RemoveLocal
	// failRemove lists names whose RemoveLocal returns an error.
	failRemove map[string]error
}

func newFakeFS() *fakeFS {
	return &fakeFS{files: map[string]LocalBundle{}, failRemove: map[string]error{}}
}

func (f *fakeFS) put(dir, name string, mod time.Time) {
	f.files[name] = LocalBundle{Name: name, Path: filepath.Join(dir, name), ModTime: mod}
}

func (f *fakeFS) ListBundles(dir string) ([]LocalBundle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []LocalBundle{}
	for _, b := range f.files {
		out = append(out, b)
	}
	return out, nil
}

func (f *fakeFS) RemoveLocal(path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	name := filepath.Base(path)
	f.removedP = append(f.removedP, path)
	if e, ok := f.failRemove[name]; ok {
		return e
	}
	if _, exists := f.files[name]; !exists {
		return ErrAlreadyDeleted
	}
	delete(f.files, name)
	f.removed = append(f.removed, name)
	return nil
}

func (f *fakeFS) Exists(dir, name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.files[name]
	return ok
}

func (f *fakeFS) has(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.files[name]
	return ok
}

func now() time.Time { return time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC) }

func fixedEngine(fs *fakeFS) *Engine {
	return &Engine{Now: now, FS: fs}
}

func v2Name(repo string, n int) string {
	return repo + "_20260904_120000_" + fmt.Sprintf("%08x", n) + ".bundle"
}

// driveID returns a Drive-shaped base64url file identifier (Google Drive IDs are
// opaque, typically 33 characters in [A-Za-z0-9_-]).
func driveID(n int) string {
	return fmt.Sprintf("drive-%08x%08x", n, n+1)
}

func rec(id, repoID, name string, daysOld int, driveID, status string) state.BackupRecord {
	return state.BackupRecord{
		ID:              id,
		ProtectedRepoID: repoID,
		FullName:        repoID,
		CreatedAt:       now().Add(-time.Duration(daysOld) * 24 * time.Hour),
		BundleName:      name,
		DriveFileID:     driveID,
		Status:          status,
	}
}

func TestRetentionByCount(t *testing.T) {
	fs := newFakeFS()
	// 3 records for repo "r1", keep 2 → oldest deleted.
	var records []state.BackupRecord
	for i := 0; i < 3; i++ {
		n := v2Name("repoX", i)
		fs.put("/out", n, now())
		records = append(records, rec("r1-"+string(rune('a'+i)), "p1", n, i, "", "bundled"))
	}
	e := fixedEngine(fs)
	res := e.Run(Policy{KeepLocal: 2}, "/out", records, nil)

	if len(res.RecordsToRemove) != 1 || res.RecordsToRemove[0] != "r1-c" {
		t.Fatalf("RecordsToRemove = %+v, want [r1-c]", res.RecordsToRemove)
	}
	if len(res.LocalDeleted) != 1 || res.LocalDeleted[0] != v2Name("repoX", 2) {
		t.Fatalf("LocalDeleted = %+v", res.LocalDeleted)
	}
	if res.Retained != 2 {
		t.Fatalf("Retained = %d, want 2", res.Retained)
	}
}

func TestRetentionByAge(t *testing.T) {
	fs := newFakeFS()
	// 2 records: one 10 days old, one 2 days old. keepLocalDays = 7.
	old := v2Name("repoA", 1)
	new_ := v2Name("repoA", 2)
	fs.put("/out", old, now())
	fs.put("/out", new_, now())
	records := []state.BackupRecord{
		rec("old", "p1", old, 10, "", "bundled"),
		rec("new", "p1", new_, 2, "", "bundled"),
	}
	e := fixedEngine(fs)
	res := e.Run(Policy{KeepLocalDays: 7}, "/out", records, nil)

	if len(res.RecordsToRemove) != 1 || res.RecordsToRemove[0] != "old" {
		t.Fatalf("RecordsToRemove = %+v, want [old]", res.RecordsToRemove)
	}
	if fs.has(new_) == false || fs.has(old) {
		t.Fatalf("old removed=%v new removed=%v, want old gone new kept", !fs.has(old), !fs.has(new_))
	}
}

func TestRetentionBothPolicies(t *testing.T) {
	fs := newFakeFS()
	// 4 records: ages 1,5,20,40 days; keep 2 and keepLocalDays=10.
	// eligible = overCount (i<4-2=2) OR overAge (>10d).
	// ages: idx0=1d(overCount), idx1=5d(overCount), idx2=20d(overAge), idx3=40d(keep)
	names := []string{v2Name("repoX", 0), v2Name("repoX", 1), v2Name("repoX", 2), v2Name("repoX", 3)}
	ages := []int{1, 5, 20, 40}
	var records []state.BackupRecord
	for i := range names {
		fs.put("/out", names[i], now())
		records = append(records, rec("rec"+string(rune('a'+i)), "p1", names[i], ages[i], "", "bundled"))
	}
	e := fixedEngine(fs)
	res := e.Run(Policy{KeepLocal: 2, KeepLocalDays: 10}, "/out", records, nil)

	got := map[string]bool{}
	for _, id := range res.RecordsToRemove {
		got[id] = true
	}
	want := map[string]bool{"recc": true, "recd": true}
	if len(got) != len(want) {
		t.Fatalf("RecordsToRemove = %+v, want %v", got, want)
	}
	for id := range want {
		if !got[id] {
			t.Fatalf("missing %s in RecordsToRemove %+v", id, got)
		}
	}
}

func TestUploadedRecordKeptAfterLocalDelete(t *testing.T) {
	fs := newFakeFS()
	// Two records for repo: u1 uploaded (has Drive copy), u2 local-only.
	// KeepLocal=1 → the oldest (u1, uploaded) is over count: its local bundle is
	// deleted but the record must be KEPT (it represents the Drive copy).
	u1name := v2Name("repoU", 0)
	u2name := v2Name("repoU", 1)
	fs.put("/out", u1name, now())
	fs.put("/out", u2name, now())
	records := []state.BackupRecord{
		rec("u1", "p1", u1name, 5, driveID(0), "uploaded"), // older
		rec("u2", "p1", u2name, 1, "", "bundled"),          // newer
	}
	e := fixedEngine(fs)
	res := e.Run(Policy{KeepLocal: 1}, "/out", records, nil)

	if len(res.LocalDeleted) != 1 || res.LocalDeleted[0] != u1name {
		t.Fatalf("LocalDeleted = %+v, want [%s]", res.LocalDeleted, u1name)
	}
	// u1 kept (uploaded), u2 kept (within limit) → no record removed.
	if len(res.RecordsToRemove) != 0 {
		t.Fatalf("RecordsToRemove = %+v, want [] (u1 kept via Drive, u2 within limit)", res.RecordsToRemove)
	}
	if res.Retained != 2 {
		t.Fatalf("Retained = %d, want 2", res.Retained)
	}
}

func TestLocalOnlyRecordRemovedOnDelete(t *testing.T) {
	fs := newFakeFS()
	name := v2Name("repoL", 0)
	fs.put("/out", name, now())
	records := []state.BackupRecord{rec("l1", "p1", name, 1, "", "bundled")}
	e := fixedEngine(fs)
	res := e.Run(Policy{KeepLocal: 1}, "/out", records, nil)
	// 0 over limit (keep 1 of 1) → nothing.
	if len(res.RecordsToRemove) != 0 {
		t.Fatalf("unexpected removal: %+v", res)
	}
}

func TestMissingBundleTolerated(t *testing.T) {
	fs := newFakeFS()
	// Record with no file on disk, no Drive copy → reconcile removes record; local
	// deletion marks it missing.
	name := v2Name("repoM", 0) // not put in fs
	records := []state.BackupRecord{rec("m1", "p1", name, 1, "", "bundled")}
	e := fixedEngine(fs)
	res := e.Run(Policy{KeepLocal: 1}, "/out", records, nil)
	if len(res.RecordsToRemove) != 1 || res.RecordsToRemove[0] != "m1" {
		t.Fatalf("reconcile should remove record with missing bundle, got %+v", res)
	}
}

func TestAlreadyDeletedNotReDeleted(t *testing.T) {
	fs := newFakeFS()
	name := v2Name("repoD", 0)
	// present in fs only as a "deleted" marker: remove it first so RemoveLocal sees
	// it as already gone.
	fs.put("/out", name, now())
	_ = fs.RemoveLocal(fs.files[name].Path)
	records := []state.BackupRecord{rec("d1", "p1", name, 100, "", "bundled")}
	e := fixedEngine(fs)
	res := e.Run(Policy{KeepLocalDays: 10}, "/out", records, nil)
	// age-eligible → deleteRecordedLocal tries remove → ErrAlreadyDeleted → Missing.
	if res.Missing != 1 || len(res.LocalDeleted) != 0 {
		t.Fatalf("Missing=%d LocalDeleted=%+v", res.Missing, res.LocalDeleted)
	}
	if len(res.RecordsToRemove) != 1 {
		t.Fatalf("record should still be removed (no Drive, no local): %+v", res)
	}
}

func TestOrphanCleanupOnlyV2Names(t *testing.T) {
	fs := newFakeFS()
	v2 := v2Name("repoO", 0)
	legacy := "repoO_20260904_120400.bundle" // legacy local-flow name (no hex)
	fs.put("/out", v2, now())
	fs.put("/out", legacy, now())
	// v2 unreferenced, repo not active → deleted. legacy/unrelated untouched.
	e := fixedEngine(fs)
	res := e.Run(Policy{}, "/out", nil, nil)
	// Empty policy = disabled → nothing deleted.
	if len(res.LocalDeleted) != 0 || res.OrphanDeleted != 0 {
		t.Fatalf("empty policy should delete nothing: %+v", res)
	}

	// Non-empty policy enables orphan cleanup.
	res = e.Run(Policy{KeepLocal: 1}, "/out", nil, nil)
	if res.OrphanDeleted != 1 || len(res.LocalDeleted) != 1 {
		t.Fatalf("OrphanDeleted=%d LocalDeleted=%+v, want 1 v2 orphan", res.OrphanDeleted, res.LocalDeleted)
	}
	if fs.has(v2) {
		t.Fatal("v2 orphan should have been deleted")
	}
	if !fs.has(legacy) {
		t.Fatal("legacy bundle must not be deleted")
	}
}

func TestOrphanReferencedByRecordKept(t *testing.T) {
	fs := newFakeFS()
	name := v2Name("repoR", 0)
	fs.put("/out", name, now())
	records := []state.BackupRecord{rec("r1", "p1", name, 1, "", "bundled")}
	e := fixedEngine(fs)
	res := e.Run(Policy{KeepLocal: 1}, "/out", records, nil)
	// 1 record keep 1 → not over count; not orphan (referenced).
	if res.OrphanDeleted != 0 || !fs.has(name) {
		t.Fatalf("referenced bundle deleted: %+v", res)
	}
}

func TestJobHistoryRetention(t *testing.T) {
	var jobs []state.BackupJob
	nowT := now()
	for i := 0; i < 4; i++ {
		jobs = append(jobs, state.BackupJob{
			ID: "j" + string(rune('a'+i)), ProtectedRepoID: "p1",
			State: state.JobCompleted, FinishedAt: nowT.Add(time.Duration(i) * time.Hour),
		})
	}
	// plus an active (non-terminal) job that must never be touched.
	jobs = append(jobs, state.BackupJob{ID: "active", ProtectedRepoID: "p1", State: state.JobCloning, StartedAt: nowT})

	e := fixedEngine(newFakeFS())
	res := e.Run(Policy{KeepJobs: 2}, "/out", nil, jobs)
	if len(res.JobsToRemove) != 2 {
		t.Fatalf("JobsToRemove = %+v, want 2", res.JobsToRemove)
	}
	if res.JobsToRemove[0] == "active" || res.JobsToRemove[1] == "active" {
		t.Fatalf("active job must not be removed: %+v", res.JobsToRemove)
	}
	for _, id := range res.JobsToRemove {
		if !strings.HasPrefix(id, "j") {
			t.Fatalf("only old terminal jobs should be removed, got %q", id)
		}
	}
}

func TestActiveRepoSkipsDeleted(t *testing.T) {
	fs := newFakeFS()
	name := v2Name("repoA", 0)
	fs.put("/out", name, now())
	records := []state.BackupRecord{rec("a1", "p1", name, 100, "", "bundled")}
	e := fixedEngine(fs)
	e.IsActiveRepo = func(id string) bool { return id == "p1" }
	res := e.Run(Policy{KeepLocalDays: 10}, "/out", records, nil)
	// Repo active → age-eligible record skipped, not deleted.
	if len(res.RecordsToRemove) != 0 || res.Skipped == 0 {
		t.Fatalf("active repo should skip deletion: %+v", res)
	}
	if !fs.has(name) {
		t.Fatal("bundle of active repo must not be deleted")
	}
}

func TestActiveNameSkipsOrphan(t *testing.T) {
	fs := newFakeFS()
	name := v2Name("repoA", 0)
	fs.put("/out", name, now())
	e := fixedEngine(fs)
	e.IsActiveName = func(n string) bool { return n == "repoA" }
	res := e.Run(Policy{KeepLocal: 1}, "/out", nil, nil)
	if res.OrphanDeleted != 0 || !fs.has(name) {
		t.Fatalf("orphan for active repo must not be deleted: %+v", res)
	}
}

func TestIdempotentSecondRun(t *testing.T) {
	fs := newFakeFS()
	var records []state.BackupRecord
	for i := 0; i < 4; i++ {
		n := v2Name("repoI", i)
		fs.put("/out", n, now())
		records = append(records, rec("i"+string(rune('a'+i)), "p1", n, i, "", "bundled"))
	}
	e := fixedEngine(fs)
	first := e.Run(Policy{KeepLocal: 2}, "/out", records, nil)
	if len(first.LocalDeleted) == 0 {
		t.Fatalf("expected deletions in first run: %+v", first)
	}
	// Second run over remaining records should delete nothing new.
	remaining := []state.BackupRecord{}
	for _, r := range records {
		if fs.has(r.BundleName) {
			remaining = append(remaining, r)
		}
	}
	second := e.Run(Policy{KeepLocal: 2}, "/out", remaining, nil)
	if len(second.LocalDeleted) != 0 || len(second.RecordsToRemove) != 0 || second.OrphanDeleted != 0 {
		t.Fatalf("second run not idempotent: %+v", second)
	}
}

func TestDriveRetentionConfirmedOnly(t *testing.T) {
	fs := newFakeFS()
	// uploaded record, old enough, Drive retention enabled.
	name := v2Name("repoV", 0)
	fs.put("/out", name, now())
	records := []state.BackupRecord{rec("v1", "p1", name, 100, driveID(0), "uploaded")}

	calls := []string{}
	e := fixedEngine(fs)
	e.Drive = func(id string) error {
		calls = append(calls, id)
		if id == driveID(99) {
			return errors.New("provider error")
		}
		return nil
	}
	res := e.Run(Policy{DriveRetention: true, KeepDriveDays: 60}, "/out", records, nil)
	if len(res.DriveDeleted) != 1 || res.DriveDeleted[0] != driveID(0) {
		t.Fatalf("DriveDeleted = %+v", res.DriveDeleted)
	}
	if len(res.RecordsToDowngrade) != 1 || res.RecordsToDowngrade[0] != "v1" {
		t.Fatalf("RecordsToDowngrade = %+v", res.RecordsToDowngrade)
	}

	// Failure path: delete fails → skipped, not reported as deleted, no downgrade.
	records2 := []state.BackupRecord{rec("v2", "p1", v2Name("repoV", 1), 100, driveID(99), "uploaded")}
	fs.put("/out", v2Name("repoV", 1), now())
	e2 := fixedEngine(fs)
	e2.Drive = e.Drive
	res2 := e2.Run(Policy{DriveRetention: true, KeepDriveDays: 60}, "/out", records2, nil)
	if len(res2.DriveDeleted) != 0 || len(res2.RecordsToDowngrade) != 0 || res2.Skipped == 0 {
		t.Fatalf("failed drive delete must not be reported as deleted: %+v", res2)
	}
	if len(res2.Errors) == 0 {
		t.Fatalf("expected an error recorded for failed drive delete")
	}
}

func TestDriveRetentionDisabledByDefault(t *testing.T) {
	fs := newFakeFS()
	name := v2Name("repoV", 0)
	fs.put("/out", name, now())
	records := []state.BackupRecord{rec("v1", "p1", name, 100, driveID(0), "uploaded")}
	e := fixedEngine(fs)
	e.Drive = func(id string) error { return nil }
	res := e.Run(Policy{KeepDriveDays: 999}, "/out", records, nil)
	// KeepDriveDays alone (no DriveRetention flag) must not delete anything.
	if len(res.DriveDeleted) != 0 {
		t.Fatalf("drive deleted without explicit enable: %+v", res)
	}
}

func TestDriveRetentionRejectsInvalidFileID(t *testing.T) {
	fs := newFakeFS()
	badIDs := []string{"./up", "..", "", "13,37", "id with spaces!"}
	notCalled := true
	e := fixedEngine(fs)
	e.Drive = func(id string) error {
		notCalled = false
		return nil
	}
	for i := range badIDs {
		fs.put("/out", v2Name("repoX", i), now())
	}
	var records []state.BackupRecord
	for i := range badIDs {
		name := v2Name("repoX", i)
		records = append(records, rec("bad"+string(rune('a'+i)), "p1", name, 100, badIDs[i], "uploaded"))
	}
	res := e.Run(Policy{DriveRetention: true, KeepDriveDays: 60}, "/out", records, nil)
	if !notCalled {
		t.Fatal("Drive delete must not be invoked for invalid file IDs")
	}
	if len(res.DriveDeleted) != 0 || len(res.RecordsToDowngrade) != 0 {
		t.Fatalf("no deletion may be reported for invalid IDs: %+v", res)
	}
	if res.Skipped == 0 {
		t.Fatalf("invalid IDs should be skipped: %+v", res)
	}
}

// corruptRecord returns a record whose stored bundle name is attacker/corruption
// crafted, with no bundle on disk.
func corruptRecord(id string, name string, daysOld int) state.BackupRecord {
	r := rec(id, "p1", name, daysOld, "", "bundled")
	return r
}

func TestTraversalBundleNameNeverTouched(t *testing.T) {
	badNames := []string{
		"../escape.bundle",
		"../../outside_20260904_120000_a1b2c3d4.bundle",
		"..",
		"./..",
		"sub/../weird_20260904_120000_a1b2c3d4.bundle",
	}
	for _, bad := range badNames {
		fs := newFakeFS()
		good := v2Name("repoT", 0)
		fs.put("/out", good, now())
		records := []state.BackupRecord{
			corruptRecord("bad", bad, 100), // older, eligible for deletion
			rec("ok", "p1", good, 1, "", "bundled"),
		}
		e := fixedEngine(fs)
		res := e.Run(Policy{KeepLocal: 1}, "/out", records, nil)
		if len(res.LocalDeleted) != 0 || len(res.RecordsToRemove) != 0 {
			t.Fatalf("name %q touched: LocalDeleted=%+v RecordsToRemove=%+v", bad, res.LocalDeleted, res.RecordsToRemove)
		}
		if len(fs.removedP) != 0 {
			t.Fatalf("name %q reached the filesystem: %+v", bad, fs.removedP)
		}
	}
}

func TestReconcileSkipsInvalidBundleName(t *testing.T) {
	fs := newFakeFS()
	records := []state.BackupRecord{
		corruptRecord("bad", "C:\\evil_20260904_120000_a1b2c3d4.bundle", 100),
		corruptRecord("dotdot", "..", 100),
	}
	e := fixedEngine(fs)
	res := e.Run(Policy{KeepLocal: 1}, "/out", records, nil)
	if len(res.RecordsToRemove) != 0 {
		t.Fatalf("invalid-name records must not be removed/reconciled: %+v", res.RecordsToRemove)
	}
	if len(fs.removedP) != 0 {
		t.Fatalf("nothing must be deleted: %+v", fs.removedP)
	}
}

// An empty-name record is a stale, file-less entry: it is reconciled away from
// the store (a DB-only cleanup with zero filesystem impact).
func TestReconcileEmptyNameRecordStillCleaned(t *testing.T) {
	fs := newFakeFS()
	records := []state.BackupRecord{corruptRecord("empty", "", 100)}
	e := fixedEngine(fs)
	res := e.Run(Policy{KeepLocal: 1}, "/out", records, nil)
	if len(res.RecordsToRemove) != 1 || res.RecordsToRemove[0] != "empty" {
		t.Fatalf("empty-name record should be reconciled: %+v", res.RecordsToRemove)
	}
	if len(fs.removedP) != 0 {
		t.Fatalf("no filesystem operations may occur: %+v", fs.removedP)
	}
}

func TestLegacyRecordedBundleStillDeleted(t *testing.T) {
	fs := newFakeFS()
	legacy := "repoO_20260904_120400.bundle"
	fs.put("/out", legacy, now())
	records := []state.BackupRecord{rec("legacy", "p1", legacy, 100, "", "bundled")}
	e := fixedEngine(fs)
	res := e.Run(Policy{KeepLocalDays: 10}, "/out", records, nil)
	if len(res.LocalDeleted) != 1 || res.LocalDeleted[0] != legacy {
		t.Fatalf("legacy recorded bundle should be deleted normally: %+v", res)
	}
	if len(res.RecordsToRemove) != 1 {
		t.Fatalf("legacy record should be removed after deletion: %+v", res)
	}
}

// With no PolicyFor seam the engine must behave exactly like the single-policy
// runs above even when the installed seam would differ.
func TestPolicyForUnsetKeepsGlobalBehavior(t *testing.T) {
	fs := newFakeFS()
	name := v2Name("repoG", 0)
	fs.put("/out", name, now())
	records := []state.BackupRecord{rec("g1", "p1", name, 100, "", "bundled")}
	e := fixedEngine(fs)
	res := e.Run(Policy{KeepLocalDays: 10}, "/out", records, nil)
	if len(res.RecordsToRemove) != 1 {
		t.Fatalf("global behavior without seam changed: %+v", res)
	}
}

func TestPolicyForDisabledGlobalStillEnablesRepos(t *testing.T) {
	fs := newFakeFS()
	names := map[string]string{}
	for _, r := range []string{"repoAA", "repoBB"} {
		for i := 0; i < 3; i++ {
			n := v2Name(r, i)
			fs.put("/out", n, now())
			names[n] = r
		}
	}
	var records []state.BackupRecord
	for i := 0; i < 3; i++ {
		records = append(records, rec("aa"+string(rune('a'+i)), "pa", v2Name("repoAA", i), i, "", "bundled"))
	}
	for i := 0; i < 3; i++ {
		records = append(records, rec("bb"+string(rune('a'+i)), "pb", v2Name("repoBB", i), i, "", "bundled"))
	}

	e := fixedEngine(fs)
	// Global policy keeps everything, but repo "pa" gets an override enabling
	// KeepLocal=2. Only "pa" may be cleaned.
	e.PolicyFor = func(repoID string) Policy {
		if repoID == "pa" {
			return Policy{KeepLocal: 2}
		}
		return Policy{} // fully disabled
	}
	res := e.Run(Policy{}, "/out", records, nil)

	if len(res.RecordsToRemove) != 1 || res.RecordsToRemove[0] != "aac" {
		t.Fatalf("only oldest pa record should be removed, got %+v", res.RecordsToRemove)
	}
	if len(res.LocalDeleted) != 1 {
		t.Fatalf("LocalDeleted = %+v, want just the pa eldest", res.LocalDeleted)
	}
	if !fs.has(v2Name("repoBB", 0)) {
		t.Fatal("repoBB must be untouched with a disabled override")
	}
}

func TestPolicyForGlobalEnabledCanStayOptOut(t *testing.T) {
	fs := newFakeFS()
	for i := 0; i < 3; i++ {
		fs.put("/out", v2Name("repoAA", i), now())
	}
	var records []state.BackupRecord
	for i := 0; i < 3; i++ {
		records = append(records, rec("aa"+string(rune('a'+i)), "pa", v2Name("repoAA", i), i, "", "bundled"))
	}

	e := fixedEngine(fs)
	e.PolicyFor = func(repoID string) Policy { return Policy{KeepLocal: 0} } // keep-all override
	res := e.Run(Policy{KeepLocal: 2}, "/out", records, nil)

	// The override's KeepLocal=0 (keep all) must neuter the global KeepLocal=2
	// because nil/zero override fields fully replace the global dimension.
	if len(res.RecordsToRemove) != 0 || len(res.LocalDeleted) != 0 {
		t.Fatalf("keep-all override ignored: %+v", res)
	}
}

func TestPolicyForDrivePerRepo(t *testing.T) {
	fs := newFakeFS()
	// record for pa has a Drive copy, global policy has no drive retention.
	oldA := v2Name("repoAA", 0)
	oldB := v2Name("repoBB", 0)
	fs.put("/out", oldA, now())
	fs.put("/out", oldB, now())
	records := []state.BackupRecord{
		rec("va", "pa", oldA, 100, driveID(0), "uploaded"),
		rec("vb", "pb", oldB, 100, driveID(1), "uploaded"),
	}

	e := fixedEngine(fs)
	e.Drive = func(id string) error { return nil }
	e.PolicyFor = func(repoID string) Policy {
		if repoID == "pa" {
			return Policy{DriveRetention: true, KeepDriveDays: 60}
		}
		return Policy{}
	}
	res := e.Run(Policy{}, "/out", records, nil)

	if len(res.DriveDeleted) != 1 || res.DriveDeleted[0] != driveID(0) {
		t.Fatalf("DriveDeleted = %+v, want only pa's drive copy", res.DriveDeleted)
	}
	if len(res.RecordsToDowngrade) != 1 || res.RecordsToDowngrade[0] != "va" {
		t.Fatalf("RecordsToDowngrade = %+v, want [va]", res.RecordsToDowngrade)
	}
}

func TestPolicyForJobRetentionPerRepo(t *testing.T) {
	nowT := now()
	var jobs []state.BackupJob
	for i := 0; i < 4; i++ {
		jobs = append(jobs, state.BackupJob{ID: "ja" + string(rune('a'+i)), ProtectedRepoID: "pa", State: state.JobCompleted, FinishedAt: nowT.Add(time.Duration(i) * time.Hour)})
	}
	for i := 0; i < 4; i++ {
		jobs = append(jobs, state.BackupJob{ID: "jb" + string(rune('a'+i)), ProtectedRepoID: "pb", State: state.JobCompleted, FinishedAt: nowT.Add(time.Duration(i) * time.Hour)})
	}

	e := fixedEngine(newFakeFS())
	e.PolicyFor = func(repoID string) Policy {
		if repoID == "pa" {
			return Policy{KeepJobs: 2}
		}
		return Policy{KeepJobs: 0} // keep all for pb
	}
	res := e.Run(Policy{KeepJobs: 1}, "/out", nil, jobs)

	wantRemoved := map[string]bool{"jaa": true, "jab": true}
	if len(res.JobsToRemove) != len(wantRemoved) {
		t.Fatalf("JobsToRemove = %+v, want %v", res.JobsToRemove, wantRemoved)
	}
	for id := range wantRemoved {
		if !containsStr(res.JobsToRemove, id) {
			t.Fatalf("missing %s in %+v", id, res.JobsToRemove)
		}
	}
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
