// Package schedule provides a minimal, deterministic, testable periodic task
// runner used to drive GitSafe's retention cleanup on a configured interval.
//
// The scheduler owns only *when* a task runs; the task itself (what to run) is
// injected as a callback. It deliberately does not depend on the server, state,
// or retention packages so it can be unit-tested with a fake clock and is
// suitable for any periodic job.
package schedule

import (
	"context"
	"sync"
	"time"
)

// Clock abstracts time so tests can drive the scheduler deterministically
// without sleeping. The zero implementation substitution is the wall clock.
type Clock interface {
	Now() time.Time
	// NewTicker returns a ticker that delivers on C() every d.
	NewTicker(d time.Duration) Ticker
}

// Ticker is the subset of *time.Ticker the scheduler needs.
type Ticker interface {
	C() <-chan time.Time
	Stop()
}

type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now() }

func (wallClock) NewTicker(d time.Duration) Ticker {
	return wallTicker{ticker: time.NewTicker(d)}
}

type wallTicker struct {
	ticker *time.Ticker
}

func (t wallTicker) C() <-chan time.Time { return t.ticker.C }
func (t wallTicker) Stop()               { t.ticker.Stop() }

// Scheduler fires Run periodically on a single loop. Each run is started on its
// own goroutine and runs never overlap: a tick that fires while a previous run
// is still active is skipped (dropped), matching a time.Ticker's buffered
// behavior. Stop halts the loop and waits for any in-flight run to complete, so
// no goroutine is leaked and callers can shut down cleanly.
//
// A Scheduler has a one-shot lifecycle: Start is a no-op if it is already
// running or has already been stopped. Callers that need to change the interval
// or callback replace the Scheduler instance instead (the server does this on
// setting changes).
type Scheduler struct {
	// Interval is the delay between tick firings. Must be positive.
	Interval time.Duration
	// Run is invoked (in a new goroutine) on each tick. Errors are left to the
	// caller's own logging; a failing Run never stops the scheduler.
	Run func(ctx context.Context) error
	// Recover handles the value recovered from a panicking Run. A panic in Run
	// is always recovered (the scheduler loop keeps ticking regardless) but if
	// Recover is nil the panic value is silently dropped. The server installs a
	// Recover that logs loudly and records a failed cleanup entry, so a buggy
	// scheduled run is observable without ever taking the process down.
	Recover func(v any)
	// Clock is how the scheduler reads time and creates its ticker. When nil the
	// wall clock is used.
	Clock Clock

	mu      sync.Mutex
	stop    chan struct{} // nil while stopped
	done    bool          // permanently stopped after Stop
	running bool          // a run is currently in flight
	wg      sync.WaitGroup
}

// Start launches the scheduler loop. It is a no-op when the scheduler is
// already running or has been stopped. The ticker is created synchronously so
// callers (and tests with a fake clock) can rely on it being registered by the
// time Start returns. A non-positive Interval marks the scheduler started but
// never fires (defensive; the server never schedules with one).
func (s *Scheduler) Start() {
	s.mu.Lock()
	if s.stop != nil || s.done {
		s.mu.Unlock()
		return
	}
	stop := make(chan struct{})
	s.stop = stop
	s.mu.Unlock()

	if s.Interval <= 0 {
		return
	}

	clock := s.Clock
	if clock == nil {
		clock = wallClock{}
	}
	ticker := clock.NewTicker(s.Interval)

	s.wg.Add(1)
	go s.loop(stop, ticker)
}

// Stop halts the loop and waits for both the loop and any in-flight run to
// finish. Safe to call more than once; calls after the first return quickly.
func (s *Scheduler) Stop() {
	s.mu.Lock()
	if s.stop == nil {
		s.mu.Unlock()
		return
	}
	close(s.stop)
	s.stop = nil
	s.done = true
	s.mu.Unlock()
	s.wg.Wait()
}

// Running reports whether a run is currently in flight. Used by tests (and
// status reporting) to observe concurrency or overlap deterministically.
func (s *Scheduler) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

func (s *Scheduler) loop(stop <-chan struct{}, ticker Ticker) {
	defer s.wg.Done()
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C():
			s.fire()
		case <-stop:
			return
		}
	}
}

// fire launches Run unless a previous run is still active. Runs are serialized:
// a tick arriving while one is in flight is coalesced by dropping it, so Run is
// never executed concurrently with itself.
func (s *Scheduler) fire() {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return
	}
	s.running = true
	s.mu.Unlock()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() {
			s.mu.Lock()
			s.running = false
			s.mu.Unlock()
		}()
		if s.Run == nil {
			return
		}
		// A panicking Run must never kill the loop (or the process): recover
		// unconditionally and hand the value to the optional hook.
		defer func() {
			if v := recover(); v != nil {
				if s.Recover != nil {
					s.Recover(v)
				}
			}
		}()
		_ = s.Run(context.Background())
	}()
}
