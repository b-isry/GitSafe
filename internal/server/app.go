package server

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/b-isry/gitsafe/internal/backup"
	"github.com/b-isry/gitsafe/internal/config"
)

// ErrRootUnconfigured is returned when repository discovery is requested before
// the user has chosen a root directory. The server gate redirects to the
// first-run setup page instead of scanning in this state.
var ErrRootUnconfigured = errors.New("root path is not configured")

// App configures the GitSafe web application and provides access to the real
// repository, backup and backup-run state.
//
// Repository discovery is decoupled from HTTP request lifetimes: scans run
// against their own internal context and the result is cached, so page loads
// and the settings save round-trip never block on a filesystem walk or get
// aborted by a disconnecting client. Reads always return whatever snapshot is
// cached (kicking off a refresh only when the cache has gone stale), which is
// what keeps settings saves fast and user-facing pages calm.
type App struct {
	Logger     *slog.Logger
	Config     config.Config
	Runner     backup.Runner
	OutputPath string
	Threshold  int

	mu       sync.Mutex
	cache    []RepositoryStatus
	cachedAt time.Time
	scanning bool
	lastScan time.Time
	skipped  []SkippedRepo
}

// RepoCacheTTL bounds how often a full filesystem scan is repeated in the
// background. Reads always serve the cached snapshot regardless of age.
const RepoCacheTTL = 10 * time.Second

// scanTimeout bounds a whole filesystem scan. Inspecting a very large tree with
// the 8-worker pool stays well under this in practice.
const scanTimeout = 10 * time.Minute

// SkippedRepo is a repository that could not be inspected during a scan. It is
// recorded server-side so the UI can surface a friendly count without leaking
// raw git errors.
type SkippedRepo struct {
	Path string
	Err  string
}

// RepositoryStatus is a single repository discovered by scanning.
type RepositoryStatus struct {
	Path       string
	Name       string
	Source     string
	LastCommit time.Time
	StaleDays  int
	Stale      bool
	BackedUp   bool
	LastBundle *StoredBackup
}

// StoredBackup is a bundle discovered on disk under the output directory.
type StoredBackup struct {
	RepoName  string
	Path      string
	Filename  string
	SizeBytes int64
	Created   time.Time
}

// ApplySettings swaps the running configuration with freshly-loaded settings,
// keeping the derived fields (output path and staleness threshold) in sync,
// invalidating the repository scan cache and starting a detached background
// re-scan. The caller never waits for the scan here; the next page load serves
// whatever snapshot exists by then.
func (a *App) ApplySettings(cfg config.Config) {
	if abs, err := filepath.Abs(cfg.OutputPath); err == nil {
		cfg.OutputPath = abs
	}
	a.mu.Lock()
	a.Config = cfg
	a.OutputPath = cfg.OutputPath
	a.Threshold = cfg.Days
	a.cache = nil
	a.cachedAt = time.Time{}
	a.mu.Unlock()
	a.startRefresh()
}

// ConfigSnapshot returns a copy of the current configuration under the app lock.
// A long-lived goroutine (e.g. a detached backup job) must use this rather than
// reading App.Config directly, because ApplySettings may swap the config
// concurrently with a settings save.
func (a *App) ConfigSnapshot() config.Config {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.Config
}

// ScanStatus reports the current state of repository discovery for the UI.
func (a *App) ScanStatus() (scanning bool, lastScan time.Time, skipped int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.scanning, a.lastScan, len(a.skipped)
}

// SkippedRepos returns the currently-known list of unreadable repositories.
func (a *App) SkippedRepos() []SkippedRepo {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]SkippedRepo, len(a.skipped))
	copy(out, a.skipped)
	return out
}

// Repos scans rootPath for git repositories and reports their staleness against
// the configured threshold. It reuses the same full-history semantics as the
// scanner package (looking for .git directories + `git log -1`).
// scanSkipDirs contains directory names that are never valid repository roots and
// are typically very large. Skipping them keeps scans fast on real-world trees.
var scanSkipDirs = map[string]bool{
	"node_modules": true,
	"vendor":       true,
	"target":       true,
	"dist":         true,
	"build":        true,
	"out":          true,
	"__pycache__":  true,
	".venv":        true,
	"venv":         true,
	".tox":         true,
	".next":        true,
	".cargo":       true,
	".terraform":   true,
}

// Repos returns the cached repository snapshot. Reads never block on a scan:
// with a populated cache they return immediately (refresh happens in the
// background once the cache is older than RepoCacheTTL); with an empty cache
// (first scan after boot or after a settings change) the scan runs once,
// synchronously, against an internal context so a client disconnecting cannot
// cancel it.
func (a *App) Repos(ctx context.Context) ([]RepositoryStatus, error) {
	root := a.Config.RootPath
	if root == "" {
		return nil, ErrRootUnconfigured
	}

	a.mu.Lock()
	cached := a.cache
	if cached != nil {
		stale := time.Since(a.cachedAt) >= RepoCacheTTL
		a.mu.Unlock()
		if stale {
			a.startRefresh()
		}
		return cached, nil
	}
	a.mu.Unlock()

	// Nothing cached yet. Run one scan inline (bounded, detached from any
	// request context) — or wait for the background refresh already in flight.
	return a.scanInline()
}

