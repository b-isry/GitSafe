package archiver

import (
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
