package state

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func testGitHub() GitHubConnection {
	return GitHubConnection{
		GitHubID:    42,
		Login:       "octocat",
		Name:        "The Octocat",
		AvatarURL:   "https://avatars.example/octocat.png",
		Scopes:      []string{"repo"},
		ConnectedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		TokenRef:    "github.primary",
	}
}

func testDrive() DriveConnection {
	return DriveConnection{
		AccountEmail:    "user@gmail.com",
		ConnectedAt:     time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC),
		StorageFolderID: "drive-folder-123",
		TokenRef:        "drive.primary",
	}
}

func testRepo(id int64, fullName string) ProtectedRepo {
	return ProtectedRepo{
		ID:            "repo-" + fullName,
		GitHubID:      id,
		FullName:      fullName,
		DefaultBranch: "main",
		AddedAt:       time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC),
	}
}

func testRecord(repoID string) BackupRecord {
	return BackupRecord{
		ID:              "rec-1",
		ProtectedRepoID: repoID,
		FullName:        "octocat/hello",
		CreatedAt:       time.Date(2026, 4, 5, 6, 7, 8, 0, time.UTC),
		MirroredAt:      time.Date(2026, 4, 5, 6, 6, 0, 0, time.UTC),
		HeadSHAs:        map[string]string{"main": "abc123", "dev": "def456"},
		DefaultBranch:   "main",
		DefaultHead:     "abc123",
		BranchCount:     2,
		TagCount:        1,
		BundleName:      "octocat_hello_20260405_060708.bundle",
		BundleSize:      4096,
		BundleSHA256:    "sha256hex",
		DriveFileID:     "drivefile-1",
		Status:          "uploaded",
		GitVersion:      "2.40.0",
	}
}

func testJob(id, state string) BackupJob {
	return BackupJob{
		ID:              id,
		ProtectedRepoID: "repo-x",
		FullName:        "octocat/x",
		State:           state,
		StartedAt:       time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC),
	}
}

func TestNewStoreInitialization(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "state.json")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open on missing path: %v", err)
	}
	if s.Path() != path {
		t.Errorf("path mismatch: %q != %q", s.Path(), path)
	}
	if _, ok := s.GitHubConnection(); ok {
		t.Errorf("expected no github connection")
	}
	if _, ok := s.DriveConnection(); ok {
		t.Errorf("expected no drive connection")
	}
	if got := s.ProtectedRepos(); len(got) != 0 {
		t.Errorf("expected no protected repos, got %d", len(got))
	}
	if got := s.BackupRecords(); len(got) != 0 {
		t.Errorf("expected no backup records, got %d", len(got))
	}
	if got := s.UnfinishedJobs(); len(got) != 0 {
		t.Errorf("expected no unfinished jobs, got %d", len(got))
	}
	// No file is written until first Save.
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("expected state file absent before first Save")
	}
	if err := s.Save(); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected state file after Save: %v", err)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	g := testGitHub()
	s.SetGitHubConnection(g)
	if err := s.AddProtectedRepo(testRepo(1, "a/b")); err != nil {
		t.Fatalf("AddProtectedRepo: %v", err)
	}
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reload Open: %v", err)
	}
	got, ok := s2.GitHubConnection()
	if !ok || !reflect.DeepEqual(got, g) {
		t.Errorf("github mismatch: %+v ok=%v", got, ok)
	}
	repos := s2.ProtectedRepos()
	if len(repos) != 1 || repos[0].FullName != "a/b" {
		t.Errorf("protected repos mismatch: %+v", repos)
	}
}

func TestMultipleEntitiesPersisted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, _ := Open(path)
	g := testGitHub()
	d := testDrive()
	s.SetGitHubConnection(g)
	s.SetDriveConnection(d)
	repoA := testRepo(1, "a/one")
	repoB := testRepo(2, "b/two")
	if err := s.AddProtectedRepo(repoA); err != nil {
		t.Fatal(err)
	}
	if err := s.AddProtectedRepo(repoB); err != nil {
		t.Fatal(err)
	}
	s.AddBackupRecord(testRecord(repoA.ID))
	s.AddBackupRecord(testRecord(repoB.ID))
	s.CreateBackupJob(testJob("job-1", JobCompleted))
	s.CreateBackupJob(testJob("job-2", JobFailed))
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if g2, ok := s2.GitHubConnection(); !ok || !reflect.DeepEqual(g2, g) {
		t.Errorf("github mismatch: %+v %v", g2, ok)
	}
	if d2, ok := s2.DriveConnection(); !ok || !reflect.DeepEqual(d2, d) {
		t.Errorf("drive mismatch: %+v %v", d2, ok)
	}
	if got := s2.ProtectedRepos(); len(got) != 2 {
		t.Errorf("protected repos: %d", len(got))
	}
	if got := s2.BackupRecords(); len(got) != 2 {
		t.Errorf("backup records: %d", len(got))
	}
	if got := s2.UnfinishedJobs(); len(got) != 0 {
		t.Errorf("expected no unfinished jobs (all terminal), got %d", len(got))
	}
}

