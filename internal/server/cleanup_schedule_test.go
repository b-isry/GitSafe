package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/b-isry/gitsafe/internal/config"
	"github.com/b-isry/gitsafe/internal/retention"
	"github.com/b-isry/gitsafe/internal/schedule"
	"github.com/b-isry/gitsafe/internal/state"
)

// schedFakeClock drives the server's scheduler deterministically. Advancing it
// pumps every live (non-stopped) ticker.
type schedFakeClock struct {
	mu      sync.Mutex
	now     time.Time
	tickers []*schedFakeTicker
}

func newSchedFakeClock(t0 time.Time) *schedFakeClock {
	return &schedFakeClock{now: t0}
}

func (f *schedFakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *schedFakeClock) NewTicker(d time.Duration) schedule.Ticker {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := &schedFakeTicker{
		clock:    f,
		interval: d,
		ch:       make(chan time.Time, 1),
		next:     f.now.Add(d),
	}
	f.tickers = append(f.tickers, t)
	return t
}

func (f *schedFakeClock) advance(d time.Duration) {
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

type schedFakeTicker struct {
	clock    *schedFakeClock
	interval time.Duration
	ch       chan time.Time
	next     time.Time
	stopped  bool
}

func (t *schedFakeTicker) C() <-chan time.Time { return t.ch }

func (t *schedFakeTicker) Stop() {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	t.stopped = true
}

// waitForStatus polls deterministically until cond holds.
func waitForStatus(t *testing.T, s *Server, cond func(st cleanupStatus) bool) cleanupStatus {
	t.Helper()
	for i := 0; i < 1_000_000; i++ {
		st := s.snapshotStatus()
		if cond(st) {
			return st
		}
		runtime.Gosched()
	}
	t.Fatal("cleanup status condition not reached")
	return cleanupStatus{}
}

func putRetention(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	cookie, csrf := csrfCookie(t, s)
	req := httptest.NewRequest(http.MethodPut, "/api/retention", bytes.NewReader([]byte(body)))
	req.RemoteAddr = "127.0.0.1:55555"
	req.AddCookie(cookie)
	req.Header.Set("X-CSRF-Token", csrf)
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	return rec
}

// recordingNotifier captures cleanup notifications.
type recordingNotifier struct {
	mu    sync.Mutex
	calls []string
}

func (n *recordingNotifier) NotifyCleanupResult(ctx context.Context, trigger string, res retention.Result) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls = append(n.calls, trigger)
	return nil
}

func (n *recordingNotifier) count() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.calls)
}

// failingNotifier always fails, to prove notifications never fail cleanup.
type failingNotifier struct{}

func (failingNotifier) NotifyCleanupResult(ctx context.Context, trigger string, res retention.Result) error {
	return errors.New("notification boom")
}

