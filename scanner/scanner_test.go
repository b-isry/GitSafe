package scanner

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Helper function to create a test repository
// commitTime: Time of the commit. If zero, defaults to now.
// daysAgo: If commitTime is zero, this specifies how many days ago the commit should be.
func createTestRepo(t *testing.T, parentDir string, repoName string, commitTime time.Time, daysAgo int) (string, func()) {
	t.Helper()
	repoPath := filepath.Join(parentDir, repoName)
	if err := os.MkdirAll(repoPath, 0755); err != nil {
		t.Fatalf("Failed to create repo directory %s: %v", repoPath, err)
	}

	// Initialize git repo
	cmd := exec.Command("git", "init")
	cmd.Dir = repoPath
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Failed to init git repo in %s: %v\nOutput: %s", repoPath, err, string(output))
	}

	// Create a dummy file to commit
	dummyFilePath := filepath.Join(repoPath, "dummy.txt")
	if err := os.WriteFile(dummyFilePath, []byte("hello"), 0644); err != nil {
		t.Fatalf("Failed to write dummy file in %s: %v", repoPath, err)
	}

	cmd = exec.Command("git", "add", "dummy.txt")
	cmd.Dir = repoPath
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Failed to git add in %s: %v\nOutput: %s", repoPath, err, string(output))
	}

	// Commit
	commitMsg := "Initial commit"
	var commitDate string
	if commitTime.IsZero() {
		commitTime = time.Now().AddDate(0, 0, -daysAgo)
	}
	// Format for GIT_COMMITTER_DATE
	commitDate = commitTime.Format("Mon Jan 2 15:04:05 2006 -0700")

	cmd = exec.Command("git", "commit", "-m", commitMsg)
	cmd.Dir = repoPath
	cmd.Env = append(os.Environ(), fmt.Sprintf("GIT_COMMITTER_DATE=%s", commitDate))
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Failed to git commit in %s (date: %s): %v\nOutput: %s", repoPath, commitDate, err, string(output))
	}

	cleanup := func() {
		// os.RemoveAll(repoPath) // This will be handled by cleaning up parentDir
	}
	return repoPath, cleanup
}

