package scanner

import (
	"fmt"
	"io/fs"
	"log/slog"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func FindStaleRepos(root string, thresholdDays int, logger *slog.Logger) ([]string, error) {
	if logger == nil {
		logger = slog.Default()
	}

	var staleRepos []string
	threshold := time.Now().AddDate(0, 0, -thresholdDays)

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !d.IsDir() || d.Name() != ".git" {
			return nil
		}

		repoPath := filepath.Dir(path)
		cmd := exec.Command("git", "log", "-1", "--format=%cI")
		cmd.Dir = repoPath

		output, err := cmd.Output()
		if err != nil {
			logger.Warn("skipping repository: cannot inspect last commit", "repo", repoPath, "error", err)
			return filepath.SkipDir
		}

		lastCommit, err := time.Parse(time.RFC3339, strings.TrimSpace(string(output)))
		if err != nil {
			logger.Warn("skipping repository: cannot parse last commit date", "repo", repoPath, "error", err)
			return filepath.SkipDir
		}

		if lastCommit.Before(threshold) {
			logger.Info("stale repository detected", "repo", repoPath, "lastCommit", lastCommit.Format(time.RFC3339))
			staleRepos = append(staleRepos, repoPath)
		}

		return filepath.SkipDir
	})
	if err != nil {
		return nil, fmt.Errorf("walk root %q: %w", root, err)
	}

	return staleRepos, nil
}
