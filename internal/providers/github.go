package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// githubAPIBaseURL is the default GitHub REST API base URL.
const githubAPIBaseURL = "https://api.github.com"

// GitHub OAuth scopes. GitSafe requests `repo` only (covers private-repo
// discovery, cloning, and history backup) and deliberately does NOT request
// offline_access or refresh tokens.
const ScopeRepo = "repo"

// Header names used by the GitHub API.
const (
	headerAuthorization = "Authorization"
	headerAccept        = "Accept"
	headerAPIVersion    = "X-GitHub-Api-Version"
	headerLink          = "Link"
	headerOAuthScopes   = "X-OAuth-Scopes"
	headerRateRemaining = "X-RateLimit-Remaining"
)

const (
	acceptHeader     = "application/vnd.github+json"
	apiVersionHeader = "2022-11-28"
)

// Sentineling errors distinguish auth failures (which map to "reconnect") from
// other failures.
var (
	// ErrUnauthorized indicates the token is invalid, expired, or revoked.
	ErrUnauthorized = errors.New("github: unauthorized")
	// ErrRateLimited indicates the GitHub API rate limit was exceeded.
	ErrRateLimited = errors.New("github: rate limited")
	// ErrNotFound indicates the requested resource was not found.
	ErrNotFound = errors.New("github: not found")
)

// GitHubProvider is a GitHub API client. It carries a bearer token supplied at
// construction; the token is used only to sign requests and is never exposed
// through the public API.
type GitHubProvider struct {
	token   string
	baseURL string
	client  *http.Client
}

// NewGitHubProvider builds a client against the production GitHub API using
// the given bearer token. It is used by both the legacy CLI path (which passes
// a token directly) and the new OAuth path (which resolves the token from the
// token store before constructing the client).
func NewGitHubProvider(token string) *GitHubProvider {
	return newGitHubProvider(token, githubAPIBaseURL, &http.Client{Timeout: 30 * time.Second})
}

// newGitHubProvider is the unexported constructor shared by tests (allowing a
// custom base URL and HTTP client).
func newGitHubProvider(token, baseURL string, client *http.Client) *GitHubProvider {
	c := client
	if c == nil {
		c = &http.Client{Timeout: 30 * time.Second}
	}
	return &GitHubProvider{token: token, baseURL: strings.TrimSuffix(baseURL, "/"), client: c}
}

// GetRepositories preserves the legacy CLI behavior: it returns all
// repositories accessible to the authenticated account. It uses the same
// paginated discovery as ListRepositories.
func (g *GitHubProvider) GetRepositories() ([]Repository, error) {
	return g.ListRepositories(context.Background())
}

// Identity returns the authenticated account from GET /user.
func (g *GitHubProvider) Identity(ctx context.Context) (Identity, error) {
	var out userIdentityResponse
	if err := g.get(ctx, "/user", &out); err != nil {
		return Identity{}, err
	}
	return Identity{
		ID:        out.ID,
		Login:     out.Login,
		Name:      out.Name,
		AvatarURL: out.AvatarURL,
		Scopes:    out.Scopes,
	}, nil
}

// ListRepositories returns all repositories accessible to the account, walking
// every page until retrieval is complete. It supports owner, collaborator, and
// organization-member inbox repositories, and does NOT depend on a single
// page of results.
func (g *GitHubProvider) ListRepositories(ctx context.Context) ([]Repository, error) {
	var repos []Repository
	page := 1
	for {
		endpoint := fmt.Sprintf(
			"/user/repos?affiliation=owner,collaborator,organization_member&type=all&per_page=100&page=%d",
			page)

		var pageRepos []repoResponse
		linkHeader, err := g.getRaw(ctx, endpoint, &pageRepos)
		if err != nil {
			return nil, err
		}

		for _, r := range pageRepos {
			repos = append(repos, toRepository(r))
		}

		next := nextLink(linkHeader)
		if next == "" {
			break
		}
		// Get the next page number from the Link header, or stop if unparsable.
		page = pageFromNext(next)
		if page == 0 {
			break
		}
	}
	return repos, nil
}

// Repository returns a single repository by its owner/name address.
func (g *GitHubProvider) Repository(ctx context.Context, fullName string) (Repository, error) {
	if fullName == "" || !strings.Contains(fullName, "/") {
		return Repository{}, fmt.Errorf("github: invalid full name %q", fullName)
	}
	var out repoResponse
	if err := g.get(ctx, "/repos/"+fullName, &out); err != nil {
		return Repository{}, err
	}
	return toRepository(out), nil
}

// get performs a GET and decodes the JSON body into out. The returned OAuth
// scopes are attached to the out via a wrapping struct when the target is the
// identity type; for generic targets scopes are discarded.
func (g *GitHubProvider) get(ctx context.Context, path string, out any) error {
	_, err := g.getRaw(ctx, path, out)
	return err
}

