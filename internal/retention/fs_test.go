package retention

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestDiskFSListAndRemove(t *testing.T) {
	dir := t.TempDir()
	fs := DiskFS{}

	// Missing dir → empty, no error.
	bundles, err := fs.ListBundles(filepath.Join(dir, "nope"))
	if err != nil || len(bundles) != 0 {
		t.Fatalf("ListBundles on missing dir = %+v, %v", bundles, err)
	}

	// Write two bundle files and one non-bundle.
	names := []string{"repo_20260904_120000_a1b2c3d4.bundle", "other.bundle", "notes.txt"}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	bundles, err = fs.ListBundles(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(bundles) != 2 {
		t.Fatalf("expected 2 bundles, got %d: %+v", len(bundles), bundles)
	}
	if !fs.Exists(dir, names[0]) || !fs.Exists(dir, "notes.txt") || fs.Exists(dir, "missing.bundle") {
		t.Fatal("Exists returned wrong result")
	}

	// Remove → present until removed.
	if err := fs.RemoveLocal(filepath.Join(dir, names[0])); err != nil {
		t.Fatal(err)
	}
	if fs.Exists(dir, names[0]) {
		t.Fatal("bundle should be gone after RemoveLocal")
	}
	// Removal again → ErrAlreadyDeleted.
	if err := fs.RemoveLocal(filepath.Join(dir, names[0])); !errors.Is(err, ErrAlreadyDeleted) {
		t.Fatalf("second remove err = %v, want ErrAlreadyDeleted", err)
	}
}