func TestFindStaleRepos_Stale(t *testing.T) {
	rootDir, err := os.MkdirTemp("", "test_stale_*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(rootDir)

	staleRepoPath, _ := createTestRepo(t, rootDir, "stale_repo", time.Time{}, 90) // 90 days ago
	_, _ = createTestRepo(t, rootDir, "not_stale_repo", time.Time{}, 30) // 30 days ago

	staleRepos, err := FindStaleRepos(rootDir, 60)
	if err != nil {
		t.Errorf("FindStaleRepos failed: %v", err)
	}

	if len(staleRepos) != 1 {
		t.Errorf("Expected 1 stale repo, got %d: %v", len(staleRepos), staleRepos)
	}
	if len(staleRepos) == 1 && !strings.HasSuffix(staleRepos[0], "stale_repo") {
		t.Errorf("Expected 'stale_repo' to be stale, got %s", staleRepos[0])
	}
}

func TestFindStaleRepos_NotStale(t *testing.T) {
	rootDir, err := os.MkdirTemp("", "test_not_stale_*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(rootDir)

	_, _ = createTestRepo(t, rootDir, "repo1", time.Time{}, 30) // 30 days ago
	_, _ = createTestRepo(t, rootDir, "repo2", time.Time{}, 10) // 10 days ago

	staleRepos, err := FindStaleRepos(rootDir, 60)
	if err != nil {
		t.Errorf("FindStaleRepos failed: %v", err)
	}

	if len(staleRepos) != 0 {
		t.Errorf("Expected 0 stale repos, got %d: %v", len(staleRepos), staleRepos)
	}
}

func TestFindStaleRepos_OnThreshold(t *testing.T) {
	rootDir, err := os.MkdirTemp("", "test_on_threshold_*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(rootDir)

	// Commit exactly 60 days ago, threshold is 60 days. Should NOT be stale.
	// FindStaleRepos uses threshold := time.Now().AddDate(0, 0, -thresholdDays)
	// and lastCommit.Before(threshold).
	// If lastCommit is exactly on the threshold time, it's not Before.
	thresholdDays := 60
	commitDate := time.Now().AddDate(0, 0, -thresholdDays)
	_, _ = createTestRepo(t, rootDir, "threshold_repo", commitDate, 0)


	staleRepos, err := FindStaleRepos(rootDir, thresholdDays)
	if err != nil {
		t.Errorf("FindStaleRepos failed: %v", err)
	}

	if len(staleRepos) != 0 {
		t.Errorf("Expected 0 stale repos for 'on threshold' case, got %d: %v", len(staleRepos), staleRepos)
	}
}

func TestFindStaleRepos_NotGitRepo(t *testing.T) {
	rootDir, err := os.MkdirTemp("", "test_not_git_*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(rootDir)

	// Create a directory that is not a git repo
	notGitPath := filepath.Join(rootDir, "not_a_repo")
	if err := os.Mkdir(notGitPath, 0755); err != nil {
		t.Fatalf("Failed to create non-git directory: %v", err)
	}
	// Create a valid git repo alongside
	_, _ = createTestRepo(t, rootDir, "actual_repo", time.Time{}, 90)


	staleRepos, err := FindStaleRepos(rootDir, 60)
	if err != nil {
		t.Errorf("FindStaleRepos failed: %v", err)
	}
	// Should still find the actual stale repo
	if len(staleRepos) != 1 {
		t.Errorf("Expected 1 stale repo, got %d (non-git dir should be ignored)", len(staleRepos))
	}
	if len(staleRepos) == 1 && !strings.HasSuffix(staleRepos[0], "actual_repo") {
		t.Errorf("Expected 'actual_repo' to be stale, got %s", staleRepos[0])
	}
}

func TestFindStaleRepos_GitLogFails(t *testing.T) {
	rootDir, err := os.MkdirTemp("", "test_git_log_fails_*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(rootDir)

	repoPath := filepath.Join(rootDir, "corrupted_repo")
	if err := os.Mkdir(repoPath, 0755); err != nil {
		t.Fatalf("Failed to create repo dir: %v", err)
	}

	// Create a file named .git instead of a directory to make git commands fail
	gitFilePath := filepath.Join(repoPath, ".git")
	if err := os.WriteFile(gitFilePath, []byte("not a dir"), 0644); err != nil {
		t.Fatalf("Failed to create dummy .git file: %v", err)
	}
	
	// Create a valid stale repo alongside to ensure the walk continues
	_, _ = createTestRepo(t, rootDir, "valid_stale_repo", time.Time{}, 90)


	// We expect FindStaleRepos to log an error but not to fail itself,
	// and to correctly identify other stale repos.
	staleRepos, err := FindStaleRepos(rootDir, 60)
	if err != nil {
		t.Errorf("FindStaleRepos failed unexpectedly: %v", err)
	}

	if len(staleRepos) != 1 {
		t.Errorf("Expected 1 stale repo, got %d. Corrupted repo should be skipped.", len(staleRepos))
	}
	if len(staleRepos) == 1 && !strings.HasSuffix(staleRepos[0], "valid_stale_repo") {
		t.Errorf("Expected 'valid_stale_repo' to be stale, got %s", staleRepos[0])
	}
}

// To test bad date format, we need to control the output of `git log`.
// We can do this by creating a script named `git` that shadows the real `git`
// and placing it in PATH.
func TestFindStaleRepos_BadDateFormat(t *testing.T) {
	rootDir, err := os.MkdirTemp("", "test_bad_date_*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(rootDir)

	// Directory for our fake git script
	fakeGitDir, err := os.MkdirTemp("", "fakegit_*")
	if err != nil {
		t.Fatalf("Failed to create fakegit dir: %v", err)
	}
	defer os.RemoveAll(fakeGitDir)

	// Path for the fake git script
	fakeGitPath := filepath.Join(fakeGitDir, "git")

	// Create the fake git script
	// This script will output a bad date when `log -1 --format=%cd --date=iso` is called.
	scriptContent := `#!/bin/bash
if [[ "$1" == "log" && "$2" == "-1" && "$3" == "--format=%cd" && "$4" == "--date=iso" ]]; then
  echo "This is not a valid date"
  exit 0
elif [[ "$1" == "init" || "$1" == "add" || "$1" == "commit" ]]; then
  # Call the real git for repo setup
  exec /usr/bin/git "$@"
else
  # For other commands, you might want to call the real git or error
  exec /usr/bin/git "$@" 
fi
`
	if err := os.WriteFile(fakeGitPath, []byte(scriptContent), 0755); err != nil {
		t.Fatalf("Failed to write fake git script: %v", err)
	}

	// Prepend fakeGitDir to PATH
	originalPath := os.Getenv("PATH")
	if err := os.Setenv("PATH", fmt.Sprintf("%s%c%s", fakeGitDir, os.PathListSeparator, originalPath)); err != nil {
		t.Fatalf("Failed to set PATH: %v", err)
	}
	defer os.Setenv("PATH", originalPath) // Restore PATH

	// Create a repo. The 'git init' and 'git commit' in createTestRepo will use the real git
	// because our script calls /usr/bin/git for those.
	// The commit date itself doesn't matter much here, as git log will be overridden.
	_, _ = createTestRepo(t, rootDir, "repo_with_bad_date", time.Time{}, 90)
	
	// Create a valid stale repo alongside to ensure the walk continues
	_, _ = createTestRepo(t, rootDir, "valid_stale_repo2", time.Time{}, 91)


	// FindStaleRepos should now use our fake git for the log command.
	// It should log an error for "repo_with_bad_date" and skip it.
	staleRepos, err := FindStaleRepos(rootDir, 60)
	if err != nil {
		t.Errorf("FindStaleRepos failed unexpectedly: %v", err)
	}
	
	if len(staleRepos) != 1 {
		t.Errorf("Expected 1 stale repo, got %d. Repo with bad date format should be skipped.", len(staleRepos))
	}
	if len(staleRepos) == 1 && !strings.HasSuffix(staleRepos[0], "valid_stale_repo2") {
		t.Errorf("Expected 'valid_stale_repo2' to be stale, got %s", staleRepos[0])
	}
}

// TODO: Add test for filepath.Walk error itself, though it's hard to simulate reliably.
// One way could be to make a directory unreadable after it's been found by Walk.
// However, the current error handling in FindStaleRepos just returns the error from filepath.Walk.
// The filepath.WalkFunc only returns filepath.SkipDir or the error itself.
// If the error is returned by the WalkFunc, Walk stops and returns that error.
// The current code returns `nil` from the WalkFunc for handled errors (git log fail, time parse fail),
// so the walk continues. If `os.FileInfo` itself is an error, that would be an unhandled case.
// For example, if `info` is nil and `err` is not nil at `if err != nil { return err }`.