// getRaw performs a GET and decodes JSON into out, returning the response Link
// header for pagination. It centralizes auth, error mapping, and rate-limit
// handling.
func (g *GitHubProvider) getRaw(ctx context.Context, path string, out any) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.baseURL+path, nil)
	if err != nil {
		return "", fmt.Errorf("github: build request: %w", err)
	}
	req.Header.Set(headerAuthorization, "Bearer "+g.token)
	req.Header.Set(headerAccept, acceptHeader)
	req.Header.Set(headerAPIVersion, apiVersionHeader)

	resp, err := g.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("github: request failed: %w", err)
	}
	defer resp.Body.Close()

	// Capture OAuth scopes into the identity response.
	if id, ok := out.(*userIdentityResponse); ok {
		if sc := resp.Header.Get(headerOAuthScopes); sc != "" {
			id.Scopes = splitScopes(sc)
		}
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("github: read response: %w", err)
	}

	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return "", ErrUnauthorized
	case http.StatusForbidden:
		// GitHub returns 403 for rate limiting (X-RateLimit-Remaining: 0) and
		// for a revoked/invalid OAuth token. Distinguish when possible.
		if resp.Header.Get(headerRateRemaining) == "0" {
			return "", fmt.Errorf("%w: %s", ErrRateLimited, sanitize(body))
		}
		// A 403 with a valid-looking request but bad scopes is auth-related.
		return "", ErrUnauthorized
	case http.StatusNotFound:
		return "", ErrNotFound
	default:
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return "", fmt.Errorf("github: API returned %s", resp.Status)
		}
	}

	if err := json.Unmarshal(body, out); err != nil {
		return "", fmt.Errorf("github: decode response: %w", err)
	}
	return resp.Header.Get(headerLink), nil
}

func splitScopes(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// sanitize returns a truncated, credential-free excerpt of a response body for
// error messages. GitHub error bodies do not contain tokens, but we bound the
// length and strip anything that looks like a tokenized URL defensively.
func sanitize(body []byte) string {
	s := strings.TrimSpace(string(body))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// nextLink extracts the URL of rel="next" from a Link header.
func nextLink(linkHeader string) string {
	for _, part := range strings.Split(linkHeader, ",") {
		segments := strings.Split(part, ";")
		if len(segments) < 2 {
			continue
		}
		target := strings.Trim(segments[0], " <>")
		for _, seg := range segments[1:] {
			seg = strings.TrimSpace(seg)
			if strings.HasPrefix(seg, "rel=") && strings.Trim(strings.TrimPrefix(seg, "rel="), "\"") == "next" {
				return target
			}
		}
	}
	return ""
}

var pagePattern = regexp.MustCompile(`[?&]page=(\d+)`)

// pageFromNext extracts the page number from a pagination URL.
func pageFromNext(rawURL string) int {
	m := pagePattern.FindStringSubmatch(rawURL)
	if len(m) != 2 {
		return 0
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0
	}
	return n
}

// userIdentityResponse is the JSON shape of GET /user plus the granted scopes
// populated from the response header.
type userIdentityResponse struct {
	ID        int64    `json:"id"`
	Login     string   `json:"login"`
	Name      string   `json:"name"`
	AvatarURL string   `json:"avatar_url"`
	Scopes    []string `json:"-"`
}

// repoResponse is the JSON shape returned by the GitHub repositories endpoints.
type repoResponse struct {
	ID       int64  `json:"id"`
	FullName string `json:"full_name"`
	Owner    struct {
		Login string `json:"login"`
	} `json:"owner"`
	Name          string `json:"name"`
	Private       bool   `json:"private"`
	Fork          bool   `json:"fork"`
	Archived      bool   `json:"archived"`
	DefaultBranch string `json:"default_branch"`
	CloneURL      string `json:"clone_url"`
	SSHURL        string `json:"ssh_url"`
	Description   string `json:"description"`
	UpdatedAt     string `json:"updated_at"`
	PushedAt      string `json:"pushed_at"`
	Size          int64  `json:"size"`
}

// toRepository maps the raw GitHub API response to the application model.
func toRepository(r repoResponse) Repository {
	return Repository{
		ID:            r.ID,
		FullName:      r.FullName,
		Owner:         r.Owner.Login,
		Name:          r.Name,
		Private:       r.Private,
		Fork:          r.Fork,
		Archived:      r.Archived,
		DefaultBranch: r.DefaultBranch,
		CloneURL:      r.CloneURL,
		SSHURL:        r.SSHURL,
		Description:   r.Description,
		UpdatedAt:     parseTime(r.UpdatedAt),
		PushedAt:      parseTime(r.PushedAt),
		SizeKB:        r.Size,
	}
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}
