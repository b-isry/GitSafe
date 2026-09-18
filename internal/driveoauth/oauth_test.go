package driveoauth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"golang.org/x/oauth2"
)

func testClient(t *testing.T, revokePath string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			if err := r.ParseForm(); err != nil || r.Form.Get("code") != "the-code" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"acc-token","token_type":"Bearer","refresh_token":"ref-token","expires_in":3600}`))
		case revokePath:
			if err := r.ParseForm(); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if r.Form.Get("token") == "" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	// The x/oauth2 exchange and refresh both hit the /token path of the fake
	// server; authorize requests are GET /auth.
	ep := oauth2.Endpoint{
		AuthURL:  srv.URL + "/auth",
		TokenURL: srv.URL + "/token",
	}
	c := New(Config{
		ClientID:     "drive-client",
		ClientSecret: "drive-secret",
		RedirectURL:  "http://127.0.0.1:8080/api/auth/drive/callback",
		Endpoint:     ep,
	})
	c.revokeBase = srv.URL + revokePath
	return c
}

func TestAuthorizeURLIncludesOfflineAndConsent(t *testing.T) {
	c := New(Config{ClientID: "id", RedirectURL: "http://x/cb"})
	u := c.AuthorizeURL("state-123")
	q := mustQuery(t, u)
	if q.Get("client_id") != "id" {
		t.Fatalf("client_id = %q", q.Get("client_id"))
	}
	if q.Get("redirect_uri") != "http://x/cb" {
		t.Fatalf("redirect_uri = %q", q.Get("redirect_uri"))
	}
	if q.Get("scope") != ScopeDriveFile {
		t.Fatalf("scope = %q, want %q", q.Get("scope"), ScopeDriveFile)
	}
	if q.Get("state") != "state-123" {
		t.Fatalf("state = %q", q.Get("state"))
	}
	if q.Get("access_type") != "offline" {
		t.Fatalf("access_type = %q, want offline", q.Get("access_type"))
	}
	if q.Get("prompt") != "consent" {
		t.Fatalf("prompt = %q, want consent (forces refresh token)", q.Get("prompt"))
	}
	if q.Get("client_secret") != "" || strings.Contains(u, "drive-secret") {
		t.Fatalf("client secret leaked into authorize URL: %q", u)
	}
}

func TestExchangeReturnsAccessAndRefreshTokens(t *testing.T) {
	// The fake token endpoint must answer the x/oauth2 POST.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/token" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if err := r.ParseForm(); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if r.Form.Get("code") != "the-code" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"acc-token","token_type":"Bearer","refresh_token":"ref-token","expires_in":3600}`))
	}))
	t.Cleanup(srv.Close)

	c := New(Config{
		ClientID: "id", ClientSecret: "secret",
		Endpoint: oauth2.Endpoint{AuthURL: srv.URL + "/auth", TokenURL: srv.URL + "/token"},
	})
	tok, err := c.Exchange(context.Background(), "the-code")
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if tok.AccessToken != "acc-token" {
		t.Fatalf("access token = %q", tok.AccessToken)
	}
	if tok.RefreshToken != "ref-token" {
		t.Fatalf("refresh token = %q", tok.RefreshToken)
	}
}

func TestTokenSourceRefreshesWithStoredRefreshToken(t *testing.T) {
	var gotRefresh string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/token" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if err := r.ParseForm(); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		gotRefresh = r.Form.Get("refresh_token")
		if r.Form.Get("grant_type") != "refresh_token" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"fresh-access","token_type":"Bearer","expires_in":3600}`))
	}))
	t.Cleanup(srv.Close)

	c := New(Config{ClientID: "id", ClientSecret: "secret",
		Endpoint: oauth2.Endpoint{AuthURL: srv.URL + "/auth", TokenURL: srv.URL + "/token"}})
	src := c.TokenSource(context.Background(), "stored-refresh")
	tok, err := src.Token()
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if gotRefresh != "stored-refresh" {
		t.Fatalf("refresh token sent = %q", gotRefresh)
	}
	if tok.AccessToken != "fresh-access" {
		t.Fatalf("fresh access token = %q", tok.AccessToken)
	}
}

func TestRevoke(t *testing.T) {
	c := testClient(t, "/revoke")
	if err := c.Revoke(context.Background(), "tok"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
}

func TestRevokeEmptyToken(t *testing.T) {
	c := testClient(t, "/revoke")
	if err := c.Revoke(context.Background(), ""); err == nil {
		t.Fatal("expected an error revoking an empty token")
	}
}

func mustQuery(t *testing.T, raw string) url.Values {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u.Query()
}
