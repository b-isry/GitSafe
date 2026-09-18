package archiver

import (
	"crypto/rand"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"
)

func Validate() error {
	if _, err := exec.LookPath("git"); err != nil {
		return errors.New("git executable not found in PATH")
	}
	return nil
}

func BundleRemoteRepo(cloneURL string, outputPath string) (string, error) {
	if err := os.MkdirAll(outputPath, 0o755); err != nil {
		return "", fmt.Errorf("create output directory %q: %w", outputPath, err)
	}

	// Never echo a tokenized clone URL into error messages or logs: strip any
	// userinfo (e.g. https://x-access-token:<TOKEN>@) before it can escape.
	redacted := redactCloneURL(cloneURL)

	repoName := repoNameFromCloneURL(cloneURL)
	if repoName == "" {
		return "", fmt.Errorf("could not derive repository name from clone URL %q", redacted)
	}

	tempDir, err := os.MkdirTemp("", "gitsafe-mirror-*")
	if err != nil {
		return "", fmt.Errorf("create temporary mirror directory: %w", err)
	}
	defer os.RemoveAll(tempDir)

	mirrorPath := filepath.Join(tempDir, repoName+".git")
	cloneCmd := exec.Command("git", "clone", "--mirror", cloneURL, mirrorPath)
	if output, err := cloneCmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("mirror clone %q: %w (%s)", redacted, err, string(output))
	}

	// Use a random suffix so two backups of the same repository within the same
	// second cannot overwrite each other's bundle (which would otherwise silently
	// corrupt an older record that still references the filename + SHA256).
	bundlePath := filepath.Join(outputPath, fmt.Sprintf("%s_%s.bundle", repoName, uniqueSuffix()))
	bundleCmd := exec.Command("git", "-C", mirrorPath, "bundle", "create", bundlePath, "--all")
	if output, err := bundleCmd.CombinedOutput(); err != nil {
		// Remove any partially-written bundle so it is not left as an orphan that
		// later shows up as a valid backup on the dashboard.
		_ = os.Remove(bundlePath)
		return "", fmt.Errorf("create remote bundle for %q: %w (%s)", redacted, err, string(output))
	}

	return bundlePath, nil
}

// redactCloneURL removes the userinfo (credentials) portion of a git URL so the
// remaining form can safely appear in errors and logs. It falls back to the raw
// string when the URL cannot be parsed.
func redactCloneURL(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if u, err := url.Parse(trimmed); err == nil && u.User != nil {
		u.User = nil
		if s := u.String(); s != "" {
			return s
		}
	}
	return trimmed
}

// uniqueSuffix returns a timestamp plus a random component so two bundles for
// the same repository created within the same clock resolution never collide.
func uniqueSuffix() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%s_%x", time.Now().Format("20060102_150405"), b)
}

func repoNameFromCloneURL(cloneURL string) string {
	trimmed := strings.TrimSpace(strings.TrimSuffix(cloneURL, "/"))
	if trimmed == "" {
		return ""
	}

	if strings.HasPrefix(trimmed, "git@") {
		parts := strings.SplitN(trimmed, ":", 2)
		if len(parts) == 2 {
			trimmed = parts[1]
		}
	}

	if parsed, err := url.Parse(trimmed); err == nil && parsed.Path != "" {
		trimmed = parsed.Path
	}

	base := path.Base(trimmed)
	base = strings.TrimSuffix(base, ".git")
	return strings.TrimSpace(base)
}
