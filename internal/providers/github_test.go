package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func tsHandler(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return s
}

func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func TestIdentityAndScopes(t *testing.T) {
	srv := tsHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("X-OAuth-Scopes", "repo, workflow")
		writeJSONStatus(w, http.StatusOK, map[string]any{
			"id": 42, "login": "octocat", "name": "Oc ToCat", "avatar_url": "https://a/x.png",
		})
	})
	g := newGitHubProvider("tok", srv.URL, srv.Client())
	id, err := g.Identity(context.Background())
	if err != nil {
		t.Fatalf("Identity: %v", err)
	}
	if id.ID != 42 || id.Login != "octocat" || id.Name != "Oc ToCat" {
		t.Fatalf("Identity = %+v", id)
	}
	if len(id.Scopes) != 2 || id.Scopes[0] != "repo" || id.Scopes[1] != "workflow" {
		t.Fatalf("Scopes = %v", id.Scopes)
	}
}

func TestIdentityNoScopesHeader(t *testing.T) {
	srv := tsHandler(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSONStatus(w, http.StatusOK, map[string]any{"id": 7, "login": "x"})
	})
	g := newGitHubProvider("tok", srv.URL, srv.Client())
	id, err := g.Identity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if id.Scopes != nil {
		t.Fatalf("Scopes = %v, want nil", id.Scopes)
	}
}

func TestListRepositoriesPagination(t *testing.T) {
	var (
		pageRequests int
		baseURL      string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pageRequests++
		page := r.URL.Query().Get("page")
		link := func(p string) string {
			return fmt.Sprintf("<%s/user/repos?page=%s>; rel=\"next\"", baseURL, p)
		}
		switch page {
		case "", "1":
			w.Header().Set("Link", link("2"))
			writeJSONStatus(w, http.StatusOK, []any{
				repoJSON(1, "a/b", false),
				repoJSON(2, "c/d", true),
			})
		case "2":
			w.Header().Set("Link", link("3"))
			writeJSONStatus(w, http.StatusOK, []any{repoJSON(3, "e/f", false)})
		case "3":
			writeJSONStatus(w, http.StatusOK, []any{})
		default:
			t.Fatalf("unexpected page %q", page)
		}
	}))
	t.Cleanup(srv.Close)
	baseURL = srv.URL

	g := newGitHubProvider("tok", srv.URL, srv.Client())
	repos, err := g.ListRepositories(context.Background())
	if err != nil {
		t.Fatalf("ListRepositories: %v", err)
	}
	if len(repos) != 3 {
		t.Fatalf("got %d repos, want 3", len(repos))
	}
	if repos[0].FullName != "a/b" || repos[1].Private != true || repos[2].ID != 3 {
		t.Fatalf("unexpected repos: %+v", repos)
	}
	if pageRequests != 3 {
		t.Fatalf("pageRequests = %d, want 3", pageRequests)
	}
}

func TestListRepositoriesSeedFields(t *testing.T) {
	srv := tsHandler(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSONStatus(w, http.StatusOK, []any{
			map[string]any{
				"id": 9, "full_name": "o/r", "owner": map[string]any{"login": "o"},
				"name": "r", "private": true, "fork": true, "archived": false,
				"default_branch": "main", "clone_url": "https://github.com/o/r.git",
				"ssh_url": "git@github.com:o/r.git", "description": "desc",
				"updated_at": "2024-01-02T03:04:05Z", "pushed_at": "2024-01-03T04:05:06Z",
				"size": 1234,
			},
		})
	})
	g := newGitHubProvider("tok", srv.URL, srv.Client())
	repos, err := g.ListRepositories(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r := repos[0]
	if r.ID != 9 || r.FullName != "o/r" || r.Owner != "o" || r.Name != "r" {
		t.Fatalf("repo identity fields wrong: %+v", r)
	}
	if !r.Private || !r.Fork || r.Archived || r.DefaultBranch != "main" {
		t.Fatalf("repo flags wrong: %+v", r)
	}
	if r.CloneURL != "https://github.com/o/r.git" || r.SSHURL != "git@github.com:o/r.git" {
		t.Fatalf("clone urls wrong: %+v", r)
	}
	if r.SizeKB != 1234 {
		t.Fatalf("size = %d", r.SizeKB)
	}
	wantUpdated := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	if !r.UpdatedAt.Equal(wantUpdated) {
		t.Fatalf("UpdatedAt = %v, want %v", r.UpdatedAt, wantUpdated)
	}
}

