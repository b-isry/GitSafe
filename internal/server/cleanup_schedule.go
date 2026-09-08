package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/b-isry/gitsafe/internal/config"
	"github.com/b-isry/gitsafe/internal/history"
	"github.com/b-isry/gitsafe/internal/notify"
	"github.com/b-isry/gitsafe/internal/retention"
	"github.com/b-isry/gitsafe/internal/schedule"
)

// cleanupStatus is the in-memory observability state for cleanup runs, whether
// triggered manually from the UI or by the scheduler. A dedicated statusMu
// guards it so a running cleanup never blocks the status read in GET
// /api/retention.
type cleanupStatus struct {
	Running     bool
	LastStart   time.Time
	LastFinish  time.Time
	LastTrigger string
	Success     *bool
	Result      retention.Result
}

// scheduledView is the JSON shape of the scheduling + run-status block exposed
// by GET /api/retention.
type scheduledView struct {
	Enabled      bool             `json:"enabled"`
	IntervalDays int              `json:"intervalDays"`
	Running      bool             `json:"running"`
	LastStart    string           `json:"lastStart,omitempty"`
	LastFinish   string           `json:"lastFinish,omitempty"`
	LastTrigger  string           `json:"lastTrigger,omitempty"`
	Success      *bool            `json:"success,omitempty"`
	NextRun      string           `json:"nextRun,omitempty"`
	Result       retention.Result `json:"lastResult"`
}

// CleanupNotifier receives notifications about completed cleanup runs. A
// notification failure is tolerated: cleanup itself must never fail because a
// notification did. The returned error is recorded in the cleanup history so an
// operator can see that the target could not be reached.
type CleanupNotifier interface {
	NotifyCleanupResult(ctx context.Context, trigger string, res retention.Result) error
}

// notifyCleanupNotifier is the production CleanupNotifier: it logs each run and,
// whenever webhook notifications are configured, delivers the run summary to
// the webhook. Delivery failures are returned (surfaced in history) but never
// fail the cleanup.
type notifyCleanupNotifier struct {
	logger  *slog.Logger
	webhook *notify.Notifier
	// status returns a snapshot of the current run status, used for run timing.
	status func() cleanupStatus
}

func (n notifyCleanupNotifier) NotifyCleanupResult(ctx context.Context, trigger string, res retention.Result) error {
	n.logger.Info("cleanup completed",
		"trigger", trigger,
		"inspected", res.Inspected,
		"retained", res.Retained,
		"localDeleted", len(res.LocalDeleted),
		"driveDeleted", len(res.DriveDeleted),
		"orphanDeleted", res.OrphanDeleted,
		"skipped", res.Skipped,
		"missing", res.Missing,
		"errorCount", len(res.Errors),
	)

	if n.webhook == nil {
		return nil
	}
	st := n.status()
	ev := notify.Event{
		Trigger:       trigger,
		StartedAt:     st.LastStart,
		FinishedAt:    st.LastFinish,
		Success:       len(res.Errors) == 0,
		Inspected:     res.Inspected,
		Retained:      res.Retained,
		LocalDeleted:  len(res.LocalDeleted),
		DriveDeleted:  len(res.DriveDeleted),
		OrphanDeleted: res.OrphanDeleted,
		Skipped:       res.Skipped,
		Missing:       res.Missing,
		Errors:        res.Errors,
	}
	if err := n.webhook.Deliver(ctx, ev); err != nil && !errors.Is(err, notify.ErrDisabled) {
		return err
	}
	return nil
}

// recordCleanupPanic turns a panicking cleanup run into an observable failed
// run: it logs loudly, clears the run status, and persists a failed history
// entry. It is invoked from executeCleanup's own recovery (both manual and
// scheduled runs) and from the scheduler's Recover hook as a belt-and-braces
// guard for panics that escape executeCleanup entirely.
func (s *Server) recordCleanupPanic(trigger string, start time.Time, v any) {
	msg := fmt.Sprintf("cleanup panicked: %v", v)
	s.logger.Error("cleanup run panicked", "trigger", trigger, "panic", msg)
	s.setCleanupStatus(func(st *cleanupStatus) {
		st.Running = false
		st.LastFinish = s.schedNow()
		st.Success = nil
	})
	s.recordHistory(trigger, start, s.schedNow(), false, retention.Result{Errors: []string{msg}}, nil)
}

// recordHistory appends a completed run to the persisted history. Recording
// must never fail anything: failures are logged and dropped.
func (s *Server) recordHistory(trigger string, start, finish time.Time, success bool, res retention.Result, notifyErr error) {
	if s.historyStore == nil {
		return
	}
	entry := history.Entry{
		StartedAt:     start,
		FinishedAt:    finish,
		Trigger:       trigger,
		Success:       success,
		Inspected:     res.Inspected,
		Retained:      res.Retained,
		LocalDeleted:  len(res.LocalDeleted),
		DriveDeleted:  len(res.DriveDeleted),
		OrphanDeleted: res.OrphanDeleted,
		Skipped:       res.Skipped,
		Missing:       res.Missing,
		ErrorCount:    len(res.Errors),
		Errors:        res.Errors,
	}
	if notifyErr != nil && !errors.Is(notifyErr, notify.ErrDisabled) {
		entry.NotifyError = notifyErr.Error()
	}
	if err := s.historyStore.Add(entry); err != nil {
		s.logger.Warn("record cleanup history", "error", err)
	}
}

