package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestSaveRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	cfg := Defaults()
	cfg.Days = 45
	cfg.OutputPath = "C:\\backups"
	cfg.Cloud = CloudConfig{Enabled: true, CredentialsFile: "svc.json"}

	if err := cfg.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	for _, want := range []string{"outputPath", "days: 45", "enabled: true", "svc.json"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("saved config missing %q\n%s", want, raw)
		}
	}
	// The raw credential *contents* must never be persisted via Save; only a path is written.
	if strings.Contains(string(raw), "private_key") {
		t.Errorf("Save persisted raw credential contents")
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.OutputPath != cfg.OutputPath || loaded.Days != 45 {
		t.Errorf("round-trip mismatch: %+v", loaded)
	}
	if !loaded.Cloud.Enabled || loaded.Cloud.CredentialsFile != "svc.json" {
		t.Errorf("cloud round-trip mismatch: %+v", loaded.Cloud)
	}
}

func TestSaveAtomicNoTempLeftover(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	cfg := Defaults()
	if err := cfg.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Errorf("expected no temp file after save, got %q", e.Name())
		}
	}
}

func TestSavePreservesExistingPermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	cfg := Defaults()
	// Some operators gitignore/restrict the config. Whatever mode the target
	// reports must survive the save (the save must not force its own default).
	if err := os.WriteFile(path, []byte("outputPath: C:\\backups\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	cfg.OutputPath = "C:\\backups"
	if err := cfg.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if after.Mode().Perm() != before.Mode().Perm() {
		t.Errorf("save changed config permissions: %#o -> %#o", before.Mode().Perm(), after.Mode().Perm())
	}
}

func TestConcurrentSavesDoNotCorrupt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			cfg := Defaults()
			cfg.OutputPath = fmt.Sprintf("C:\\backups-%d", n)
			if err := cfg.Save(path); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent Save failed: %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("final config unreadable after concurrent saves: %v", err)
	}
	if loaded.OutputPath == "" {
		t.Errorf("config lost data under concurrency: %+v", loaded)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Errorf("temp file left after concurrent saves: %q", e.Name())
		}
	}
}

func TestConfigValidateRequiresOutputPath(t *testing.T) {
	cfg := Defaults()
	cfg.OutputPath = ""
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error for empty outputPath")
	}
}
