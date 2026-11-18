package tokenstore

import (
	"errors"
	"testing"

	"github.com/zalando/go-keyring"
)

func TestKeyringStoreSetGetDelete(t *testing.T) {
	keyring.MockInit()
	store := New()

	const ref = "test.ref"
	if err := store.Set(ref, "secret-token"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := store.Get(ref)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "secret-token" {
		t.Fatalf("Get returned %q, want %q", got, "secret-token")
	}
	if err := store.Delete(ref); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.Get(ref); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after delete = %v, want ErrNotFound", err)
	}
}

func TestKeyringStoreGetMissing(t *testing.T) {
	keyring.MockInit()
	store := New()
	if _, err := store.Get("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get missing = %v, want ErrNotFound", err)
	}
}

func TestKeyringStoreDeleteMissingIsIdempotent(t *testing.T) {
	keyring.MockInit()
	store := New()
	if err := store.Delete("missing"); err != nil {
		t.Fatalf("Delete missing: want nil, got %v", err)
	}
}

func TestKeyringStoreRejectsEmptyArgs(t *testing.T) {
	keyring.MockInit()
	store := New()
	if err := store.Set("", "v"); err == nil {
		t.Fatal("Set with empty ref: want error")
	}
	if err := store.Set("r", ""); err == nil {
		t.Fatal("Set with empty value: want error")
	}
	if _, err := store.Get(""); err == nil {
		t.Fatal("Get with empty ref: want error")
	}
	if err := store.Delete(""); err == nil {
		t.Fatal("Delete with empty ref: want error")
	}
}

func TestKeyringStoreDistinctRefsAreIsolated(t *testing.T) {
	keyring.MockInit()
	store := New()
	if err := store.Set("a", "va"); err != nil {
		t.Fatal(err)
	}
	if err := store.Set("b", "vb"); err != nil {
		t.Fatal(err)
	}
	ga, _ := store.Get("a")
	gb, _ := store.Get("b")
	if ga != "va" || gb != "vb" {
		t.Fatalf("refs not isolated: a=%q b=%q", ga, gb)
	}
}

func TestKeyringStoreSurfacesKeyringError(t *testing.T) {
	boom := errors.New("keyring down")
	keyring.MockInitWithError(boom)
	store := New()
	if err := store.Set("r", "v"); !errors.Is(err, boom) {
		t.Fatalf("Set error = %v, want wrapped %v", err, boom)
	}
	if _, err := store.Get("r"); !errors.Is(err, boom) {
		t.Fatalf("Get error = %v, want wrapped %v", err, boom)
	}
	if err := store.Delete("r"); !errors.Is(err, boom) {
		t.Fatalf("Delete error = %v, want wrapped %v", err, boom)
	}
}