func TestBackupJobsAccessors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, _ := Open(path)
	s.CreateBackupJob(testJob("job-1", JobCloning))
	s.CreateBackupJob(testJob("job-2", JobCompleted))

	if got := s.BackupJobs(); len(got) != 2 {
		t.Fatalf("BackupJobs() returned %d jobs, want 2", len(got))
	}
	if j, ok := s.BackupJob("job-1"); !ok || j.State != JobCloning {
		t.Fatalf("BackupJob(job-1) = %+v, %v", j, ok)
	}
	if _, ok := s.BackupJob("missing"); ok {
		t.Fatal("BackupJob(missing) should report not found")
	}
}

func TestBackupJobRemove(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, _ := Open(path)
	s.CreateBackupJob(testJob("job-1", JobEnqueued))
	s.CreateBackupJob(testJob("job-2", JobCompleted))

	if err := s.RemoveBackupJob("job-1"); err != nil {
		t.Fatalf("RemoveBackupJob failed: %v", err)
	}
	if _, ok := s.BackupJob("job-1"); ok {
		t.Fatal("job-1 should have been removed")
	}
	if _, ok := s.BackupJob("job-2"); !ok {
		t.Fatal("job-2 should remain")
	}
	if err := s.RemoveBackupJob("missing"); err == nil {
		t.Fatal("RemoveBackupJob(missing) should error")
	}
}

func TestBackupRecordAddAndList(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, _ := Open(path)
	r1 := testRecord("repo-a")
	r1.ID = "rec-1"
	r2 := testRecord("repo-a")
	r2.ID = "rec-2"
	s.AddBackupRecord(r1)
	s.AddBackupRecord(r2)

	if len(s.BackupRecords()) != 2 {
		t.Fatalf("records = %+v", s.BackupRecords())
	}
}

func TestAtomicReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, _ := Open(path)
	s.SetGitHubConnection(testGitHub())
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}

	// Second save with a changed connection: the file must hold only the new doc.
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	other := testGitHub()
	other.Login = "someone-else"
	s2.SetGitHubConnection(other)
	if err := s2.Save(); err != nil {
		t.Fatal(err)
	}

	// No leftover temp files.
	entries, _ := os.ReadDir(filepath.Dir(path))
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Errorf("leftover temp file: %s", e.Name())
		}
	}

	s3, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	g, ok := s3.GitHubConnection()
	if !ok || g.Login != "someone-else" {
		t.Errorf("expected replaced connection, got %+v", g)
	}
	if len(s3.ProtectedRepos()) != 0 {
		t.Errorf("expected no protected repos in fresh second doc")
	}
}

func TestFailedWritePreservesPreviousState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	s, _ := Open(path)
	s.SetGitHubConnection(testGitHub())
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// Force a write failure: replace the parent directory path with a regular
	// file so MkdirAll fails before anything is written.
	badPath := filepath.Join(dir, "blocked")
	if err := os.WriteFile(badPath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	badParent := filepath.Join(badPath, "sub")
	sBad, err := Open(filepath.Join(badParent, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	sBad.SetGitHubConnection(testGitHub())
	if err := sBad.Save(); err == nil {
		t.Fatalf("expected Save to fail")
	}

	// Prior valid file is untouched.
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(original, after) {
		t.Errorf("prior state was modified after a failed write")
	}

	// No temp residue anywhere.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Errorf("leftover temp file: %s", e.Name())
		}
	}
}

