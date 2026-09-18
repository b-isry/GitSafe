package server

import (
	"sync"

	"github.com/b-isry/gitsafe/internal/config"
)

// App holds the live configuration shared by the GitSafe web application. It no
// longer performs local filesystem scanning: GitSafe is GitHub-only, so the App
// is a thin, thread-safe holder for the running config and derived settings
// (notably the bundle output directory used by protected-repo backups).
type App struct {
	Config     config.Config
	OutputPath string

	mu sync.Mutex
}

// ConfigSnapshot returns a copy of the current configuration under the app lock.
// A long-lived goroutine (e.g. a detached backup job) must use this rather than
// reading App.Config directly because the config never changes after startup.
func (a *App) ConfigSnapshot() config.Config {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.Config
}
