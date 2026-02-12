package archiver

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

func Validate() error {
	if _, err := exec.LookPath("git"); err != nil {
		return errors.New("git executable not found in PATH")
	}
	return nil
}

func BundleRepo(repoPath, outputPath string, backupHistory bool, logger *slog.Logger) (string, error) {
	if logger == nil {
		logger = slog.Default()
	}

	if err := os.MkdirAll(outputPath, 0o755); err != nil {
		return "", fmt.Errorf("create output directory %q: %w", outputPath, err)
	}

	if !backupHistory {
		logger.Warn("backupHistory=false requested, forcing full-history bundle for disaster recovery", "repo", repoPath)
	}

	repoName := filepath.Base(repoPath)
	timestamp := time.Now().Format("20060102_150405")
	bundlePath := filepath.Join(outputPath, fmt.Sprintf("%s_%s.bundle", repoName, timestamp))

	cmd := exec.Command("git", "-C", repoPath, "bundle", "create", bundlePath, "--all")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("create git bundle for %q: %w (%s)", repoPath, err, string(output))
	}

	logger.Info("backup bundle created", "repo", repoPath, "bundlePath", bundlePath)
	return bundlePath, nil
}
