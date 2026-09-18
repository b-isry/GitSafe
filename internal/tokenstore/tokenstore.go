// Package tokenstore persists OAuth access tokens in the OS keychain through
// the go-keyring library.
//
// GitSafe deliberately does NOT store tokens in config.yaml, state.json, logs,
// cookies, temporary files, or any database/state structure. Only a token
// reference (for example "github.primary") lives in the state layer; the token
// value itself is held here and encrypted by the operating system.
//
// The OS keyring is the only storage backend. There is intentionally no
// encrypted-file fallback: if the keyring is unavailable, calls fail with an
// explicit error and the caller surfaces a "reconnect" state rather than
// silently downgrading to a weaker store.
package tokenstore

import (
	"errors"
	"fmt"

	"github.com/zalando/go-keyring"
)

// Service is the GitSafe-wide keyring service name. It is centralized here so
// all tokenstore callers share one namespace. Tokens are scoped by user (the
// token reference), so different providers/entities never collide.
const Service = "gitsafe"

// Token references used across GitSafe. These are the only values that may
// appear in persistent state; never the tokens themselves.
const (
	// GitHubToken is the token reference for the connected GitHub account.
	GitHubToken = "github.primary"
	// DriveToken is the token reference for the connected Google Drive account.
	// It stores the OAuth refresh token, from which fresh access tokens are
	// minted on demand for detached backup work.
	DriveToken = "drive.primary"
)

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