func TestRetentionGetSchedulingDefaults(t *testing.T) {
	s := newCloudServer(t, &fakeStateStore{}, newFakeTokenStore(), nil)
	rec := request(t, s, http.MethodGet, "/api/retention", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body struct {
		Retention retentionView `json:"retention"`
		Scheduled scheduledView `json:"scheduled"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Retention.ScheduledCleanupEnabled {
		t.Error("scheduled cleanup must default to disabled")
	}
	if body.Scheduled.Enabled || body.Scheduled.IntervalDays != 0 || body.Scheduled.Running {
		t.Errorf("unexpected default scheduled view: %+v", body.Scheduled)
	}
}

func TestRetentionPutEnablesAndDisablesScheduling(t *testing.T) {
	s := newCloudServer(t, &fakeStateStore{}, newFakeTokenStore(), nil)
	f := newSchedFakeClock(time.Unix(1700000000, 0))
	s.schedClock = f

	rec := putRetention(t, s, `{"scheduledCleanupEnabled":true,"scheduledCleanupIntervalDays":7}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("enable status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	loaded := s.app.ConfigSnapshot()
	if !loaded.Retention.ScheduledCleanupEnabled || loaded.Retention.ScheduledCleanupIntervalDays != 7 {
		t.Fatalf("scheduling not persisted: %+v", loaded.Retention)
	}
	s.schedMu.Lock()
	enabledSched := s.sched != nil
	s.schedMu.Unlock()
	if !enabledSched {
		t.Fatal("expected a running scheduler after enabling")
	}

	rec = putRetention(t, s, `{"scheduledCleanupEnabled":false,"scheduledCleanupIntervalDays":0}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	s.schedMu.Lock()
	disabledSched := s.sched == nil
	s.schedMu.Unlock()
	if !disabledSched {
		t.Fatal("expected scheduler stopped after disabling")
	}
}

func TestRetentionPutSchedulingValidation(t *testing.T) {
	s := newCloudServer(t, &fakeStateStore{}, newFakeTokenStore(), nil)
	rec := putRetention(t, s, `{"scheduledCleanupEnabled":true}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("enabled without interval status = %d, want 422", rec.Code)
	}
	rec = putRetention(t, s, `{"scheduledCleanupEnabled":true,"scheduledCleanupIntervalDays":-3}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("negative interval status = %d, want 422", rec.Code)
	}
}

func TestScheduledCleanupRunsAndReportsStatus(t *testing.T) {
	s := newCloudServer(t, &fakeStateStore{}, newFakeTokenStore(), nil)
	f := newSchedFakeClock(time.Unix(1700000000, 0))
	s.schedClock = f

	bundle := v2Bundle("repo", 0)
	writeBundle(t, s, bundle)
	seedRecord(t, s, retentionRecord("rec1", "p1", "org/repo", bundle, 10, "", state.BackupStatusBundled))

	rec := putRetention(t, s, `{"keepLocalDays":1,"scheduledCleanupEnabled":true,"scheduledCleanupIntervalDays":1}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	f.advance(24 * time.Hour)

	st := waitForStatus(t, s, func(st cleanupStatus) bool {
		return !st.Running && st.Success != nil
	})
	if st.LastTrigger != "scheduled" {
		t.Errorf("LastTrigger = %q, want scheduled", st.LastTrigger)
	}
	if st.Success == nil || !*st.Success {
		t.Errorf("Success = %v, want true", st.Success)
	}
	if _, ok := getRecord(s, "rec1"); ok {
		t.Error("record should have been removed by the scheduled cleanup")
	}

	sched := s.scheduledView()
	if !sched.Enabled || sched.IntervalDays != 1 || sched.Running {
		t.Errorf("unexpected scheduled view after run: %+v", sched)
	}
	if sched.LastTrigger != "scheduled" || sched.NextRun == "" {
		t.Errorf("expected scheduled trigger + next run: %+v", sched)
	}
}

func TestScheduledCleanupNotifiesAndToleratesNotifierFailure(t *testing.T) {
	s := newCloudServer(t, &fakeStateStore{}, newFakeTokenStore(), nil)
	f := newSchedFakeClock(time.Unix(1700000000, 0))
	s.schedClock = f
	rec := &recordingNotifier{}
	s.cleanupNotifier = rec

	bundle := v2Bundle("repo", 0)
	writeBundle(t, s, bundle)
	seedRecord(t, s, retentionRecord("rec1", "p1", "org/repo", bundle, 10, "", state.BackupStatusBundled))

	putRetention(t, s, `{"keepLocalDays":1,"scheduledCleanupEnabled":true,"scheduledCleanupIntervalDays":1}`)
	f.advance(24 * time.Hour)
	waitForStatus(t, s, func(st cleanupStatus) bool { return !st.Running && st.Success != nil })

	if _, ok := getRecord(s, "rec1"); ok {
		t.Error("cleanup must still apply despite notifications")
	}
	if rec.count() != 1 {
		t.Errorf("expected 1 notification, got %d", rec.count())
	}

	// A failing notifier must not fail the cleanup either.
	s.cleanupNotifier = failingNotifier{}
	rec2 := putRetention(t, s, `{"keepLocalDays":1,"scheduledCleanupEnabled":true,"scheduledCleanupIntervalDays":1}`)
	if rec2.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec2.Code)
	}
	f.advance(24 * time.Hour)
	st := waitForStatus(t, s, func(st cleanupStatus) bool {
		return !st.Running && st.Success != nil
	})
	if st.Success == nil || !*st.Success {
		t.Errorf("cleanup should succeed even when the notifier fails: %+v", st)
	}
}

func TestManualCleanupRecordsManualTrigger(t *testing.T) {
	s := newCloudServer(t, &fakeStateStore{}, newFakeTokenStore(), nil)
	f := newSchedFakeClock(time.Unix(1700000000, 0))
	s.schedClock = f

	bundle := v2Bundle("repo", 0)
	writeBundle(t, s, bundle)
	seedRecord(t, s, retentionRecord("rec1", "p1", "org/repo", bundle, 10, "", state.BackupStatusBundled))
	s.app.Config.Retention = config.RetentionConfig{KeepLocalDays: 1}

	cookie, csrf := csrfCookie(t, s)
	req := httptest.NewRequest(http.MethodPost, "/api/retention/cleanup", nil)
	req.RemoteAddr = "127.0.0.1:55555"
	req.AddCookie(cookie)
	req.Header.Set("X-CSRF-Token", csrf)
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	st := waitForStatus(t, s, func(st cleanupStatus) bool { return !st.Running && st.Success != nil })
	if st.LastTrigger != "manual" {
		t.Errorf("LastTrigger = %q, want manual", st.LastTrigger)
	}
	if _, ok := getRecord(s, "rec1"); ok {
		t.Error("manual cleanup should have removed the eligible record")
	}
}

func TestSchedulerStopsWhenDisabledAndDoesNotResume(t *testing.T) {
	s := newCloudServer(t, &fakeStateStore{}, newFakeTokenStore(), nil)
	f := newSchedFakeClock(time.Unix(1700000000, 0))
	s.schedClock = f

	bundle := v2Bundle("repo", 0)
	writeBundle(t, s, bundle)
	seedRecord(t, s, retentionRecord("rec1", "p1", "org/repo", bundle, 10, "", state.BackupStatusBundled))

	putRetention(t, s, `{"keepLocalDays":1,"scheduledCleanupEnabled":true,"scheduledCleanupIntervalDays":1}`)
	f.advance(24 * time.Hour)
	waitForStatus(t, s, func(st cleanupStatus) bool { return !st.Running && st.Success != nil })

	putRetention(t, s, `{"keepLocalDays":1,"scheduledCleanupEnabled":false,"scheduledCleanupIntervalDays":0}`)
	s.schedMu.Lock()
	stopped := s.sched == nil
	s.schedMu.Unlock()
	if !stopped {
		t.Fatal("scheduler must be nil after disabling")
	}

	before := s.snapshotStatus().LastFinish
	f.advance(200 * 24 * time.Hour)
	for i := 0; i < 100_000; i++ {
		runtime.Gosched()
	}
	if after := s.snapshotStatus().LastFinish; !after.Equal(before) {
		t.Errorf("cleanup ran after disabling: before=%v after=%v", before, after)
	}
	if _, ok := getRecord(s, "rec1"); ok {
		t.Error("record should have been removed by the earlier scheduled run")
	}
}

func TestExecuteCleanupSerializesScheduledAndManual(t *testing.T) {
	s := newCloudServer(t, &fakeStateStore{}, newFakeTokenStore(), nil)
	f := newSchedFakeClock(time.Unix(1700000000, 0))
	s.schedClock = f
	n := &recordingNotifier{}
	s.cleanupNotifier = n

	deleteStarted := make(chan struct{})
	releaseDrive := make(chan struct{})
	driveCalls := 0
	s.driveDelete = func(ctx context.Context, fileID string, cloudCfg config.CloudConfig, logger *slog.Logger) error {
		driveCalls++
		close(deleteStarted)
		<-releaseDrive
		return nil
	}
	bundle := v2Bundle("repo", 0)
	writeBundle(t, s, bundle)
	seedRecord(t, s, retentionRecord("up1", "p1", "org/repo", bundle, 100, "drive-xyz-abcdefghijklmnop", state.BackupStatusUploaded))
	s.app.Config.Retention = config.RetentionConfig{DriveRetention: true, KeepDriveDays: 30}

	scheduledDone := make(chan struct{})
	go func() {
		s.executeCleanup(context.Background(), "scheduled")
		close(scheduledDone)
	}()
	<-deleteStarted

	manualDone := make(chan struct{})
	go func() {
		s.executeCleanup(context.Background(), "manual")
		close(manualDone)
	}()
	select {
	case <-manualDone:
		t.Fatal("manual cleanup must wait behind the scheduled run (cleanupMu serialization)")
	case <-time.After(100 * time.Millisecond):
	}

	close(releaseDrive)
	<-scheduledDone
	<-manualDone

	if driveCalls != 1 {
		t.Errorf("driveDelete calls = %d, want 1 (serialization must prevent double-delete)", driveCalls)
	}
	rec, _ := getRecord(s, "up1")
	if rec.Status != state.BackupStatusBundled || rec.DriveFileID != "" {
		t.Errorf("record should be downgraded once: %+v", rec)
	}
	if n.count() != 2 {
		t.Errorf("expected 2 notifications, got %d", n.count())
	}
}

func TestStopCleanupSchedulerWaitsForInFlightRun(t *testing.T) {
	s := newCloudServer(t, &fakeStateStore{}, newFakeTokenStore(), nil)
	f := newSchedFakeClock(time.Unix(1700000000, 0))
	s.schedClock = f

	deleteStarted := make(chan struct{})
	releaseDrive := make(chan struct{})
	s.driveDelete = func(ctx context.Context, fileID string, cloudCfg config.CloudConfig, logger *slog.Logger) error {
		close(deleteStarted)
		<-releaseDrive
		return nil
	}
	bundle := v2Bundle("repo", 0)
	writeBundle(t, s, bundle)
	seedRecord(t, s, retentionRecord("up1", "p1", "org/repo", bundle, 100, "drive-xyz-abcdefghijklmnop", state.BackupStatusUploaded))

	putRetention(t, s, `{"driveRetentionEnabled":true,"keepDriveDays":30,"scheduledCleanupEnabled":true,"scheduledCleanupIntervalDays":1}`)
	f.advance(24 * time.Hour)
	<-deleteStarted

	stopped := make(chan struct{})
	go func() {
		s.StopCleanupScheduler()
		close(stopped)
	}()
	select {
	case <-stopped:
		t.Fatal("StopCleanupScheduler returned while a cleanup run was in flight")
	default:
	}

	close(releaseDrive)
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("StopCleanupScheduler did not return after the run finished")
	}
	s.schedMu.Lock()
	defer s.schedMu.Unlock()
	if s.sched != nil {
		t.Fatal("scheduler must be nil after StopCleanupScheduler")
	}
}

func TestStartCleanupSchedulerUsesCurrentConfig(t *testing.T) {
	s := newCloudServer(t, &fakeStateStore{}, newFakeTokenStore(), nil)
	f := newSchedFakeClock(time.Unix(1700000000, 0))
	s.schedClock = f

	bundle := v2Bundle("repo", 0)
	writeBundle(t, s, bundle)
	seedRecord(t, s, retentionRecord("rec1", "p1", "org/repo", bundle, 10, "", state.BackupStatusBundled))

	cfg := s.app.ConfigSnapshot()
	cfg.Retention.KeepLocalDays = 1
	cfg.Retention.ScheduledCleanupEnabled = true
	cfg.Retention.ScheduledCleanupIntervalDays = 2
	s.app.ApplySettings(cfg)

	s.StartCleanupScheduler()
	f.advance(48 * time.Hour)
	waitForStatus(t, s, func(st cleanupStatus) bool { return !st.Running && st.Success != nil })

	if st := s.snapshotStatus(); st.LastTrigger != "scheduled" {
		t.Errorf("LastTrigger = %q, want scheduled", st.LastTrigger)
	}
	if _, ok := getRecord(s, "rec1"); ok {
		t.Error("scheduled cleanup should have removed the eligible record")
	}
}
