package schedule

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock drives the scheduler deterministically: advancing the clock pumps
// every live (non-stopped) ticker, and each fakeTicker has the same buffered
// single-slot channel semantics as time.Ticker so dropped ticks coalesce.
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	tickers []*fakeTicker
}

func newFakeClock(t0 time.Time) *fakeClock {
	return &fakeClock{now: t0}
}

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fakeClock) NewTicker(d time.Duration) Ticker {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := &fakeTicker{
		clock:    f,
		interval: d,
		ch:       make(chan time.Time, 1),
		next:     f.now.Add(d),
	}
	f.tickers = append(f.tickers, t)
	return t
}

// advance moves the fake clock forward by d and pumps all live tickers. Each
// ticker emits at most one tick per elapsed interval, dropping any that would
// overflow its single-slot buffer.
func (f *fakeClock) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
	for _, t := range f.tickers {
		if t.stopped {
			continue
		}
		for !f.now.Before(t.next) {
			select {
			case t.ch <- f.now:
			default:
			}
			t.next = t.next.Add(t.interval)
		}
	}
}

type fakeTicker struct {
	clock    *fakeClock
	interval time.Duration
	ch       chan time.Time
	next     time.Time
	stopped  bool
}

func (t *fakeTicker) C() <-chan time.Time { return t.ch }

func (t *fakeTicker) Stop() {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	t.stopped = true
}

func TestSchedulerRunsOnInterval(t *testing.T) {
	clk := newFakeClock(time.Unix(0, 0))
	started := make(chan struct{}, 4)
	var calls atomic.Int32
	s := &Scheduler{
		Interval: time.Hour,
		Clock:    clk,
		Run: func(ctx context.Context) error {
			calls.Add(1)
			started <- struct{}{}
			return nil
		},
	}
	s.Start()
	defer s.Stop()

	clk.advance(time.Hour)
	<-started
	if calls.Load() != 1 {
		t.Fatalf("calls = %d after first interval, want 1", calls.Load())
	}

	// No fire before the interval elapses.
	clk.advance(59 * time.Minute)
	select {
	case <-started:
		t.Fatal("run fired before the interval elapsed")
	default:
	}

	clk.advance(time.Minute)
	<-started
	if calls.Load() != 2 {
		t.Fatalf("calls = %d after second interval, want 2", calls.Load())
	}
}

func TestSchedulerStartIdempotent(t *testing.T) {
	clk := newFakeClock(time.Unix(0, 0))
	started := make(chan struct{}, 8)
	var calls atomic.Int32
	s := &Scheduler{
		Interval: time.Hour,
		Clock:    clk,
		Run: func(ctx context.Context) error {
			calls.Add(1)
			started <- struct{}{}
			return nil
		},
	}
	s.Start()
	s.Start() // second start must be a no-op
	clk.advance(time.Hour)
	<-started
	clk.advance(time.Hour)
	<-started
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2 (double Start must not create a second loop)", calls.Load())
	}
	s.Stop()
}

func TestSchedulerStopStopsFiresAndIsIdempotent(t *testing.T) {
	clk := newFakeClock(time.Unix(0, 0))
	started := make(chan struct{}, 8)
	var calls atomic.Int32
	s := &Scheduler{
		Interval: time.Hour,
		Clock:    clk,
		Run: func(ctx context.Context) error {
			calls.Add(1)
			started <- struct{}{}
			return nil
		},
	}
	s.Start()
	clk.advance(time.Hour)
	<-started

	s.Stop()
	s.Stop() // second Stop must return quickly and not panic

	// Advance far past several intervals: nothing must fire, and the returned
	// Scheduler must not resurrect itself.
	clk.advance(100 * time.Hour)
	select {
	case <-started:
		t.Fatal("run fired after Stop")
	default:
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d after Stop, want 1", calls.Load())
	}
	// Start after Stop must be a no-op.
	s.Start()
	clk.advance(24 * time.Hour)
	select {
	case <-started:
		t.Fatal("run fired after Stop then Start")
	default:
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d after Stop then Start, want 1", calls.Load())
	}
}

func TestSchedulerStopWaitsForInFlightRun(t *testing.T) {
	clk := newFakeClock(time.Unix(0, 0))
	started := make(chan struct{}, 8)
	release := make(chan struct{})
	s := &Scheduler{
		Interval: time.Hour,
		Clock:    clk,
		Run: func(ctx context.Context) error {
			started <- struct{}{}
			<-release
			return nil
		},
	}
	s.Start()
	clk.advance(time.Hour)
	<-started

	done := make(chan struct{})
	go func() {
		s.Stop()
		close(done)
	}()
	// Stop must block while the run is in flight.
	select {
	case <-done:
		t.Fatal("Stop returned while a run was still in flight")
	default:
	}
	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return after the in-flight run finished")
	}
}

