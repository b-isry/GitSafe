package retention

import (
	"os"
	"path/filepath"
	"strings"
)

// DiskFS is the production FSOps backed by the real filesystem. It relies on the
// engine's naming-convention and ownership checks for safety, so it only ever
// deletes a path handed to it by the engine.
type DiskFS struct{}

// ListBundles returns every *.bundle file under dir, deferring to an empty list
// when the directory does not exist.
func (DiskFS) ListBundles(dir string) ([]LocalBundle, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]LocalBundle, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".bundle") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, LocalBundle{
			Name:    e.Name(),
			Path:    filepath.Join(dir, e.Name()),
			ModTime: info.ModTime(),
		})
	}
	return out, nil
}

// RemoveLocal removes a bundle file, or returns ErrAlreadyDeleted when it was
// already gone.
func (DiskFS) RemoveLocal(path string) error {
	err := os.Remove(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ErrAlreadyDeleted
		}
		return err
	}
	return nil
}

// Exists reports whether a named bundle file is present in dir.
func (DiskFS) Exists(dir, name string) bool {
	if dir == "" || name == "" {
		return false
	}
	info, err := os.Stat(filepath.Join(dir, name))
	return err == nil && !info.IsDir()
}
