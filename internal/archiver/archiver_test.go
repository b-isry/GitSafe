package archiver

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestRedactCloneURLStripsCredentials(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"https://x-access-token:SECRET@github.com/acme/alpha.git", "https://github.com/acme/alpha.git"},
		{"https://user:pass@example.com/r.git", "https://example.com/r.git"},
		{"git@github.com:acme/alpha.git", "git@github.com:acme/alpha.git"},
		{"not a url", "not a url"},
		{"", ""},
	}
	for _, c := range cases {
		if got := redactCloneURL(c.in); got != c.want {
			t.Errorf("redactCloneURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRedactCloneURLNeverContainsSecret(t *testing.T) {
	out := redactCloneURL("https://x-access-token:SUPERSECRET@github.com/acme/alpha.git")
	if strings.Contains(out, "SUPERSECRET") {
		t.Errorf("redacted URL leaked the secret: %q", out)
	}
}

func TestUniqueSuffixDistinguishesBundles(t *testing.T) {
	a := uniqueSuffix()
	b := uniqueSuffix()
	if a == b {
		t.Errorf("uniqueSuffix returned identical values: %q", a)
	}
}

func TestRepoNameFromCloneURL(t *testing.T) {
	if got := repoNameFromCloneURL("https://x-access-token:tok@github.com/acme/alpha.git"); got != "alpha" {
		t.Errorf("repoNameFromCloneURL = %q, want alpha", got)
	}
}

// TestBuildCloneURLValidation verifies that BuildCloneURL rejects malicious repository names.
func TestBuildCloneURLValidation(t *testing.T) {
	tests := []struct {
		name          string
		fullName      string
		wantError     bool
		errorContains string
	}{
		{"valid name", "owner/repo", false, ""},
		{"valid with dots", "owner.name/repo-name", false, ""},
		{"valid with underscores", "owner_name/repo_name", false, ""},
		{"valid with hyphens", "owner-name/repo-name", false, ""},
		{"starts with hyphen", "-owner/repo", true, "invalid repository name"},
		{"equals dot", "./repo", true, "invalid repository name"},
		{"equals dotdot", "../repo", true, "invalid repository name"},
		{"path traversal", "owner/../repo", true, "invalid repository name"},
		{"empty owner", "/repo", true, "invalid repository name"},
		{"empty name", "owner/", true, "invalid repository name"},
		{"contains space", "owner/ repo", true, "invalid repository name"},
		{"contains backslash", "owner\\repo", true, "invalid repository name"},
		{"contains colon", "owner:repo", true, "invalid repository name"},
		{"contains at sign", "owner@repo", true, "invalid repository name"},
		{"contains dollar", "owner$repo", true, "invalid repository name"},
		{"upload-pack injection", "owner/repo --upload-pack=evil", true, "invalid repository name"},
		{"ext protocol", "ext::ssh -oProxyCommand=evil host/path", true, "invalid repository name"},
		{"file protocol", "file:///etc/passwd", true, "invalid repository name"},
		{"non-GitHub host", "evil.com/owner/repo", true, "invalid repository name"},
		{"SSH URL", "git@github.com:owner/repo.git", true, "invalid repository name"},
		{"HTTP URL", "http://github.com/owner/repo.git", true, "invalid repository name"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := BuildCloneURL(tt.fullName)
			if tt.wantError {
				if err == nil {
					t.Errorf("BuildCloneURL(%q) expected error but got none", tt.fullName)
				} else if tt.errorContains != "" && !strings.Contains(err.Error(), tt.errorContains) {
					t.Errorf("BuildCloneURL(%q) error = %q, want to contain %q", tt.fullName, err.Error(), tt.errorContains)
				}
			} else {
				if err != nil {
					t.Errorf("BuildCloneURL(%q) unexpected error: %v", tt.fullName, err)
				}
			}
		})
	}
}

// TestTokenRedactionInErrors verifies that tokens are redacted from error messages.
func TestTokenRedactionInErrors(t *testing.T) {
	// Create a temp directory for output
	tmpDir, err := os.MkdirTemp("", "gitsafe-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Try to clone a non-existent repo with a token-like string in the URL
	// The error should not contain the token
	ctx := context.Background()
	_, err = BundleRemoteRepo(ctx, "owner/repo", "FAKE_TOKEN_12345", tmpDir)
	if err == nil {
		t.Fatal("expected error for non-existent repo")
	}
	errMsg := err.Error()
	if strings.Contains(errMsg, "FAKE_TOKEN_12345") {
		t.Errorf("token leaked in error message: %s", errMsg)
	}
	if strings.Contains(errMsg, "token") && strings.Contains(strings.ToLower(errMsg), "fake") {
		t.Errorf("token-like string leaked in error: %s", errMsg)
	}
}

// TestRedactOutput verifies that redactOutput removes sensitive data.
func TestRedactOutput(t *testing.T) {
	cases := []struct {
		in      string
		notWant string
	}{
		{"Authorization: Basic dXNlcjpwYXNz", "dXNlcjpwYXNz"},
		{"Authorization: Basic QWxhZGRpbjpvcGVuIHNlc2FtZQ==", "QWxhZGRpbjpvcGVuIHNlc2FtZQ=="},
		{"https://user:pass@github.com/owner/repo.git", "user:pass"},
		{"error: https://token123@github.com/owner/repo.git failed", "token123"},
	}

	for _, c := range cases {
		out := redactOutput(c.in)
		if strings.Contains(out, c.notWant) {
			t.Errorf("redactOutput(%q) leaked %q: %s", c.in, c.notWant, out)
		}
	}
}
