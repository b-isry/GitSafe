package state

import (
	"fmt"
	"os"
	"path/filepath"
)

type persistenceBackend interface {
	load() ([]byte, bool, error)
	save([]byte) error
}

type filePersistenceBackend struct {
	path string
}

func (f *filePersistenceBackend) load() ([]byte, bool, error) {
	data, err := os.ReadFile(f.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("state: read %q: %w", f.path, err)
	}
	return data, true, nil
}

func (f *filePersistenceBackend) save(data []byte) error {
	parent := filepath.Dir(f.path)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("state: create directory %q: %w", parent, err)
	}

	tmp, err := os.CreateTemp(parent, ".gitsafe-state-*.tmp")
	if err != nil {
		return fmt.Errorf("state: create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		if tmp != nil {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("state: write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("state: sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("state: close temp file: %w", err)
	}
	if err := os.Rename(tmpName, f.path); err != nil {
		return fmt.Errorf("state: replace %q: %w", f.path, err)
	}
	tmp = nil
	return nil
}

func openStore(location string, backend persistenceBackend) (*Store, error) {
	s := &Store{path: location, backend: backend, doc: cleanDocument()}
	data, found, err := backend.load()
	if err != nil {
		return nil, err
	}
	if !found {
		return s, nil
	}
	if err := s.decode(data); err != nil {
		return nil, err
	}
	s.normalizeInterrupted()
	return s, nil
}