// executeCleanup runs the retention engine under the current policy, records
// run status, persists history, and notifies. It is the single entry point for
// both manual and scheduled runs; the underlying runCleanup is serialized by
// cleanupMu so the two can never run destructively at the same time. A panicking
// engine never takes the scheduler (or the process) down: the panic is
// recovered, recorded as a failed run, and subsequent runs continue.
func (s *Server) executeCleanup(ctx context.Context, trigger string) retention.Result {
	start := s.schedNow()
	s.setCleanupStatus(func(st *cleanupStatus) {
		st.Running = true
		st.LastStart = start
		st.LastTrigger = trigger
		st.Success = nil
	})

	panicked := false
	res := func() (r retention.Result) {
		defer func() {
			if v := recover(); v != nil {
				panicked = true
				s.recordCleanupPanic(trigger, start, v)
			}
		}()
		return s.runCleanup(ctx)
	}()
	if panicked {
		return res
	}

	finish := s.schedNow()
	success := len(res.Errors) == 0
	s.setCleanupStatus(func(st *cleanupStatus) {
		st.Running = false
		st.LastFinish = finish
		st.Success = &success
		st.Result = res
	})

	var notifyErr error
	if s.cleanupNotifier != nil {
		notifyErr = s.cleanupNotifier.NotifyCleanupResult(ctx, trigger, res)
		if notifyErr != nil {
			s.logger.Warn("cleanup notification failed", "trigger", trigger, "error", notifyErr)
		}
	}
	s.recordHistory(trigger, start, finish, success, res, notifyErr)
	return res
}

// runScheduledCleanup is the callback the scheduler fires on each interval. It
// reports no error to the scheduler: the run outcome is observable through the
// cleanup status, and a notification failure must not look like a cleanup failure.
func (s *Server) runScheduledCleanup(ctx context.Context) error {
	s.executeCleanup(ctx, "scheduled")
	return nil
}

// startSchedulerFor replaces any running scheduler with one reflecting cfg's
// scheduling settings, or stops scheduling entirely when it is disabled. It is
// safe to call repeatedly (startup and every settings/retention save).
func (s *Server) startSchedulerFor(cfg config.Config) {
	enabled := cfg.Retention.ScheduledCleanupEnabled
	intervalDays := cfg.Retention.ScheduledCleanupIntervalDays

	s.schedMu.Lock()
	defer s.schedMu.Unlock()

	if s.sched != nil {
		s.sched.Stop()
		s.sched = nil
	}

	if !enabled || intervalDays <= 0 {
		return
	}

	sched := &schedule.Scheduler{
		Interval: time.Duration(intervalDays) * 24 * time.Hour,
		Clock:    s.schedClock, // nil → the scheduler falls back to the wall clock
		Run:      s.runScheduledCleanup,
		// Belt-and-braces: executeCleanup already recovers its own panics, but a
		// panic escaping it (or anywhere in the scheduling glue) must still be
		// surfaced as a failed run instead of killing or starving the loop.
		Recover: func(v any) {
			s.recordCleanupPanic("scheduled", s.schedNow(), v)
		},
	}
	sched.Start()
	s.sched = sched
}

// StartCleanupScheduler starts scheduling per the current configuration. It is
// called at startup after cloud wiring; settings saves restart the scheduler
// through handleRetentionPut.
func (s *Server) StartCleanupScheduler() {
	s.startSchedulerFor(s.app.ConfigSnapshot())
}

// StopCleanupScheduler stops any running scheduler, waiting for an in-flight
// cleanup run to finish. Idempotent; used by graceful shutdown and tests.
func (s *Server) StopCleanupScheduler() {
	s.schedMu.Lock()
	defer s.schedMu.Unlock()
	if s.sched == nil {
		return
	}
	s.sched.Stop()
	s.sched = nil
}

// scheduledView reports current scheduling configuration and run status.
func (s *Server) scheduledView() scheduledView {
	cfg := s.app.ConfigSnapshot()
	st := s.snapshotStatus()

	v := scheduledView{
		Enabled:      cfg.Retention.ScheduledCleanupEnabled,
		IntervalDays: cfg.Retention.ScheduledCleanupIntervalDays,
		Running:      st.Running,
		LastTrigger:  st.LastTrigger,
		Success:      st.Success,
		Result:       st.Result,
	}
	if !st.LastStart.IsZero() {
		v.LastStart = formatTime(st.LastStart)
	}
	if !st.LastFinish.IsZero() {
		v.LastFinish = formatTime(st.LastFinish)
	}

	// The approximate next run: the next interval boundary after the most recent
	// run when idle. Deterministic with the injected clock.
	if v.Enabled && v.IntervalDays > 0 && !st.Running {
		base := st.LastFinish
		if base.IsZero() {
			base = st.LastStart
		}
		if base.IsZero() {
			base = s.schedNow()
		}
		v.NextRun = formatTime(base.Add(time.Duration(v.IntervalDays) * 24 * time.Hour))
	}
	return v
}

func (s *Server) setCleanupStatus(f func(st *cleanupStatus)) {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	f(&s.cleanupStatus)
}

func (s *Server) snapshotStatus() cleanupStatus {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	return s.cleanupStatus
}

func (s *Server) schedNow() time.Time {
	if s.schedClock != nil {
		return s.schedClock.Now()
	}
	return time.Now()
}
