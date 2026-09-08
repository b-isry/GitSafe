package scanner

import (
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFindStaleRepos(t *testing.T) {
	rootDir := t.TempDir()
	createRepoWithCommitDate(t, rootDir, "stale-repo", time.Now().AddDate(0, 0, -90))
	createRepoWithCommitDate(t, rootDir, "active-repo", time.Now().AddDate(0, 0, -10))

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	staleRepos, err := FindStaleRepos(rootDir, 60, logger)
	if err != nil {
		t.Fatalf("FindStaleRepos failed: %v", err)
	}

	if len(staleRepos) != 1 {
		t.Fatalf("expected 1 stale repo, got %d (%v)", len(staleRepos), staleRepos)
	}

	if !strings.HasSuffix(staleRepos[0], "stale-repo") {
		t.Fatalf("expected stale-repo to be detected, got %v", staleRepos)
	}
}

func TestFindStaleReposNoMatches(t *testing.T) {
	rootDir := t.TempDir()
	createRepoWithCommitDate(t, rootDir, "active-repo-1", time.Now().AddDate(0, 0, -15))
	createRepoWithCommitDate(t, rootDir, "active-repo-2", time.Now().AddDate(0, 0, -5))

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	staleRepos, err := FindStaleRepos(rootDir, 60, logger)
	if err != nil {
		t.Fatalf("FindStaleRepos failed: %v", err)
	}

	if len(staleRepos) != 0 {
		t.Fatalf("expected 0 stale repos, got %d (%v)", len(staleRepos), staleRepos)
	}
}

func createRepoWithCommitDate(t *testing.T, parentDir, repoName string, commitDate time.Time) {
	t.Helper()

	repoPath := filepath.Join(parentDir, repoName)
	if err := os.MkdirAll(repoPath, 0o755); err != nil {
		t.Fatalf("create repo dir: %v", err)
	}

	runGit(t, repoPath, nil, "init")
	runGit(t, repoPath, nil, "config", "user.email", "test@example.com")
	runGit(t, repoPath, nil, "config", "user.name", "GitSafe Test")

	filePath := filepath.Join(repoPath, "README.md")
	if err := os.WriteFile(filePath, []byte("test"), 0o644); err != nil {
		t.Fatalf("write test file: %v", err)
	}

	runGit(t, repoPath, nil, "add", ".")

	commitEnv := []string{
		"GIT_AUTHOR_DATE=" + commitDate.Format(time.RFC3339),
		"GIT_COMMITTER_DATE=" + commitDate.Format(time.RFC3339),
	}
	runGit(t, repoPath, commitEnv, "commit", "-m", "initial commit")
}

func runGit(t *testing.T, dir string, extraEnv []string, args ...string) {
	t.Helper()

	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), extraEnv...)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v failed: %v (%s)", args, err, string(output))
	}
}
