package server

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/b-isry/gitsafe/internal/state"
	"github.com/b-isry/gitsafe/internal/tokenstore"
)

// TestBackupConcurrencyCapped verifies that starting more protected backups than
// the global concurrency limit never runs more mirrors at once: the excess jobs
// wait in the enqueued state and drain as slots free up.
func TestBackupConcurrencyCapped(t *testing.T) {
	st := &fakeStateStore{}
	const total = maxConcurrentBackups + 2
	for i := 0; i < total; i++ {
		seedProtectedRepo(st, string(rune('a'+i)), "acme/repo-"+string(rune('a'+i)), "main")
	}
	tk := newFakeTokenStore()
	tk.data[tokenstore.GitHubToken] = "tok"
	st.SetGitHubConnection(state.GitHubConnection{Login: "octocat", TokenRef: tokenstore.GitHubToken})
	s := newCloudServer(t, st, tk, &GitHubOAuth{ClientID: "id"})

	var inFlight atomic.Int32
	var exceeded atomic.Bool
	release := make(chan struct{})
	s.bundleBackup = func(ctx context.Context, fullName, token, output string) (bundleOutcome, error) {
		got := inFlight.Add(1)
		if got > maxConcurrentBackups {
			exceeded.Store(true)
		}
		<-release
		inFlight.Add(-1)
		return bundleOutcome{BundlePath: "/out/x.bundle", BundleName: "x_20260904_120000_a1b2c3d4.bundle", SizeBytes: 1, SHA256: "a"}, nil
	}

	for i := 0; i < total; i++ {
		if _, err := s.startProtectedBackup(string(rune('a' + i))); err != nil {
			t.Fatalf("start %d: %v", i, err)
		}
	}

	// Wait until the semaphore has saturated.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && inFlight.Load() != maxConcurrentBackups {
		time.Sleep(10 * time.Millisecond)
	}
	if inFlight.Load() != maxConcurrentBackups {
		t.Fatalf("in-flight = %d, want %d", inFlight.Load(), maxConcurrentBackups)
	}
	if exceeded.Load() {
		t.Fatal("concurrency limit was exceeded")
	}

	close(release)

	// All jobs must eventually complete.
	deadline = time.Now().Add(10 * time.Second)
	for {
		jobs := st.BackupJobs()
		done := 0
		for _, j := range jobs {
			if j.State == state.JobCompleted {
				done++
			}
		}
		if done == total {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d/%d jobs completed; jobs=%+v", done, total, jobs)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if exceeded.Load() {
		t.Fatal("concurrency limit was exceeded")
	}
}
