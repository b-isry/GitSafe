package backup

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/b-isry/gitsafe/internal/config"
)

func TestRunCreatesRealBundle(t *testing.T) {
	repoDir := createGitRepo(t)
	outDir := t.TempDir()

	runner := New(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	res := runner.Run(context.Background(), Request{
		RepoPath:      repoDir,
		OutputPath:    outDir,
		BackupHistory: true,
		Cloud:         config.CloudConfig{Enabled: false},
	}, nil)

	if res.State != StateDone {
		t.Fatalf("expected done, got %s (err: %s)", res.State, res.Error)
	}
	if res.BundlePath == "" {
		t.Fatalf("expected a bundle path")
	}
	if _, err := os.Stat(res.BundlePath); err != nil {
		t.Fatalf("bundle file does not exist: %v", err)
	}
	if res.BundleSize == 0 {
		t.Fatalf("expected non-zero bundle size, got %d", res.BundleSize)
	}
	if filepath.Ext(res.BundleName) != ".bundle" {
		t.Fatalf("expected .bundle filename, got %q", res.BundleName)
	}
	if res.Cloud.Status != "none" {
		t.Fatalf("expected cloud status none (disabled), got %s", res.Cloud.Status)
	}
}

func TestRunCloudDisabledNeverUploads(t *testing.T) {
	repoDir := createGitRepo(t)
	res := New(nil).Run(context.Background(), Request{
		RepoPath:      repoDir,
		OutputPath:    t.TempDir(),
		BackupHistory: true,
		Cloud:         config.CloudConfig{Enabled: false},
	}, nil)
	if res.State != StateDone {
		t.Fatalf("expected done, got %s", res.State)
	}
	if res.Cloud.Status != "none" {
		t.Fatalf("expected none, got %s", res.Cloud.Status)
	}
}

func TestRunMissingRepoFails(t *testing.T) {
	res := New(nil).Run(context.Background(), Request{
		RepoPath:      filepath.Join(t.TempDir(), "does-not-exist"),
		OutputPath:    t.TempDir(),
		BackupHistory: true,
		Cloud:         config.CloudConfig{Enabled: false},
	}, nil)
	if res.State != StateFailed {
		t.Fatalf("expected failed, got %s", res.State)
	}
	if res.Error == "" {
		t.Fatalf("expected an error message")
	}
}

func createGitRepo(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "GitSafe Test")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("test"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "initial commit")
	return dir
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v failed: %v (%s)", args, err, string(out))
	}
}