func TestRepositoryByFullName(t *testing.T) {
	srv := tsHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/o/r" {
			http.NotFound(w, r)
			return
		}
		writeJSONStatus(w, http.StatusOK, repoJSON(1, "o/r", true))
	})
	g := newGitHubProvider("tok", srv.URL, srv.Client())
	repo, err := g.Repository(context.Background(), "o/r")
	if err != nil {
		t.Fatalf("Repository: %v", err)
	}
	if repo.ID != 1 || repo.FullName != "o/r" || !repo.Private {
		t.Fatalf("repo = %+v", repo)
	}
}

func TestRepositoryInvalidName(t *testing.T) {
	g := newGitHubProvider("tok", githubAPIBaseURL, nil)
	if _, err := g.Repository(context.Background(), "no-slash"); err == nil {
		t.Fatal("expected error for invalid full name")
	}
}

func repoJSON(id int64, fullName string, private bool) map[string]any {
	parts := strings.SplitN(fullName, "/", 2)
	return map[string]any{
		"id": id, "full_name": fullName,
		"owner": map[string]any{"login": parts[0]}, "name": parts[1],
		"private": private, "default_branch": "main",
		"clone_url": "https://github.com/" + fullName + ".git",
	}
}

func TestUnauthorized(t *testing.T) {
	srv := tsHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"message":"Bad credentials"}`))
	})
	g := newGitHubProvider("tok", srv.URL, srv.Client())
	_, err := g.Identity(context.Background())
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
}

func TestForbiddenRateLimit(t *testing.T) {
	srv := tsHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"message":"API rate limit exceeded"}`))
	})
	g := newGitHubProvider("tok", srv.URL, srv.Client())
	_, err := g.Identity(context.Background())
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}
}

func TestForbiddenBadToken(t *testing.T) {
	srv := tsHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"message":"token does not have scope"}`))
	})
	g := newGitHubProvider("tok", srv.URL, srv.Client())
	_, err := g.Identity(context.Background())
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
}

func TestNotFound(t *testing.T) {
	srv := tsHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"message":"Not Found"}`))
	})
	g := newGitHubProvider("tok", srv.URL, srv.Client())
	_, err := g.Repository(context.Background(), "nope/nope")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestServerErrorStatus(t *testing.T) {
	srv := tsHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	g := newGitHubProvider("tok", srv.URL, srv.Client())
	if _, err := g.Identity(context.Background()); err == nil {
		t.Fatal("expected error for 500")
	}
}

func TestMalformedJSON(t *testing.T) {
	srv := tsHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{not json`))
	})
	g := newGitHubProvider("tok", srv.URL, srv.Client())
	if _, err := g.Identity(context.Background()); err == nil {
		t.Fatal("expected error for malformed JSON")
	}
}

func TestNetworkError(t *testing.T) {
	// A server that accepts then immediately closes sockets produces a
	// connection error on a conventional client.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("unreachable")
	}))
	url := srv.URL
	srv.Close()
	g := newGitHubProvider("tok", url, &http.Client{Timeout: 2 * time.Second})
	if _, err := g.Identity(context.Background()); err == nil {
		t.Fatal("expected network error")
	}
}

func TestGetRepositoriesLegacyPassthrough(t *testing.T) {
	srv := tsHandler(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSONStatus(w, http.StatusOK, []any{repoJSON(5, "x/y", false)})
	})
	g := newGitHubProvider("tok", srv.URL, srv.Client())
	repos, err := g.GetRepositories()
	if err != nil {
		t.Fatalf("GetRepositories: %v", err)
	}
	if len(repos) != 1 || repos[0].ID != 5 {
		t.Fatalf("repos = %+v", repos)
	}
}
