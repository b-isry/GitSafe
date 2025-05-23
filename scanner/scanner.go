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

			cmd := exec.Command("git", "log", "-1", "--format=%cd", "--date=iso")
			cmd.Dir = repoPath
			output, err := cmd.Output()
			if err != nil {
				log.Printf("Error running git log for repo %s: %v", repoPath, err)
				return nil
			}

			dateStr := strings.TrimSpace(string(output))
			lastCommit, err := time.Parse("2006-01-02 15:04:05 -0700", dateStr)
			if err != nil {
				log.Printf("Error parsing date for repo %s (date string: %s): %v", repoPath, dateStr, err)
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
