package archiver

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// repoNameRegex validates GitHub owner/name format: owner/name where each segment
// contains letters, digits, dot, underscore, hyphen. Must not start with "-" or equal "." or ".."
var repoNameRegex = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*$`)

func Validate() error {
	if _, err := exec.LookPath("git"); err != nil {
		return errors.New("git executable not found in PATH")
	}
	return nil
}

// ValidateGitVersion checks that git is at least 2.31 (required for env-based http.extraheader)
func ValidateGitVersion(minMajor, minMinor int) error {
	cmd := exec.Command("git", "--version")
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("failed to run git --version: %w", err)
	}
	// git version 2.45.2.windows.1
	versionStr := strings.TrimSpace(string(out))
	var major, minor int
	if _, err := fmt.Sscanf(versionStr, "git version %d.%d", &major, &minor); err != nil {
		return fmt.Errorf("unexpected git version format: %s", versionStr)
	}
	if major < minMajor || (major == minMajor && minor < minMinor) {
		return fmt.Errorf("git version %d.%d is too old; need at least %d.%d", major, minor, minMajor, minMinor)
	}
	return nil
}

// BuildCloneURL constructs a https://github.com/owner/name.git URL from a validated fullName.
// The token is NOT included in the URL; it will be passed via git's environment config.
func BuildCloneURL(fullName string) (string, error) {
	if !repoNameRegex.MatchString(fullName) {
		return "", fmt.Errorf("invalid repository name: %q", fullName)
	}
	return "https://github.com/" + fullName + ".git", nil
}

// BundleRemoteRepo securely clones a repository using the token via git's http.extraheader
// config. The token never appears in argv, URLs, or logs.
func BundleRemoteRepo(ctx context.Context, fullName, token, outputPath string) (string, error) {
	if err := os.MkdirAll(outputPath, 0o755); err != nil {
		return "", fmt.Errorf("create output directory %q: %w", outputPath, err)
	}

	cloneURL, err := BuildCloneURL(fullName)
	if err != nil {
		return "", err
	}

	// Derive a safe repo name for the temp directory
	repoName := safeRepoName(fullName)
	if repoName == "" {
		return "", fmt.Errorf("could not derive safe repository name from %q", fullName)
	}

	// Create a per-job temp directory for HOME and staging
	tempDir, err := os.MkdirTemp("", "gitsafe-job-*")
	if err != nil {
		return "", fmt.Errorf("create temporary job directory: %w", err)
	}
	defer os.RemoveAll(tempDir)

	mirrorPath := filepath.Join(tempDir, repoName+".git")

	// Prepare git environment with token via http.extraheader (git 2.31+)
	gitEnv := prepareGitEnv(token, tempDir)

	// Clone with --mirror
	cloneCmd := exec.CommandContext(ctx, "git", "clone", "--mirror", cloneURL, mirrorPath)
	cloneCmd.Env = gitEnv
	if output, err := cloneCmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("mirror clone %q: %w (%s)", cloneURL, err, redactOutput(string(output)))
	}

	// Create bundle with random suffix
	bundleName := fmt.Sprintf("%s_%s.bundle", repoName, uniqueSuffix())
	bundlePath := filepath.Join(outputPath, bundleName)
	bundleCmd := exec.CommandContext(ctx, "git", "-C", mirrorPath, "bundle", "create", bundlePath, "--all")
	bundleCmd.Env = gitEnv
	if output, err := bundleCmd.CombinedOutput(); err != nil {
		_ = os.Remove(bundlePath)
		return "", fmt.Errorf("create remote bundle for %q: %w (%s)", cloneURL, err, redactOutput(string(output)))
	}

	return bundlePath, nil
}

// prepareGitEnv returns a clean environment for git commands with the token
// passed via http.extraheader config. Sets GIT_ALLOW_PROTOCOL=https,
// GIT_TERMINAL_PROMPT=0, GIT_CONFIG_NOSYSTEM=1, GIT_CONFIG_GLOBAL=/dev/null,
// and HOME to the given temp dir.
func prepareGitEnv(token, tempDir string) []string {
	// Basic auth header for x-access-token
	authHeader := "Authorization: Basic " + basicAuthHeader("x-access-token", token)

	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + tempDir,
		"GIT_ALLOW_PROTOCOL=https",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.https://github.com/.extraheader",
		"GIT_CONFIG_VALUE_0=" + authHeader,
	}
}

// basicAuthHeader creates a Basic auth header value for the given user/pass
func basicAuthHeader(user, pass string) string {
	// In practice, we'd use base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
	// but we'll do it manually to avoid extra imports
	return base64Encode(user + ":" + pass)
}

// base64Encode is a minimal base64 encoder
func base64Encode(s string) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	var result strings.Builder
	input := []byte(s)
	for i := 0; i < len(input); i += 3 {
		var b1, b2, b3 byte
		b1 = input[i]
		if i+1 < len(input) {
			b2 = input[i+1]
		}
		if i+2 < len(input) {
			b3 = input[i+2]
		}
		result.WriteByte(alphabet[b1>>2])
		result.WriteByte(alphabet[((b1&0x03)<<4)|(b2>>4)])
		if i+1 < len(input) {
			result.WriteByte(alphabet[((b2&0x0f)<<2)|(b3>>6)])
		} else {
			result.WriteByte('=')
		}
		if i+2 < len(input) {
			result.WriteByte(alphabet[b3&0x3f])
		} else {
			result.WriteByte('=')
		}
	}
	return result.String()
}

// safeRepoName extracts a filesystem-safe name from fullName (owner/name)
func safeRepoName(fullName string) string {
	parts := strings.Split(fullName, "/")
	if len(parts) != 2 {
		return ""
	}
	name := parts[1]
	// Remove .git suffix if present
	name = strings.TrimSuffix(name, ".git")
	return name
}

// redactOutput removes potential tokens from git command output
func redactOutput(output string) string {
	// Redact any Authorization header values that might have leaked
	authPattern := regexp.MustCompile(`(?i)Authorization:\s*Basic\s+[A-Za-z0-9+/=]+`)
	output = authPattern.ReplaceAllString(output, "Authorization: Basic [REDACTED]")
	// Redact any token-like strings in URLs
	tokenURLPattern := regexp.MustCompile(`https://[^@\s]+@github\.com`)
	output = tokenURLPattern.ReplaceAllString(output, "https://[REDACTED]@github.com")
	return output
}

func uniqueSuffix() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%s_%x", time.Now().Format("20060102_150405"), b)
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

// repoNameFromCloneURL extracts the repository name from a clone URL.
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
