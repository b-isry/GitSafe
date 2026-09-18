// Package githuboauth implements the server-side GitHub OAuth authorization-code
// exchange, token introspection, and token revocation needed to connect a GitHub
// account.
//
// GitSafe requests the `repo` scope only and deliberately does NOT request
// offline_access or refresh tokens. The client secret is read from the
// environment and is never exposed to the browser, serialized, or logged.
//
// PKCE is deliberately NOT used: GitHub's OAuth endpoints (for both GitHub Apps
// and classic OAuth Apps) do not support it. The documented CSRF defense for
// GitHub authorization flows is the `state` parameter, which GitSafe implements
// as a random, per-session, single-use value (see the server session layer).
package githuboauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Endpoints for the GitHub OAuth and token-info APIs. Overridable in tests.
const (
	authorizeURL = "https://github.com/login/oauth/authorize"
	tokenURL     = "https://github.com/login/oauth/access_token"
	// appsBaseURL hosts the GitHub REST endpoints used for token revocation.
	appsBaseURL = "https://api.github.com"
)

// Config carries the OAuth application settings. ClientID and RedirectURL are
// non-secret and may live in configuration; ClientSecret comes from an
// environment variable and is never persisted or logged.
type Config struct {
	ClientID     string
	ClientSecret string
	RedirectURL  string
	Scopes       []string
}

// Client performs the authorization-code exchange and token revocation.
type Client struct {
	cfg    Config
	client *http.Client
	// base overrides the authorize URL for tests.
	base string
	// apiBase overrides the GitHub REST API base URL for revocation in tests.
	apiBase string
}

// New builds a Client from the given configuration.
func New(cfg Config) *Client {
	return &Client{
		cfg:    cfg,
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

// ErrDenied indicates the user declined authorization at GitHub.
var ErrDenied = errors.New("github oauth: authorization denied")

// ErrInvalidState indicates the OAuth state could not be validated.
var ErrInvalidState = errors.New("github oauth: invalid state")

// AuthorizeURL returns the URL to send the user to begin the OAuth flow.
// state must be a per-session, single-use value.
func (c *Client) AuthorizeURL(state string) string {
	q := url.Values{}
	q.Set("client_id", c.cfg.ClientID)
	if c.cfg.RedirectURL != "" {
		q.Set("redirect_uri", c.cfg.RedirectURL)
	}
	q.Set("scope", strings.Join(c.cfg.Scopes, " "))
	q.Set("state", state)
	base := c.base
	if base == "" {
		base = authorizeURL
	}
	return base + "?" + q.Encode()
}

// tokenResponse is the shape returned by the /access_token endpoint. It
// includes an "error" field on failure.
type tokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	Scopes      string `json:"scope"`
	Error       string `json:"error"`
}

// Exchange converts an authorization code into an access token. It validates
// that the response actually carried a token and returns ErrDenied for a
// user-visible denial.
func (c *Client) Exchange(ctx context.Context, code string) (token, scope string, err error) {
	form := url.Values{}
	form.Set("client_id", c.cfg.ClientID)
	form.Set("client_secret", c.cfg.ClientSecret)
	form.Set("code", code)
	if c.cfg.RedirectURL != "" {
		form.Set("redirect_uri", c.cfg.RedirectURL)
	}

	base := c.base
	if base == "" {
		base = tokenURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base, strings.NewReader(form.Encode()))
	if err != nil {
		return "", "", fmt.Errorf("github oauth: build exchange request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("github oauth: exchange failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", fmt.Errorf("github oauth: read exchange response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("github oauth: token endpoint returned %s", resp.Status)
	}

	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", "", fmt.Errorf("github oauth: decode exchange response: %w", err)
	}
	if tr.Error != "" {
		if tr.Error == "access_denied" {
			return "", "", ErrDenied
		}
		return "", "", fmt.Errorf("github oauth: %s", tr.Error)
	}
	if tr.AccessToken == "" {
		return "", "", fmt.Errorf("github oauth: no access token in response")
	}
	return tr.AccessToken, tr.Scopes, nil
}

// revokeRequest is the JSON body GitHub expects for token revocation. It is
// sent with Basic auth (client id as username, client secret as password) and
// contains only the token value being revoked.
type revokeRequest struct {
	AccessToken string `json:"access_token"`
}

// Revoke invalidates an access token server-side through the GitHub API
// "Delete an app token" endpoint. It authenticates with the application's own
// client credentials, so it requires a configured client secret. A 404 (the
// token is already unknown/expired) is treated as success because the token is
// already unusable. Revocation is best-effort: callers must remove the local
// credential regardless of the result, since local removal is the security
// boundary.
func (c *Client) Revoke(ctx context.Context, token string) error {
	if token == "" {
		return errors.New("github oauth: cannot revoke an empty token")
	}
	body, err := json.Marshal(revokeRequest{AccessToken: token})
	if err != nil {
		return fmt.Errorf("github oauth: encode revoke request: %w", err)
	}
	base := c.apiBase
	if base == "" {
		base = appsBaseURL
	}
	endpoint := base + "/applications/" + url.PathEscape(c.cfg.ClientID) + "/token"
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return fmt.Errorf("github oauth: build revoke request: %w", err)
	}
	req.SetBasicAuth(c.cfg.ClientID, c.cfg.ClientSecret)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("github oauth: revoke failed: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusNotFound:
		// 204 revoked; 404 already gone (expired or already revoked).
		return nil
	default:
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("github oauth: revoke returned %s: %s", resp.Status, sanitize(respBody))
	}
}

// sanitize returns a bounded, newline-stripped excerpt of a response body for
// error messages.
func sanitize(body []byte) string {
	s := strings.TrimSpace(string(body))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
