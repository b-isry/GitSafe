package cloud

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/api/drive/v3"
	"google.golang.org/api/option"
)

// fakeDriveServer is a minimal Drive API stub sufficient for
// EnsureGitSafeFolder. It dispatches on HTTP method only (the generated client
// derives the URL path from its base endpoint), so listFiles controls the
// Files.List response and any folder create returns the created folder id.
func fakeDriveServer(t *testing.T, listFiles []*drive.File) (*httptest.Server, *[]string) {
	t.Helper()
	queries := &[]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			*queries = append(*queries, r.URL.RawQuery)
			if len(listFiles) == 0 {
				_ = json.NewEncoder(w).Encode(&drive.FileList{Files: []*drive.File{}})
				return
			}
			_ = json.NewEncoder(w).Encode(&drive.FileList{Files: listFiles})
		case http.MethodPost:
			var f drive.File
			_ = json.NewDecoder(r.Body).Decode(&f)
			if f.MimeType != "application/vnd.google-apps.folder" {
				w.WriteHeader(http.StatusBadRequest)
			}
			_ = json.NewEncoder(w).Encode(&drive.File{Id: "new-folder-1", Name: f.Name})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	return srv, queries
}

func newServiceFromFake(t *testing.T, url string) *drive.Service {
	t.Helper()
	svc, err := drive.NewService(context.Background(),
		option.WithHTTPClient(http.DefaultClient),
		option.WithEndpoint(url+"/"))
	if err != nil {
		t.Fatalf("drive.NewService: %v", err)
	}
	return svc
}

// TestEnsureGitSafeFolderReusesExistingFolder verifies that the folder lookup
// narrows to a non-trashed GitSafe folder with the folder mime type and returns
// the existing ID without creating another folder.
func TestEnsureGitSafeFolderReusesExistingFolder(t *testing.T) {
	srv, queries := fakeDriveServer(t, []*drive.File{{Id: "folder-99", CreatedTime: "2026-01-01T00:00:00.000Z"}})
	defer srv.Close()

	got, err := EnsureGitSafeFolder(context.Background(), newServiceFromFake(t, srv.URL))
	if err != nil {
		t.Fatalf("EnsureGitSafeFolder: %v", err)
	}
	if got != "folder-99" {
		t.Fatalf("folder id = %q, want folder-99", got)
	}
	if len(*queries) != 1 {
		t.Fatalf("expected exactly one list call, got %d", len(*queries))
	}
	q, err := url.QueryUnescape((*queries)[0])
	if err != nil {
		t.Fatalf("unescape query: %v", err)
	}
	for _, want := range []string{"name='GitSafe'", "mimeType='application/vnd.google-apps.folder'", "trashed=false"} {
		if !strings.Contains(q, want) {
			t.Errorf("list query missing %q: %q", want, q)
		}
	}
}

// TestEnsureGitSafeFolderCreatesWhenMissing verifies a fresh account gets a
// GitSafe folder created instead of failing.
func TestEnsureGitSafeFolderCreatesWhenMissing(t *testing.T) {
	srv, _ := fakeDriveServer(t, nil)
	defer srv.Close()

	got, err := EnsureGitSafeFolder(context.Background(), newServiceFromFake(t, srv.URL))
	if err != nil {
		t.Fatalf("EnsureGitSafeFolder: %v", err)
	}
	if got != "new-folder-1" {
		t.Fatalf("folder id = %q, want new-folder-1", got)
	}
}

// TestEnsureGitSafeFolderRepeatedLookupIsStable verifies two lookups against the
// same account resolve to the same folder ID.
func TestEnsureGitSafeFolderRepeatedLookupIsStable(t *testing.T) {
	srv, queries := fakeDriveServer(t, []*drive.File{{Id: "folder-stable"}})
	defer srv.Close()
	ctx := context.Background()
	svc := newServiceFromFake(t, srv.URL)

	a, err := EnsureGitSafeFolder(ctx, svc)
	if err != nil {
		t.Fatalf("first lookup: %v", err)
	}
	b, err := EnsureGitSafeFolder(ctx, svc)
	if err != nil {
		t.Fatalf("second lookup: %v", err)
	}
	if a != "folder-stable" || b != a {
		t.Fatalf("lookups = %q %q, want the same folder-stable", a, b)
	}
	if len(*queries) != 2 {
		t.Fatalf("expected one list call per lookup, got %d", len(*queries))
	}
}

// TestEnsureGitSafeFolderPropagatesErrors verifies upstream failures surface
// instead of being swallowed.
func TestEnsureGitSafeFolderPropagatesErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "boom"}})
	}))
	defer srv.Close()

	if _, err := EnsureGitSafeFolder(context.Background(), newServiceFromFake(t, srv.URL)); err == nil {
		t.Fatal("expected an error from the upstream failure")
	}
}

// TestUploadPlacesFileUnderParent verifies UploadFile attaches the file to the
// given parent (the GitSafe folder) rather than the Drive root, and that the
// uploaded name is the bundle's basename.
func TestUploadPlacesFileUnderParent(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(&drive.File{Id: "uploaded-file-1"})
	}))
	defer srv.Close()
	svc, err := drive.NewService(context.Background(),
		option.WithHTTPClient(http.DefaultClient),
		option.WithEndpoint(srv.URL+"/"))
	if err != nil {
		t.Fatalf("drive.NewService: %v", err)
	}

	if _, err := UploadFile(context.Background(), svc, realFile(t, "backup.test"), "git-safe-folder", nil, nil); err != nil {
		t.Fatalf("UploadFile: %v", err)
	}
	for _, want := range []string{`"name":"backup.test"`, `"parents":["git-safe-folder"]`} {
		if !strings.Contains(body, want) {
			t.Errorf("upload request missing %q", want)
		}
	}
}

// realFile writes a small on-disk file in a temp dir and returns its path.
func realFile(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte("bundle payload"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	return path
}