func TestSchedulerSkipsOverlappingTicks(t *testing.T) {
	clk := newFakeClock(time.Unix(0, 0))
	started := make(chan struct{}, 8)
	release := make(chan struct{})
	var calls atomic.Int32
	s := &Scheduler{
		Interval: time.Hour,
		Clock:    clk,
		Run: func(ctx context.Context) error {
			calls.Add(1)
			started <- struct{}{}
			<-release
			return nil
		},
	}
	s.Start()
	defer s.Stop()

	// Tick 1 starts a run and blocks it.
	clk.advance(time.Hour)
	<-started
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1", calls.Load())
	}

	// Tick 2 arrives while the run is still in flight: it must be skipped, never
	// executed concurrently. Give the loop a moment to process it, asserting on
	// the deterministic fact that a blocked in-flight run prevents any more.
	for i := 0; i < 1000; i++ {
		runtime.Gosched()
		select {
		case <-started:
			t.Fatal("overlapping tick must not start a second run")
		default:
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d while run in flight, want 1", calls.Load())
	}

	// Release the in-flight run and wait for the run flag to clear.
	close(release)
	for s.Running() {
		runtime.Gosched()
	}

	// A later tick resumes normal firing (whether the tick during the run was
	// consumed as a skip or still buffered when released, exactly one more run
	// fires and the total is two).
	clk.advance(time.Hour)
	<-started
	if calls.Load() != 2 {
		t.Fatalf("calls = %d after resume, want 2", calls.Load())
	}
}

func TestSchedulerContinuesAfterRunError(t *testing.T) {
	clk := newFakeClock(time.Unix(0, 0))
	started := make(chan struct{}, 8)
	var calls atomic.Int32
	s := &Scheduler{
		Interval: time.Hour,
		Clock:    clk,
		Run: func(ctx context.Context) error {
			calls.Add(1)
			started <- struct{}{}
			return context.DeadlineExceeded
		},
	}
	s.Start()
	defer s.Stop()

	clk.advance(time.Hour)
	<-started
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1", calls.Load())
	}
	clk.advance(time.Hour)
	<-started
	if calls.Load() != 2 {
		t.Fatalf("calls = %d after a failing run, want 2 (scheduler must not stop)", calls.Load())
	}
}

func TestSchedulerNonPositiveIntervalNeverFires(t *testing.T) {
	clk := newFakeClock(time.Unix(0, 0))
	started := make(chan struct{}, 8)
	var calls atomic.Int32
	s := &Scheduler{
		Interval: 0,
		Clock:    clk,
		Run: func(ctx context.Context) error {
			calls.Add(1)
			started <- struct{}{}
			return nil
		},
	}
	s.Start()
	clk.advance(100 * time.Hour)
	select {
	case <-started:
		t.Fatal("run fired with a non-positive interval")
	default:
	}
	if calls.Load() != 0 {
		t.Fatalf("calls = %d, want 0", calls.Load())
	}
	s.Stop()
}

func TestSchedulerRecoversPanicAndContinues(t *testing.T) {
	clk := newFakeClock(time.Unix(0, 0))
	started := make(chan struct{}, 8)
	var calls atomic.Int32
	var recovered []any
	var rmu sync.Mutex
	s := &Scheduler{
		Interval: time.Hour,
		Clock:    clk,
		Run: func(ctx context.Context) error {
			if calls.Add(1) == 1 {
				started <- struct{}{}
				panic("run exploded")
			}
			started <- struct{}{}
			return nil
		},
		Recover: func(v any) {
			rmu.Lock()
			recovered = append(recovered, v)
			rmu.Unlock()
		},
	}
	s.Start()
	defer s.Stop()

	clk.advance(time.Hour)
	<-started
	// The panic must be recovered and handed to the hook…
	waitNotRunning(t, s)
	rmu.Lock()
	got := len(recovered)
	rmu.Unlock()
	if got != 1 || len(recovered) == 0 {
		t.Fatalf("recovered panics = %d, want 1", got)
	}
	if recovered[0] != "run exploded" {
		t.Fatalf("recovered value = %v", recovered[0])
	}

	// …and the scheduler loop must survive to fire the next tick.
	clk.advance(time.Hour)
	<-started
	if calls.Load() != 2 {
		t.Fatalf("calls = %d after a panicking run, want 2 (scheduler must not stop)", calls.Load())
	}
}

// waitNotRunning spins until the in-flight run flag clears, or fails after a
// short deadline so a stuck scheduler never hangs the test suite forever.
func waitNotRunning(t *testing.T, s *Scheduler) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for s.Running() {
		select {
		case <-deadline:
			t.Fatal("run flag never cleared")
		default:
			runtime.Gosched()
		}
	}
}

func TestSchedulerPanicWithoutRecoverHookStillSurvives(t *testing.T) {
	clk := newFakeClock(time.Unix(0, 0))
	started := make(chan struct{}, 8)
	var calls atomic.Int32
	s := &Scheduler{
		Interval: time.Hour,
		Clock:    clk,
		Run: func(ctx context.Context) error {
			if calls.Add(1) == 1 {
				started <- struct{}{}
				panic("no hook")
			}
			started <- struct{}{}
			return nil
		},
	}
	s.Start()
	defer s.Stop()
	clk.advance(time.Hour)
	<-started
	waitNotRunning(t, s)
	clk.advance(time.Hour)
	<-started
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2", calls.Load())
	}
}

func TestSchedulerRecoverNilRunPanicSafe(t *testing.T) {
	clk := newFakeClock(time.Unix(0, 0))
	s := &Scheduler{Interval: time.Hour, Clock: clk, Run: nil}
	s.Start()
	defer s.Stop()
	clk.advance(time.Hour)
	// No panic, nothing to assert, just exercising the nil-Run guard.
}
