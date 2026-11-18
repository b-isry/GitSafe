package tokenstore

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func newFileStoreForTest(t *testing.T, key string) *FileStore {
	t.Helper()
	st, err := NewFileStore(filepath.Join(t.TempDir(), "tokens"), key)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	return st
}

func TestFileStoreSetGetDelete(t *testing.T) {
	st := newFileStoreForTest(t, "test-key-材料")
	if err := st.Set("github.42", "secret-token"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := st.Get("github.42")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "secret-token" {
		t.Fatalf("Get = %q, want %q", got, "secret-token")
	}
	if err := st.Delete("github.42"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := st.Get("github.42"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after delete = %v, want ErrNotFound", err)
	}
}

func TestFileStoreGetMissing(t *testing.T) {
	st := newFileStoreForTest(t, "k")
	if _, err := st.Get("github.999"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get missing = %v, want ErrNotFound", err)
	}
}

func TestFileStoreDeleteMissingIsIdempotent(t *testing.T) {
	st := newFileStoreForTest(t, "k")
	if err := st.Delete("github.999"); err != nil {
		t.Fatalf("Delete missing: want nil, got %v", err)
	}
}

func TestFileStoreRejectsEmptyArgs(t *testing.T) {
	st := newFileStoreForTest(t, "k")
	if err := st.Set("", "v"); err == nil {
		t.Fatal("Set with empty ref: want error")
	}
	if _, err := st.Get(""); err == nil {
		t.Fatal("Get with empty ref: want error")
	}
	if err := st.Delete(""); err == nil {
		t.Fatal("Delete with empty ref: want error")
	}
}

func TestFileStoreDistinctRefsAreIsolated(t *testing.T) {
	st := newFileStoreForTest(t, "k")
	if err := st.Set("github.1", "va"); err != nil {
		t.Fatal(err)
	}
	if err := st.Set("github.2", "vb"); err != nil {
		t.Fatal(err)
	}
	ga, _ := st.Get("github.1")
	gb, _ := st.Get("github.2")
	if ga != "va" || gb != "vb" {
		t.Fatalf("refs not isolated: a=%q b=%q", ga, gb)
	}
}

func TestFileStoreRequiresKey(t *testing.T) {
	if _, err := NewFileStore(t.TempDir(), ""); err == nil {
		t.Fatal("empty key: want error")
	}
	if _, err := NewFileStore("", "k"); err == nil {
		t.Fatal("empty dir: want error")
	}
}

func TestFileStoreCiphertextNotPlaintext(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tokens")
	st, err := NewFileStore(dir, "k")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Set("github.42", "super-duper-secret"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "github.42.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "super-duper-secret") {
		t.Fatal("token stored in plaintext")
	}
}

func TestFileStoreTamperedFileFails(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tokens")
	st, err := NewFileStore(dir, "k")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Set("github.42", "tok"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "github.42.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[0] ^= 0xff
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Get("github.42"); err == nil {
		t.Fatal("tampered file must not decode")
	}
}

func TestFileStoreWrongKeyFailsDecrypt(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tokens")
	st, err := NewFileStore(dir, "right-key")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Set("github.42", "tok"); err != nil {
		t.Fatal(err)
	}
	other, err := NewFileStore(dir, "wrong-key")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Get("github.42"); err == nil {
		t.Fatal("wrong key must not decrypt")
	}
}

func TestFileStoreFileModes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits are not enforced on Windows")
	}
	dir := filepath.Join(t.TempDir(), "tokens")
	st, err := NewFileStore(dir, "k")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Set("github.42", "tok"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("dir mode = %o, want 700", perm)
	}
	fi, err := os.Stat(filepath.Join(dir, "github.42.json"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("file mode = %o, want 600", perm)
	}
}

func TestFileStorePathHardening(t *testing.T) {
	st := newFileStoreForTest(t, "k")
	bad := "../../../etc/passwd"
	if err := st.Set(bad, "v"); err != nil {
		t.Fatal(err)
	}
	path := st.pathFor(bad)
	forbidden := filepath.Join("..", "..", "etc", "passwd")
	if strings.Contains(path, forbidden) || !strings.HasPrefix(path, st.dir) {
		t.Fatalf("traversal not contained: %s", path)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("hardened path did not persist: %v", err)
	}
}
