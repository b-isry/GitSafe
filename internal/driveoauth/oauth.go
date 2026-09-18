// Package driveoauth implements the server-side Google Drive OAuth
// authorization-code flow with offline access, refresh-token handling, and
// token revocation needed to connect a Google Drive account.
//
// Unlike the GitHub flow, Drive MUST request a refresh token (access_type=
// offline) because backups run detached from any browser session and need to
// mint fresh access tokens long after the initial connection. The client secret
// is read from the environment and is never exposed to the browser, serialized,
// or logged. The CSRF defense is the same single-use per-session `state`
// parameter used for GitHub.
package driveoauth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// ScopeDriveFile limits access to files created or opened by this application.
// GitSafe uploads backups into a folder it creates, so it never needs access to
// files owned by other applications.
const ScopeDriveFile = "https://www.googleapis.com/auth/drive.file"

// Endpoints for the Google OAuth APIs. Overridable in tests.
const (
	// revokeURL revokes an OAuth token. It accepts the token as a POST form
	// field and needs no client credentials beyond a registered token.
	revokeURL = "https://oauth2.googleapis.com/revoke"
)

// ErrDenied indicates the user declined authorization at Google.
var ErrDenied = errors.New("drive oauth: authorization denied")

// ErrInvalidState indicates the OAuth state could not be validated.
var ErrInvalidState = errors.New("drive oauth: invalid state")

// Config carries the OAuth application settings. ClientID and RedirectURL are
// non-secret and may live in configuration; ClientSecret comes from an
// environment variable and is never persisted or logged.
type Config struct {
	ClientID     string
	ClientSecret string
	RedirectURL  string
	// Endpoint overrides the Google/OAuth endpoint for tests. An empty value
	// uses Google's production endpoint.
	Endpoint oauth2.Endpoint
}

// Client performs the authorization-code exchange, refresh-token handling, and
// token revocation.
type Client struct {
	cfg    Config
	client *http.Client
	// revokeBase overrides the Google revocation URL for tests.
	revokeBase string
}

// New builds a Client from the given configuration.
func New(cfg Config) *Client {
	if cfg.Endpoint == (oauth2.Endpoint{}) {
		cfg.Endpoint = google.Endpoint
	}
	return &Client{
		cfg:    cfg,
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

// oauthCfg returns the underlying x/oauth2 configuration used for the
// authorization URL and token exchange.
func (c *Client) oauthCfg() *oauth2.Config {
	return &oauth2.Config{
		ClientID:     c.cfg.ClientID,
		ClientSecret: c.cfg.ClientSecret,
		RedirectURL:  c.cfg.RedirectURL,
		Scopes:       []string{ScopeDriveFile},
		Endpoint:     c.cfg.Endpoint,
	}
}

// AuthorizeURL returns the URL to send the user to begin the OAuth flow.
// state must be a per-session, single-use value. offline access is mandatory
// (GitSafe mints fresh access tokens from the refresh token for detached backup
// and cleanup work), and a forced consent prompt guarantees a refresh token is
// issued on every connect.
func (c *Client) AuthorizeURL(state string) string {
	cfg := c.oauthCfg()
	return cfg.AuthCodeURL(
		state,
		oauth2.AccessTypeOffline,
		oauth2.ApprovalForce,
	)
}

// Exchange converts an authorization code into an OAuth token. It returns
// ErrDenied for a user-visible denial.
func (c *Client) Exchange(ctx context.Context, code string) (*oauth2.Token, error) {
	tok, err := c.oauthCfg().Exchange(ctx, code)
	if err != nil {
		if strings.Contains(err.Error(), "access_denied") {
			return nil, ErrDenied
		}
		return nil, fmt.Errorf("drive oauth: exchange failed: %w", err)
	}
	if tok.AccessToken == "" {
		return nil, errors.New("drive oauth: no access token in response")
	}
	return tok, nil
}

// TokenSource returns a token source that refreshes a stored refresh token into
// fresh access tokens. A source is always returned (Google's x/oauth2 client
// performs refreshes lazily on first access), so callers must verify the
// refresh token is non-empty before use.
func (c *Client) TokenSource(ctx context.Context, refreshToken string) oauth2.TokenSource {
	return c.oauthCfg().TokenSource(ctx, &oauth2.Token{RefreshToken: refreshToken})
}

// Revoke invalidates a token server-side through Google's revocation endpoint.
// It accepts either an access token or the refresh token itself. Revocation is
// best-effort: callers must remove the local credential regardless of the
// result, since local removal is the security boundary.
func (c *Client) Revoke(ctx context.Context, token string) error {
	if token == "" {
		return errors.New("drive oauth: cannot revoke an empty token")
	}
	form := url.Values{}
	form.Set("token", token)

	base := c.revokeBase
	if base == "" {
		base = revokeURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("drive oauth: build revoke request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("drive oauth: revoke failed: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	default:
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("drive oauth: revoke returned %s: %s", resp.Status, sanitize(respBody))
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
