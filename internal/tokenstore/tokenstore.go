// Package tokenstore persists OAuth access tokens behind a uniform Store
// interface. Two backends exist:
//
//   - KeyringStore keeps tokens in the OS keychain through go-keyring (used in
//     local development).
//   - FileStore keeps tokens in AES-256-GCM-encrypted files (used in production
//     deployments, and required there because headless containers usually have
//     no Secret Service/dbus to back the keyring).
//
// GitSafe deliberately does NOT store tokens in config.yaml, state.json, logs,
// cookies, temporary files, or any database/state structure. Only a token
// reference (for example "github.42") lives in the state layer; the token
// value itself is held here and encrypted by the operator-supplied key or the
// operating system.
//
// Token references are per-user (see GitHubTokenFor and DriveTokenFor) so
// distinct accounts never share a storage entry. Backends are selected at
// server construction from the deployment environment; server.New refuses to
// boot if the selected backend fails a Set/Get/Delete self-test.
package tokenstore

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/zalando/go-keyring"
)

// Service is the GitSafe-wide keyring service name. It is centralized here so
// all tokenstore callers share one namespace. Tokens are scoped by user (the
// token reference), so different providers/entities never collide.
const Service = "gitsafe"

// Token references used by GitSafe. These are the only values that may appear
// in persistent state; never the tokens themselves.
//
// GitHubToken and DriveToken are the legacy single-tenant references. Current
// code stores per-user references built by GitHubTokenFor/DriveTokenFor so
// every account uses an isolated storage key, both on the OS keyring and in the
// encrypted file backend.
const (
	// GitHubToken is the legacy token reference for the connected GitHub account.
	GitHubToken = "github.primary"
	// DriveToken is the legacy token reference for the connected Google Drive
	// account. It stores the OAuth refresh token, from which fresh access
	// tokens are minted on demand for detached backup work.
	DriveToken = "drive.primary"
)

// GitHubTokenFor returns the per-user token reference for a GitHub account.
func GitHubTokenFor(userID int64) string {
	return "github." + strconv.FormatInt(userID, 10)
}

// DriveTokenFor returns the per-user token reference for a Google Drive account.
func DriveTokenFor(userID int64) string {
	return "drive." + strconv.FormatInt(userID, 10)
}

// ErrNotFound is returned when no token exists for the given reference.
var ErrNotFound = keyring.ErrNotFound

// Store is the minimal token persistence surface. The rest of GitSafe depends
// on this interface, not on the keyring details.
type Store interface {
	// Set stores a token value under the given reference.
	Set(ref, value string) error
	// Get returns the token value for the given reference.
	Get(ref string) (string, error)
	// Delete removes the token for the given reference.
	Delete(ref string) error
}

// KeyringStore stores tokens in the OS keychain.
type KeyringStore struct{}

// New returns a KeyringStore backed by the OS keychain. It reads no secrets
// from configuration.
func New() *KeyringStore { return &KeyringStore{} }

// Set stores value under reference in the OS keychain.
func (k *KeyringStore) Set(ref, value string) error {
	if ref == "" {
		return errors.New("tokenstore: empty reference")
	}
	if value == "" {
		return errors.New("tokenstore: empty token value")
	}
	if err := keyring.Set(Service, ref, value); err != nil {
		return fmt.Errorf("tokenstore: set %q: %w", ref, err)
	}
	return nil
}

// Get returns the token stored under reference, or ErrNotFound.
func (k *KeyringStore) Get(ref string) (string, error) {
	if ref == "" {
		return "", errors.New("tokenstore: empty reference")
	}
	v, err := keyring.Get(Service, ref)
	if err != nil {
		if errors.Is(err, keyring.ErrNotFound) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("tokenstore: get %q: %w", ref, err)
	}
	return v, nil
}

// Delete removes the token stored under reference. It is idempotent: deleting
// a non-existent token returns nil.
func (k *KeyringStore) Delete(ref string) error {
	if ref == "" {
		return errors.New("tokenstore: empty reference")
	}
	if err := keyring.Delete(Service, ref); err != nil {
		// Deleting a missing token is a no-op success.
		if errors.Is(err, keyring.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("tokenstore: delete %q: %w", ref, err)
	}
	return nil
}