func TestConcurrentAccessSerialized(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, _ := Open(path)

	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := testRepo(int64(100+i), "o/r")
			r.ID = "id-" + r.FullName
			_ = s.AddProtectedRepo(r)
			s.AddBackupRecord(testRecord(r.ID))
			_ = s.Save()
			s.BackupRecordsForRepo(r.ID)
		}(i)
	}
	wg.Wait()

	// Final file is valid JSON and contains every distinct entity.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc document
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("final file is not valid JSON: %v", err)
	}
	if len(doc.ProtectedRepos) != n {
		t.Errorf("expected %d protected repos, got %d", n, len(doc.ProtectedRepos))
	}
	if len(doc.BackupRecords) != n {
		t.Errorf("expected %d backup records, got %d", n, len(doc.BackupRecords))
	}
}

func TestInvalidJSONRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{not json`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); !errors.Is(err, ErrCorruptState) {
		t.Fatalf("expected ErrCorruptState, got %v", err)
	}
	// File untouched.
	data, _ := os.ReadFile(path)
	if string(data) != `{not json` {
		t.Errorf("corrupt file was modified")
	}
}

func TestUnsupportedVersionRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	content := `{"version":999,"github":null,"drive":null,"protectedRepos":[],"backupRecords":[],"backupJobs":[]}`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("expected ErrUnsupportedVersion, got %v", err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != content {
		t.Errorf("unsupported-version file was modified")
	}
}

func TestMissingVersionRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("expected ErrUnsupportedVersion for missing version, got %v", err)
	}
}

func TestStructurallyInvalidStateRejected(t *testing.T) {
	cases := map[string]string{
		"empty file":       ``,
		"null document":    `null`,
		"top-level array":  `[]`,
		"wrong type field": `{"version":1,"githubId":"not-a-number"}`,
		"unknown field":    `{"version":1,"unknown":1,"github":null,"drive":null,"protectedRepos":[],"backupRecords":[],"backupJobs":[]}`,
		"trailing garbage": `{"version":1,"github":null,"drive":null,"protectedRepos":[],"backupRecords":[],"backupJobs":[]} extra`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "state.json")
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(path); !errors.Is(err, ErrCorruptState) {
				t.Fatalf("expected ErrCorruptState, got %v", err)
			}
			data, _ := os.ReadFile(path)
			if string(data) != content {
				t.Errorf("structurally invalid file was modified")
			}
		})
	}
}

func TestRunningJobsBecomeInterrupted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	content := `{
  "version":1,
  "github":null,
  "drive":null,
  "protectedRepos":[],
  "backupRecords":[],
  "backupJobs":[
    {"id":"j-running","protectedRepoId":"r1","fullName":"a/b","state":"cloning","startedAt":"2026-01-01T00:00:00Z"},
    {"id":"j-done","protectedRepoId":"r1","fullName":"a/b","state":"completed","startedAt":"2026-01-01T00:00:00Z","finishedAt":"2026-01-01T00:01:00Z"}
  ]
}`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	j, ok := s.BackupJob("j-running")
	if !ok {
		t.Fatal("expected j-running")
	}
	if j.State != JobInterrupted {
		t.Errorf("expected interrupted, got %q", j.State)
	}
	if j.Error == "" {
		t.Errorf("expected a sanitized error message")
	}
	if j.FinishedAt.IsZero() {
		t.Errorf("expected finishedAt set on interrupted job")
	}
	// Not auto-persisted.
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), `"interrupted"`) {
		t.Errorf("interrupted normalization should not be persisted until Save")
	}
}

func TestTerminalJobsUnchanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	content := `{
  "version":1,
  "github":null,
  "drive":null,
  "protectedRepos":[],
  "backupRecords":[],
  "backupJobs":[
    {"id":"j-c","protectedRepoId":"r1","fullName":"a/b","state":"failed","startedAt":"2026-01-01T00:00:00Z","error":"boom"}
  ]
}`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	j, _ := s.BackupJob("j-c")
	if j.State != JobFailed {
		t.Errorf("expected failed preserved, got %q", j.State)
	}
	if j.Error != "boom" {
		t.Errorf("expected error preserved, got %q", j.Error)
	}
	if !j.FinishedAt.IsZero() {
		t.Errorf("expected finishedAt NOT to be overwritten on terminal job")
	}
	if got := s.UnfinishedJobs(); len(got) != 0 {
		t.Errorf("expected no unfinished jobs, got %d", len(got))
	}
}

func TestSecretsNotSerialized(t *testing.T) {
	// Write a document with every model populated and inspect the raw JSON keys.
	s, _ := Open(filepath.Join(t.TempDir(), "state.json"))
	s.SetGitHubConnection(testGitHub())
	s.SetDriveConnection(testDrive())
	repo := testRepo(1, "a/b")
	_ = s.AddProtectedRepo(repo)
	s.AddBackupRecord(testRecord(repo.ID))
	s.CreateBackupJob(testJob("job-1", JobCompleted))

	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(s.Path())

	// No secret-looking key names anywhere.
	lower := strings.ToLower(string(data))
	for _, forbidden := range []string{
		`"token"`, `"accessToken"`, `"access_token"`, `"secret"`, `"password"`,
		`"privateKey"`, `"credentials"`, `"client_secret"`, `"refreshToken"`,
		`"authorization"`,
	} {
		if strings.Contains(lower, forbidden) {
			t.Errorf("found forbidden key %q in serialized state:\n%s", forbidden, data)
		}
	}
	// tokenRef values are present but never accompanied by token material.
	if !strings.Contains(string(data), `"tokenRef"`) {
		t.Errorf("expected tokenRef (a reference, not the token) in serialized state")
	}
	// No local filesystem path or repoPath identity leaked.
	for _, forbidden := range []string{`"repoPath"`, `"rootPath"`, `"tempDir"`, `"cloneURL"`, `"path"`} {
		if strings.Contains(lower, forbidden) {
			t.Errorf("found local-path field %q in serialized state:\n%s", forbidden, data)
		}
	}
}

func TestDuplicateProtectedRepoRejected(t *testing.T) {
	s, _ := Open(filepath.Join(t.TempDir(), "state.json"))
	if err := s.AddProtectedRepo(testRepo(7, "a/b")); err != nil {
		t.Fatal(err)
	}
	dup := testRepo(7, "a/b")
	dup.ID = "different-id"
	if err := s.AddProtectedRepo(dup); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("expected ErrDuplicate, got %v", err)
	}
}

func TestUpdateAndRemoveProtectedRepo(t *testing.T) {
	s, _ := Open(filepath.Join(t.TempDir(), "state.json"))
	repo := testRepo(7, "a/b")
	if err := s.AddProtectedRepo(repo); err != nil {
		t.Fatal(err)
	}
	repo.FullName = "a/renamed"
	if err := s.UpdateProtectedRepo(repo); err != nil {
		t.Fatal(err)
	}
	got, ok := s.ProtectedRepo(repo.ID)
	if !ok || got.FullName != "a/renamed" {
		t.Errorf("update failed: %+v ok=%v", got, ok)
	}
	if err := s.RemoveProtectedRepo(repo.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.ProtectedRepo(repo.ID); ok {
		t.Errorf("repo should be removed")
	}
	if err := s.RemoveProtectedRepo(repo.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound on second remove, got %v", err)
	}
}

func TestBackupRecordsForRepo(t *testing.T) {
	s, _ := Open(filepath.Join(t.TempDir(), "state.json"))
	rA := testRepo(1, "a/one")
	rB := testRepo(2, "b/two")
	_ = s.AddProtectedRepo(rA)
	_ = s.AddProtectedRepo(rB)
	s.AddBackupRecord(testRecord(rA.ID))
	s.AddBackupRecord(testRecord(rA.ID))
	s.AddBackupRecord(testRecord(rB.ID))
	if got := s.BackupRecordsForRepo(rA.ID); len(got) != 2 {
		t.Errorf("expected 2 records for repo A, got %d", len(got))
	}
	if got := s.BackupRecordsForRepo(rB.ID); len(got) != 1 {
		t.Errorf("expected 1 record for repo B, got %d", len(got))
	}
}

func TestOpenEmptyPathRejected(t *testing.T) {
	if _, err := Open(""); err == nil {
		t.Fatalf("expected error for empty path")
	}
}

func TestUnfinishedAfterSavePersistsInterrupted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, _ := Open(path)
	s.CreateBackupJob(testJob("j-x", JobCloning))
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	j, _ := s2.BackupJob("j-x")
	if j.State != JobInterrupted {
		t.Errorf("expected interrupted after load+save, got %q", j.State)
	}
}
