package scanner

import (
	"os"
	"os/exec"

	"path/filepath"
	"strings"
	"time"
)

func FindStaleRepos(root string, thresholdDays int) ([]string, error) {
	var staleRepos []string
	threshold := time.Now().AddDate(0, 0, -thresholdDays)

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() && info.Name() == ".git" {
			repoPath := filepath.Dir(path)

			cmd := exec.Command("git", "log", "-1", "--format=%cd")
			cmd.Dir = repoPath
			output, err := cmd.Output()
			if err != nil {
				return nil
			}

			dateStr := strings.TrimSpace(string(output))
			lastCommit, err := time.Parse("Mon Jan 2 15:04:05 2006 -0700", dateStr)
			if err != nil {
				return nil
			}

			if lastCommit.Before(threshold) {
				staleRepos = append(staleRepos, repoPath)
			}
		}
		return nil
	})
	return staleRepos, err
}
