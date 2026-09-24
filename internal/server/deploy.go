package server

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"

	"github.com/b-isry/gitsafe/internal/tokenstore"
)

// Deployment-level environment variables for token persistence. These are
// operator-owned and never read from config files or surfaced to the UI.
const (
	// TokenKeyEnv supplies the key material for the encrypted file token store.
	// It is REQUIRED in production (any non-loopback base URL): server boot
	// refuses to start without it so deployments cannot silently degrade to
	// echoes of plaintext-critical errors at login time.
	TokenKeyEnv = "GITSAFE_TOKEN_KEY"

	// TokenDataDirEnv overrides the directory where the encrypted token files
	// live. When unset, the server's state base path (the directory of the
	// config file) is used. On Render, mount the persistent disk here.
	TokenDataDirEnv = "GITSAFE_DATA_DIR"
)

// IsLoopbackHost reports whether baseURL points at the local machine
// (localhost or a loopback IP). Non-loopback base URLs are treated as
// production deployments.
func IsLoopbackHost(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// tokenStoreFactoryFromEnv selects the token backend from the deployment
// environment:
//
//   - GITSAFE_TOKEN_KEY set   → the encrypted FileStore (production).
//   - GITSAFE_TOKEN_KEY unset → the OS keyring (local development).
//
// The file store root is GITSAFE_DATA_DIR when set, otherwise defaultDir (the
// server's state base path).
func tokenStoreFactoryFromEnv(defaultDir string) (TokenStoreFactory, error) {
	key := os.Getenv(TokenKeyEnv)
	if key == "" {
		return func() (TokenStore, error) { return tokenstore.New(), nil }, nil
	}
	dir := os.Getenv(TokenDataDirEnv)
	if dir == "" {
		dir = defaultDir
	}
	st, err := tokenstore.NewFileStore(dir, key)
	if err != nil {
		return nil, fmt.Errorf("file store: %w", err)
	}
	return func() (TokenStore, error) { return st, nil }, nil
}
