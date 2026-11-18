package server

import (
	"database/sql"
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
	// config file) is used.
	TokenDataDirEnv = "GITSAFE_DATA_DIR"

	DatabaseURLEnv = "DATABASE_URL"
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

func validateDeploymentEnvironment(baseURL string) error {
	production := !IsLoopbackHost(baseURL)
	databaseConfigured := strings.TrimSpace(os.Getenv(DatabaseURLEnv)) != ""
	tokenKeyConfigured := os.Getenv(TokenKeyEnv) != ""
	if production {
		var missing []string
		if !databaseConfigured {
			missing = append(missing, DatabaseURLEnv)
		}
		if !tokenKeyConfigured {
			missing = append(missing, TokenKeyEnv)
		}
		if len(missing) > 0 {
			return fmt.Errorf("server: refusing to boot production deployment: %s required", strings.Join(missing, " and "))
		}
		return nil
	}
	if databaseConfigured && !tokenKeyConfigured {
		return fmt.Errorf("server: refusing to boot with PostgreSQL persistence: %s is required", TokenKeyEnv)
	}
	return nil
}

func tokenStoreFactoryFromEnv(db *sql.DB, defaultDir string) (TokenStoreFactory, error) {
	key := os.Getenv(TokenKeyEnv)
	if db != nil {
		return func(userID int64) (TokenStore, error) {
			return tokenstore.NewPostgresStore(db, userID, key)
		}, nil
	}
	if key == "" {
		return func(int64) (TokenStore, error) { return tokenstore.New(), nil }, nil
	}
	dir := os.Getenv(TokenDataDirEnv)
	if dir == "" {
		dir = defaultDir
	}
	st, err := tokenstore.NewFileStore(dir, key)
	if err != nil {
		return nil, fmt.Errorf("file store: %w", err)
	}
	return func(int64) (TokenStore, error) { return st, nil }, nil
}
