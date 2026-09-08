package githuboauth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func testClient(cfg Config, base string, hc *http.Client) *Client {
	c := New(cfg)
	c.base = base
	if hc != nil {
		c.client = hc
	}
	return c
}

func TestAuthorizeURL(t *testing.T) {
	c := testClient(Config{
		ClientID:    "abc123",
		RedirectURL: "http://127.0.0.1:8080/api/auth/github/callback",
		Scopes:      []string{"repo"},
	}, "", nil)
	u := c.AuthorizeURL("state-val")
	parsed, err := url.Parse(u)
	if err != nil {
		t.Fatalf("parse %q: %v", u, err)
	}
	if parsed.Host != "github.com" || parsed.Path != "/login/oauth/authorize" {
		t.Fatalf("unexpected authorize url: %q", u)
	}
	q := parsed.Query()
	if q.Get("client_id") != "abc123" {
		t.Fatalf("client_id = %q", q.Get("client_id"))
	}
	if q.Get("scope") != "repo" {
		t.Fatalf("scope = %q", q.Get("scope"))
	}
	if q.Get("state") != "state-val" {
		t.Fatalf("state = %q", q.Get("state"))
	}
	if !strings.Contains(u, "redirect_uri=") {
		t.Fatalf("missing redirect_uri: %q", u)
	}
}

func TestExchangeSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
			t.Errorf("content-type = %q", ct)
		}
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		if r.PostForm.Get("client_secret") != "shh" {
			t.Errorf("client_secret not sent")
		}
		if r.PostForm.Get("code") != "the-code" {
			t.Errorf("code = %q", r.PostForm.Get("code"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"access_token":"tok-123","token_type":"bearer","scope":"repo"}`))
	}))
	t.Cleanup(srv.Close)

	c := testClient(Config{ClientID: "id", ClientSecret: "shh", RedirectURL: "http://127.0.0.1/cb"}, srv.URL, srv.Client())
	token, scopes, err := c.Exchange(context.Background(), "the-code")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if token != "tok-123" {
		t.Fatalf("token = %q", token)
	}
	if scopes != "repo" {
		t.Fatalf("scopes = %q", scopes)
	}
}

func TestExchangeAccessDenied(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"error":"access_denied"}`))
	}))
	t.Cleanup(srv.Close)
	c := testClient(Config{ClientID: "id", ClientSecret: "s"}, srv.URL, srv.Client())
	_, _, err := c.Exchange(context.Background(), "code")
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("err = %v, want ErrDenied", err)
	}
}

func TestExchangeServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	c := testClient(Config{ClientID: "id", ClientSecret: "s"}, srv.URL, srv.Client())
	if _, _, err := c.Exchange(context.Background(), "code"); err == nil {
		t.Fatal("expected error for 500")
	}
}

func TestExchangeNoToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	c := testClient(Config{ClientID: "id", ClientSecret: "s"}, srv.URL, srv.Client())
	if _, _, err := c.Exchange(context.Background(), "code"); err == nil {
		t.Fatal("expected error for empty token response")
	}
}

func TestExchangeNetworkError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()
	c := testClient(Config{ClientID: "id", ClientSecret: "s"}, url, &http.Client{Timeout: 2 * time.Second})
	if _, _, err := c.Exchange(context.Background(), "code"); err == nil {
		t.Fatal("expected network error")
	}
}

func TestExchangeUnexpectedErrorField(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"error":"incorrect_client_credentials"}`))
	}))
	t.Cleanup(srv.Close)
	c := testClient(Config{ClientID: "id", ClientSecret: "s"}, srv.URL, srv.Client())
	if _, _, err := c.Exchange(context.Background(), "code"); err == nil {
		t.Fatal("expected error for API error field")
	}
}
