package tokenstore

import (
	"encoding/json"
	"os"
	"testing"
)

func TestTokenCiphertextBoundToAAD(t *testing.T) {
	key, _ := deriveTokenKey("k")
	nonce, ct, err := encryptToken(key, "secret", []byte("user-1|github.1"))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := decryptToken(key, nonce, ct, []byte("user-1|github.1"), false); err != nil || got != "secret" {
		t.Fatalf("round trip = %q, %v", got, err)
	}
	// A row moved to another user/reference must not decrypt, even with the
	// legacy fallback enabled (the fallback only accepts aad-less ciphertexts).
	if _, err := decryptToken(key, nonce, ct, []byte("user-2|github.2"), true); err == nil {
		t.Fatal("ciphertext decrypted under a different aad")
	}
}

func TestLegacyCiphertextStillDecrypts(t *testing.T) {
	key, _ := deriveTokenKey("k")
	nonce, ct, err := encryptToken(key, "old", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := decryptToken(key, nonce, ct, []byte("user-1|github.1"), true); err != nil || got != "old" {
		t.Fatalf("legacy decrypt = %q, %v", got, err)
	}
	if _, err := decryptToken(key, nonce, ct, []byte("user-1|github.1"), false); err == nil {
		t.Fatal("legacy fallback must be opt-in")
	}
}

func TestFileStoreReadsVersion1Files(t *testing.T) {
	dir := t.TempDir()
	st, err := NewFileStore(dir, "k")
	if err != nil {
		t.Fatal(err)
	}
	nonce, ct, err := encryptToken(st.key, "legacy-token", nil)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(fileToken{Version: 1, Nonce: nonce, Ciphertext: ct})
	if err := os.WriteFile(st.pathFor("github.7"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := st.Get("github.7"); err != nil || got != "legacy-token" {
		t.Fatalf("Get = %q, %v", got, err)
	}
	// A version 2 file copied to another reference fails.
	if err := st.Set("github.8", "new-token"); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(st.pathFor("github.8"))
	if err := os.WriteFile(st.pathFor("github.9"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Get("github.9"); err == nil {
		t.Fatal("a token file copied to another reference must not decrypt")
	}
}