// scanInline performs a scan synchronously when no snapshot exists, or waits
// for an in-flight background scan to populate the cache.
func (a *App) scanInline() ([]RepositoryStatus, error) {
	a.mu.Lock()
	if !a.scanning {
		a.scanning = true
		a.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), scanTimeout)
		defer cancel()
		repos, skipped := a.scan(ctx)
		a.mu.Lock()
		a.cache = repos
		a.cachedAt = time.Now()
		a.lastScan = time.Now()
		a.skipped = skipped
		a.scanning = false
		a.mu.Unlock()
		return repos, nil
	}
	a.mu.Unlock()

	deadline := time.Now().Add(scanTimeout)
	for {
		a.mu.Lock()
		done := !a.scanning
		c := a.cache
		a.mu.Unlock()
		if done || time.Now().After(deadline) {
			return c, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// StartRefresh kicks off a detached background scan. Only one scan runs at a
// time; concurrent calls are coalesced into the single in-flight one.
func (a *App) StartRefresh() {
	a.startRefresh()
}

func (a *App) startRefresh() {
	a.mu.Lock()
	if a.scanning {
		a.mu.Unlock()
		return
	}
	a.scanning = true
	a.mu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), scanTimeout)
		defer cancel()
		repos, skipped := a.scan(ctx)
		a.mu.Lock()
		a.cache = repos
		a.cachedAt = time.Now()
		a.lastScan = time.Now()
		a.skipped = skipped
		a.scanning = false
		a.mu.Unlock()
	}()
}

// scan walks the configured root for git repositories, inspects each one in a
// worker pool and reports staleness against the configured threshold. It runs
// with its own internal context, never with a request context, so client
// disconnects cannot abort it mid-walk. Repositories that cannot be inspected
// are skipped: the error is logged server-side and recorded for a friendly
// count in the UI — it never crashes the scan or leaks raw git output.
func (a *App) scan(ctx context.Context) ([]RepositoryStatus, []SkippedRepo) {
	if fi, err := os.Stat(a.Config.RootPath); err != nil || !fi.IsDir() {
		a.Logger.Warn("root path is not a directory", "path", a.Config.RootPath, "error", err)
		return nil, nil
	}

	root := a.Config.RootPath
	var paths []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !d.IsDir() {
			return nil
		}
		if scanSkipDirs[d.Name()] {
			return filepath.SkipDir
		}
		if d.Name() != ".git" {
			return nil
		}
		paths = append(paths, filepath.Dir(path))
		return filepath.SkipDir
	})
	if err != nil {
		if ctx.Err() != nil {
			a.Logger.Warn("repository scan canceled", "error", err)
			return nil, nil
		}
		a.Logger.Warn("repository scan walk failed", "error", err)
		return nil, nil
	}

	// Inspect repositories concurrently so a large tree does not serialize git
	// process startup, which dominates on Windows.
	const workers = 8
	sem := make(chan struct{}, workers)
	results := make([]RepositoryStatus, len(paths))
	skipped := make([]SkippedRepo, 0, 8)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i, p := range paths {
		wg.Add(1)
		go func(i int, p string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			last, staleDays, err := lastCommit(ctx, p)
			if err != nil {
				a.Logger.Warn("skipping unreadable repository", "repo", p, "error", err)
				mu.Lock()
				if len(skipped) < 100 {
					skipped = append(skipped, SkippedRepo{Path: p, Err: err.Error()})
				}
				mu.Unlock()
				return
			}
			results[i] = RepositoryStatus{
				Path:       p,
				Name:       filepath.Base(p),
				Source:     "local",
				LastCommit: last,
				StaleDays:  staleDays,
				Stale:      staleDays > a.Threshold,
			}
		}(i, p)
	}
	wg.Wait()

	var repos []RepositoryStatus
	for _, r := range results {
		if r.Path != "" {
			repos = append(repos, r)
		}
	}
	return repos, skipped
}

// Backups scans the output directory for bundle files. A bundle is attributed to
// a repository by the repository-name prefix of its filename.
func (a *App) Backups(ctx context.Context) ([]StoredBackup, error) {
	entries, err := os.ReadDir(a.OutputPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []StoredBackup
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".bundle") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, StoredBackup{
			RepoName:  strings.SplitN(e.Name(), "_", 2)[0],
			Path:      filepath.Join(a.OutputPath, e.Name()),
			Filename:  e.Name(),
			SizeBytes: info.Size(),
			Created:   info.ModTime(),
		})
	}
	return out, nil
}

// gitTimeout bounds per-repository inspection so a hung or oversized repository
// cannot stall the whole scan.
const gitTimeout = 10 * time.Second

func lastCommit(ctx context.Context, repoPath string) (time.Time, int, error) {
	timeout, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(timeout, "git", "log", "-1", "--format=%cI")
	cmd.Dir = repoPath
	out, err := cmd.Output()
	if err != nil {
		return time.Time{}, 0, err
	}
	ts, err := time.Parse(time.RFC3339, strings.TrimSpace(string(out)))
	if err != nil {
		return time.Time{}, 0, err
	}
	days := int(time.Since(ts).Hours() / 24)
	if days < 0 {
		days = 0
	}
	return ts, days, nil
}
