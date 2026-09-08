package history

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

const fixedLimit = 50

func openTest(t *testing.T, dir string, limit func() int, wantFiles ...string) *Store {
	t.Helper()
	path := filepath.Join(dir, "cleanup-history.json")
	s, err := Open(path, limit, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

func entry(n int) Entry {
	base := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	return Entry{
		StartedAt:    base.Add(-time.Minute),
		FinishedAt:   base,
		Trigger:      "manual",
		Success:      true,
		Inspected:    n,
		Retained:     n,
		LocalDeleted: n,
		Errors:       []string{"boom " + string(rune('a'+n))},
	}
}

func TestAddNewestFirstAndBounded(t *testing.T) {
	dir := t.TempDir()
	s := openTest(t, dir, func() int { return 3 })

	for i := 0; i < 6; i++ {
		if err := s.Add(entry(i)); err != nil {
			t.Fatalf("Add(%d): %v", i, err)
		}
	}
	recent := s.Recent(10)
	if len(recent) != 3 {
		t.Fatalf("len = %d, want 3 (bounded)", len(recent))
	}
	// Newest first.
	if recent[0].Inspected != 5 || recent[2].Inspected != 3 {
		t.Fatalf("ordering wrong: %+v", recent)
	}
}

func TestOpenReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cleanup-history.json")
	s, err := Open(path, func() int { return 10 }, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := s.Add(entry(i)); err != nil {
			t.Fatalf("Add(%d): %v", i, err)
		}
	}

	// Fresh store over the same path reloads what was persisted.
	s2, err := Open(path, func() int { return 10 }, nil)
	if err != nil {
		t.Fatalf("Open 2: %v", err)
	}
	recent := s2.Recent(10)
	if len(recent) != 3 {
		t.Fatalf("reloaded len = %d, want 3", len(recent))
	}
	if recent[0].Inspected != 2 || !recent[0].Success {
		t.Fatalf("reloaded newest = %+v", recent[0])
	}
	if len(recent[0].Errors) != 1 {
		t.Fatalf("errors not preserved: %+v", recent[0])
	}
}

func TestMissingFileStartsClean(t *testing.T) {
	dir := t.TempDir()
	s := openTest(t, filepath.Join(dir, "nope"), func() int { return 10 })
	if s.Len() != 0 {
		t.Fatalf("len = %d, want 0", s.Len())
	}
}

func TestCorruptFileStartsClean(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cleanup-history.json")
	if err := os.WriteFile(path, []byte("{ not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path, func() int { return 10 }, nil)
	if err != nil {
		t.Fatalf("Open on corrupt file must not fail: %v", err)
	}
	if s.Len() != 0 {
		t.Fatalf("len = %d, want 0", s.Len())
	}
	// The corrupt file survives until the first Add replaces it.
	raw, _ := os.ReadFile(path)
	if string(raw) != "{ not json" {
		t.Fatalf("corrupt file mutated by Open: %q", raw)
	}

	// A subsequent Add repairs the file atomically.
	if err := s.Add(entry(1)); err != nil {
		t.Fatalf("Add after corruption: %v", err)
	}
	recent := s.Recent(1)
	if len(recent) != 1 || recent[0].Inspected != 1 {
		t.Fatalf("post-corruption history wrong: %+v", recent)
	}
}

func TestWrongVersionStartsClean(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cleanup-history.json")
	if err := os.WriteFile(path, []byte(`{"version": 99, "entries": []}`), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path, func() int { return 10 }, nil)
	if err != nil {
		t.Fatalf("Open on wrong version must not fail: %v", err)
	}
	if s.Len() != 0 {
		t.Fatalf("len = %d, want 0", s.Len())
	}
}

func TestEmptyFileStartsClean(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cleanup-history.json")
	if err := os.WriteFile(path, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path, func() int { return 10 }, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.Len() != 0 {
		t.Fatalf("len = %d, want 0", s.Len())
	}
}

func TestDisabledLimitDropsAndDoesNotWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cleanup-history.json")
	s, err := Open(path, func() int { return 0 }, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Add(entry(1)); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if s.Len() != 0 {
		t.Fatalf("len = %d, want 0 (disabled)", s.Len())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("disabled history must not be written to disk")
	}
	if s.Recent(10) != nil {
		t.Fatalf("Recent with disabled limit must return nil")
	}
}

func TestLiveLimitChange(t *testing.T) {
	dir := t.TempDir()
	current := 3
	s := openTest(t, dir, func() int { return current })

	for i := 0; i < 3; i++ {
		if err := s.Add(entry(i)); err != nil {
			t.Fatalf("Add(%d): %v", i, err)
		}
	}
	current = 0 // disable at runtime
	if err := s.Add(entry(99)); err != nil {
		t.Fatalf("Add disabled: %v", err)
	}
	if s.Recent(10) != nil {
		t.Fatalf("Recent after disable must return nil")
	}
	if s.Len() != 3 {
		t.Fatalf("in-memory len = %d, want 3 (existing entries kept)", s.Len())
	}
}

func TestAtomicWriteNoTempLeftover(t *testing.T) {
	dir := t.TempDir()
	s := openTest(t, dir, func() int { return 10 })
	if err := s.Add(entry(1)); err != nil {
		t.Fatalf("Add: %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(dir, ".cleanup-history-*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("temp files left behind: %v", matches)
	}
}

func TestRecentDefensiveCopy(t *testing.T) {
	dir := t.TempDir()
	s := openTest(t, dir, func() int { return 10 })
	if err := s.Add(entry(1)); err != nil {
		t.Fatalf("Add: %v", err)
	}
	got := s.Recent(1)
	got[0].Errors[0] = "mutated"
	if s.Recent(1)[0].Errors[0] == "mutated" {
		t.Fatal("Recent returned a shared slice")
	}
}

func TestRecentCap(t *testing.T) {
	dir := t.TempDir()
	s := openTest(t, dir, func() int { return 10 })
	for i := 0; i < 5; i++ {
		if err := s.Add(entry(i)); err != nil {
			t.Fatalf("Add(%d): %v", i, err)
		}
	}
	if got := s.Recent(2); len(got) != 2 {
		t.Fatalf("Recent(2) = %d entries", len(got))
	}
	if got := s.Recent(0); got != nil {
		t.Fatal("Recent(0) must be nil")
	}
}

func TestMalformedEntryDropped(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cleanup-history.json")
	data := `{"version":1,"entries":[
		{"startedAt":"2026-09-06T11:00:00Z","trigger":"manual"},
		{"startedAt":"2026-09-06T11:00:00Z","finishedAt":"2026-09-06T12:00:00Z","trigger":"scheduled","success":true,"inspected":2,"retained":2}
	]}`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path, func() int { return 10 }, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	recent := s.Recent(10)
	if len(recent) != 1 {
		t.Fatalf("len = %d, want 1 (zero-FinishedAt entry dropped)", len(recent))
	}
	if recent[0].Trigger != "scheduled" {
		t.Fatalf("survivor wrong: %+v", recent[0])
	}
}
