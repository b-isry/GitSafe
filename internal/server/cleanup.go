package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path"

	"github.com/b-isry/gitsafe/internal/config"
	"github.com/b-isry/gitsafe/internal/notify"
	"github.com/b-isry/gitsafe/internal/retention"
	"github.com/b-isry/gitsafe/internal/state"
)

// retentionView is the JSON shape of the retention policy exposed to the UI. It
// deliberately exposes no filesystem paths.
type retentionView struct {
	KeepLocal       int  `json:"keepLocal"`
	KeepLocalDays   int  `json:"keepLocalDays"`
	KeepJobs        int  `json:"keepJobs"`
	KeepDriveDays   int  `json:"keepDriveDays"`
	DriveRetention  bool `json:"driveRetentionEnabled"`
	DriveConfigured bool `json:"driveConfigured"`

	ScheduledCleanupEnabled      bool `json:"scheduledCleanupEnabled"`
	ScheduledCleanupIntervalDays int  `json:"scheduledCleanupIntervalDays"`
	CleanupHistoryLimit          int  `json:"cleanupHistoryLimit"`
}

func toRetentionView(cfg config.Config) retentionView {
	return retentionView{
		KeepLocal:       cfg.Retention.KeepLocal,
		KeepLocalDays:   cfg.Retention.KeepLocalDays,
		KeepJobs:        cfg.Retention.KeepJobs,
		KeepDriveDays:   cfg.Retention.KeepDriveDays,
		DriveRetention:  cfg.Retention.DriveRetention,
		DriveConfigured: driveEnabledFor(cfg.Cloud),

		ScheduledCleanupEnabled:      cfg.Retention.ScheduledCleanupEnabled,
		ScheduledCleanupIntervalDays: cfg.Retention.ScheduledCleanupIntervalDays,
		CleanupHistoryLimit:          cfg.Retention.CleanupHistoryLimit,
	}
}

// notificationsView is the read-only JSON shape of the notification settings.
// The auth token is write-only: it is never exposed once configured.
type notificationsView struct {
	Enabled               bool   `json:"enabled"`
	Type                  string `json:"type"`
	WebhookURL            string `json:"webhookUrl,omitempty"`
	WebhookAuthConfigured bool   `json:"webhookAuthConfigured"`
	WebhookTimeoutSeconds int    `json:"webhookTimeoutSeconds"`
	NotifyOnSuccess       bool   `json:"notifyOnSuccess"`
	NotifyOnFailure       bool   `json:"notifyOnFailure"`
}

func toNotificationsView(n config.NotificationConfig) notificationsView {
	return notificationsView{
		Enabled:               n.Enabled,
		Type:                  n.Type,
		WebhookURL:            n.WebhookURL,
		WebhookAuthConfigured: n.WebhookAuthToken != "",
		WebhookTimeoutSeconds: n.WebhookTimeoutSeconds,
		NotifyOnSuccess:       n.NotifyOnSuccess,
		NotifyOnFailure:       n.NotifyOnFailure,
	}
}

// overrideView is the JSON shape of a single per-repo retention override. Nil
// dimensions are omitted so the UI can treat them as "inherit the global".
type overrideView struct {
	RepositoryID   int64  `json:"repositoryId"`
	Name           string `json:"name,omitempty"`
	KeepLocal      *int   `json:"keepLocal,omitempty"`
	KeepLocalDays  *int   `json:"keepLocalDays,omitempty"`
	KeepJobs       *int   `json:"keepJobs,omitempty"`
	KeepDriveDays  *int   `json:"keepDriveDays,omitempty"`
	DriveRetention *bool  `json:"driveRetentionEnabled,omitempty"`
}

func (s *Server) overridesView() []overrideView {
	cfg := s.app.ConfigSnapshot()
	idByName := map[int64]string{}
	if s.stateStore != nil {
		for _, r := range s.stateStore.ProtectedRepos() {
			idByName[r.GitHubID] = r.FullName
		}
	}
	out := make([]overrideView, 0, len(cfg.RepositoryOverrides))
	for _, ov := range cfg.RepositoryOverrides {
		v := overrideView{
			RepositoryID:   ov.RepositoryID,
			KeepLocal:      ov.KeepLocal,
			KeepLocalDays:  ov.KeepLocalDays,
			KeepJobs:       ov.KeepJobs,
			KeepDriveDays:  ov.KeepDriveDays,
			DriveRetention: ov.DriveRetention,
		}
		if n := idByName[ov.RepositoryID]; n != "" {
			v.Name = n
		}
		out = append(out, v)
	}
	return out
}

// handleRetentionGet reports the current retention policy, the most recent
// cleanup result, scheduling status, notification settings, per-repo overrides,
// and a bounded cleanup history. Read-only; loopback only (no session/CSRF
// required). History is capped server-side so the response stays small.
func (s *Server) handleRetentionGet(w http.ResponseWriter, r *http.Request) {
	cfg := s.app.ConfigSnapshot()
	view := toRetentionView(cfg)
	s.cleanupMu.Lock()
	last := s.lastCleanup
	s.cleanupMu.Unlock()

	var hist []any
	if s.historyStore != nil {
		for _, e := range s.historyStore.Recent(historyBlockSize) {
			hist = append(hist, e)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"retention":     view,
		"lastCleanup":   last,
		"scheduled":     s.scheduledView(),
		"notifications": toNotificationsView(cfg.Notifications),
		"overrides":     s.overridesView(),
		"history":       hist,
	})
}

// historyBlockSize caps the cleanup-history entries returned by GET
// /api/retention. The full bounded history stays on disk; the API is a preview.
const historyBlockSize = 10

// maxRetentionOverrides caps the per-repository override list a single PUT
// /api/retention can carry, protecting against a pathologically large batch.
const maxRetentionOverrides = 1000

// retentionInput is the payload accepted by PUT /api/retention. Pointer fields
// preserve existing values when omitted, so clients that predate a field (or
// simply do not send it) never silently reset it.
type retentionInput struct {
	KeepLocal      int  `json:"keepLocal"`
	KeepLocalDays  int  `json:"keepLocalDays"`
	KeepJobs       int  `json:"keepJobs"`
	KeepDriveDays  int  `json:"keepDriveDays"`
	DriveRetention bool `json:"driveRetentionEnabled"`

	ScheduledCleanupEnabled      bool `json:"scheduledCleanupEnabled"`
	ScheduledCleanupIntervalDays int  `json:"scheduledCleanupIntervalDays"`

	CleanupHistoryLimit *int                `json:"cleanupHistoryLimit,omitempty"`
	Notifications       *notificationsInput `json:"notifications,omitempty"`
	Overrides           []overrideInput     `json:"overrides,omitempty"`
}

// notificationsInput is the write path for notification settings. The auth
// token is write-only: an absent (nil) token preserves what is stored, a
// present non-empty string replaces it, and a present empty string clears it.
type notificationsInput struct {
	Enabled               *bool   `json:"enabled,omitempty"`
	Type                  string  `json:"type,omitempty"`
	WebhookURL            *string `json:"webhookUrl,omitempty"`
	WebhookAuthHeader     *string `json:"webhookAuthHeader,omitempty"`
	WebhookAuthToken      *string `json:"webhookAuthToken,omitempty"`
	WebhookTimeoutSeconds *int    `json:"webhookTimeoutSeconds,omitempty"`
	NotifyOnSuccess       *bool   `json:"notifyOnSuccess,omitempty"`
	NotifyOnFailure       *bool   `json:"notifyOnFailure,omitempty"`
}

// overrideInput is one per-repo retention override from the UI.
type overrideInput struct {
	RepositoryID   int64 `json:"repositoryId"`
	KeepLocal      *int  `json:"keepLocal,omitempty"`
	KeepLocalDays  *int  `json:"keepLocalDays,omitempty"`
	KeepJobs       *int  `json:"keepJobs,omitempty"`
	KeepDriveDays  *int  `json:"keepDriveDays,omitempty"`
	DriveRetention *bool `json:"driveRetentionEnabled,omitempty"`
}

// validate rejects malformed overrides: an identifier must be present and
// non-negative retention knobs must never be negative (a negative value would
// be read as "delete everything").
func (o overrideInput) validate() error {
	if o.RepositoryID <= 0 {
		return errors.New("retention override requires a valid repository id")
	}
	for _, v := range []struct {
		name string
		val  *int
	}{
		{"keepLocal", o.KeepLocal},
		{"keepLocalDays", o.KeepLocalDays},
		{"keepJobs", o.KeepJobs},
		{"keepDriveDays", o.KeepDriveDays},
	} {
		if v.val != nil && *v.val < 0 {
			return fmt.Errorf("retention override %s must be >= 0 for repository %d", v.name, o.RepositoryID)
		}
	}
	return nil
}

// toOverrideConfig maps a validated UI override into the config representation.
// validation happens first via overrideInput.validate.
func toOverrideConfig(in overrideInput, name string) config.RepositoryRetentionOverride {
	return config.RepositoryRetentionOverride{
		RepositoryID:   in.RepositoryID,
		KeepLocal:      in.KeepLocal,
		KeepLocalDays:  in.KeepLocalDays,
		KeepJobs:       in.KeepJobs,
		KeepDriveDays:  in.KeepDriveDays,
		DriveRetention: in.DriveRetention,
	}
}

func applyNotificationsInput(current config.NotificationConfig, in notificationsInput) config.NotificationConfig {
	n := current
	if in.Enabled != nil {
		n.Enabled = *in.Enabled
	}
	if in.Type != "" {
		n.Type = in.Type
	}
	if in.WebhookURL != nil {
		n.WebhookURL = *in.WebhookURL
	}
	if in.WebhookAuthHeader != nil {
		n.WebhookAuthHeader = *in.WebhookAuthHeader
	}
	if in.WebhookAuthToken != nil {
		n.WebhookAuthToken = *in.WebhookAuthToken
	}
	if in.WebhookTimeoutSeconds != nil {
		n.WebhookTimeoutSeconds = *in.WebhookTimeoutSeconds
	}
	if in.NotifyOnSuccess != nil {
		n.NotifyOnSuccess = *in.NotifyOnSuccess
	}
	if in.NotifyOnFailure != nil {
		n.NotifyOnFailure = *in.NotifyOnFailure
	}
	return n
}

// handleRetentionPut updates the retention policy (including per-repo
// overrides, notification settings, and the history limit). Session + CSRF +
// validation apply, mirroring the settings save flow. When the scheduling
// settings change, the running scheduler is cleanly replaced to reflect the new
// configuration.
func (s *Server) handleRetentionPut(w http.ResponseWriter, r *http.Request) {
	sess := s.sessionFromRequest(r)
	if sess == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "No active session. Refresh the page and try again."})
		return
	}
	if !validateCSRF(sess, r.Header.Get("X-CSRF-Token")) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "CSRF validation failed."})
		return
	}

	var in retentionInput
	if err := decodeJSONBody(w, r, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid request body."})
		return
	}
	if len(in.Overrides) > maxRetentionOverrides {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "Too many retention overrides in one request."})
		return
	}

	next := s.app.ConfigSnapshot()
	policy := next.Retention
	policy.KeepLocal = in.KeepLocal
	policy.KeepLocalDays = in.KeepLocalDays
	policy.KeepJobs = in.KeepJobs
	policy.KeepDriveDays = in.KeepDriveDays
	policy.DriveRetention = in.DriveRetention
	policy.ScheduledCleanupEnabled = in.ScheduledCleanupEnabled
	policy.ScheduledCleanupIntervalDays = in.ScheduledCleanupIntervalDays
	if in.CleanupHistoryLimit != nil {
		policy.CleanupHistoryLimit = *in.CleanupHistoryLimit
	}
	if err := policy.Validate(); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
		return
	}
	next.Retention = policy

	if in.Notifications != nil {
		next.Notifications = applyNotificationsInput(next.Notifications, *in.Notifications)
	}
	if in.Overrides != nil {
		overrides := make([]config.RepositoryRetentionOverride, 0, len(in.Overrides))
		names := map[int64]string{}
		seen := map[int64]bool{}
		for _, ov := range in.Overrides {
			if err := ov.validate(); err != nil {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
				return
			}
			if seen[ov.RepositoryID] {
				continue
			}
			seen[ov.RepositoryID] = true
			overrides = append(overrides, toOverrideConfig(ov, names[ov.RepositoryID]))
		}
		next.RepositoryOverrides = overrides
	}

	// Mirror config.Load's normalization: derive sources from the root path so
	// validation holds regardless of whether the in-memory config was built by
	// Load (which normally synthesizes sources) or directly.
	if len(next.Sources) == 0 {
		next.Sources = next.EffectiveSources()
	}

	if err := next.Validate(); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
		return
	}

	if err := next.Save(s.configPath); err != nil {
		s.logger.Error("save retention config", "path", s.configPath, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Could not write the configuration file."})
		return
	}

	loaded, err := config.Load(s.configPath)
	if err != nil {
		s.logger.Error("reload config after retention save", "path", s.configPath, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "The configuration was saved but could not be re-read."})
		return
	}
	s.app.ApplySettings(loaded)
	s.startSchedulerFor(loaded)

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":            true,
		"retention":     toRetentionView(loaded),
		"notifications": toNotificationsView(loaded.Notifications),
		"overrides":     s.overridesView(),
	})
}

// handleRetentionCleanup triggers a manual cleanup run and returns the result.
// Session + CSRF apply.
func (s *Server) handleRetentionCleanup(w http.ResponseWriter, r *http.Request) {
	sess := s.sessionFromRequest(r)
	if sess == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "No active session. Refresh the page and try again."})
		return
	}
	if !validateCSRF(sess, r.Header.Get("X-CSRF-Token")) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "CSRF validation failed."})
		return
	}

	res := s.executeCleanup(context.Background(), "manual")
	writeJSON(w, http.StatusOK, map[string]any{"cleanup": res})
}

// runCleanup runs the retention engine under the current policy and applies the
// resulting state mutations atomically. It is serialized by cleanupMu.
func (s *Server) runCleanup(ctx context.Context) retention.Result {
	cfg := s.app.ConfigSnapshot()

	s.cleanupMu.Lock()
	defer s.cleanupMu.Unlock()

	// Without a cloud state store there is no backup history to retain; simply
	// record an empty run and return. Local orphan cleanup is intentionally not
	// performed in this state.
	if s.stateStore == nil {
		res := retention.Result{Retained: 0}
		s.lastCleanup = res
		return res
	}

	// Snapshot active jobs under jobMu, atomically with job creation, so a repo
	// that starts a backup mid-cleanup is never a deletion target.
	s.jobMu.Lock()
	jobs := s.stateStore.BackupJobs()
	activeRepo := map[string]bool{}
	activeName := map[string]bool{}
	for _, j := range jobs {
		if state.IsTerminalState(j.State) {
			continue
		}
		activeRepo[j.ProtectedRepoID] = true
		if j.FullName != "" {
			activeName[bareRepoName(j.FullName)] = true
		}
	}
	s.jobMu.Unlock()

	records := s.stateStore.BackupRecords()
	repos := s.stateStore.ProtectedRepos()

	// Per-repo overrides are keyed by the immutable GitHub repository ID, while
	// records are keyed by the local protected-repo UUID. Resolve the mapping
	// once and install an engine PolicyFor that merges the override onto the
	// global policy per repository. Repositories without an override get the
	// global policy unchanged.
	githubIDByProtectedID := map[string]int64{}
	for _, r := range repos {
		githubIDByProtectedID[r.ID] = r.GitHubID
	}
	overrideByGitHubID := map[int64]config.RepositoryRetentionOverride{}
	for _, ov := range cfg.RepositoryOverrides {
		overrideByGitHubID[ov.RepositoryID] = ov
	}

	cloudCfg := cfg.Cloud
	engine := &retention.Engine{
		FS: retention.DiskFS{},
		Drive: func(fileID string) error {
			return s.driveDelete(ctx, fileID, cloudCfg, s.logger)
		},
		IsActiveRepo: func(id string) bool { return activeRepo[id] },
		IsActiveName: func(name string) bool { return activeName[name] },
		PolicyFor: func(protectedRepoID string) config.RetentionConfig {
			ov, ok := overrideByGitHubID[githubIDByProtectedID[protectedRepoID]]
			if !ok {
				return cfg.Retention
			}
			return config.MergeRetention(cfg.Retention, ov)
		},
	}

	res := engine.Run(cfg.Retention, cfg.OutputPath, records, jobs)

	// Apply state mutations deterministically: remove records first (removal
	// takes precedence over a downgrade of the same record), then jobs.
	removing := map[string]bool{}
	for _, id := range res.RecordsToRemove {
		removing[id] = true
	}
	for _, id := range res.RecordsToRemove {
		if err := s.stateStore.RemoveBackupRecord(id); err != nil {
			s.logger.Warn("retention: remove record", "record", id, "error", err)
		}
	}
	for _, id := range res.RecordsToDowngrade {
		if removing[id] {
			continue
		}
		rec, ok := s.recordByID(records, id)
		if !ok {
			continue
		}
		rec.DriveFileID = ""
		rec.Status = state.BackupStatusBundled
		if err := s.stateStore.UpdateBackupRecord(rec); err != nil {
			s.logger.Warn("retention: downgrade record", "record", id, "error", err)
		}
	}
	for _, id := range res.JobsToRemove {
		if err := s.stateStore.RemoveBackupJob(id); err != nil {
			s.logger.Warn("retention: remove job", "job", id, "error", err)
		}
	}
	if len(res.RecordsToRemove) > 0 || len(res.RecordsToDowngrade) > 0 || len(res.JobsToRemove) > 0 {
		if err := s.saveState(); err != nil {
			s.logger.Warn("retention: persist mutations", "error", err)
			res.Errors = append(res.Errors, fmt.Sprintf("persist retention mutations: %v", err))
		}
	}

	s.lastCleanup = res
	return res
}

// recordByID finds a backup record in a snapshot by ID.
func (s *Server) recordByID(records []state.BackupRecord, id string) (state.BackupRecord, bool) {
	for _, rec := range records {
		if rec.ID == id {
			return rec, true
		}
	}
	return state.BackupRecord{}, false
}

// bareRepoName returns the last path segment of a GitHub full name (owner/name),
// matching the prefix GitSafe bakes into bundle filenames.
func bareRepoName(fullName string) string {
	return path.Base(fullName)
}

// handleRetentionNotifyTest delivers a synthetic "test" notification to the
// configured webhook so an operator can verify the target, auth header, and
// reachability before enabling real notifications. It never touches cleanup
// state or history. Session + CSRF apply.
func (s *Server) handleRetentionNotifyTest(w http.ResponseWriter, r *http.Request) {
	sess := s.sessionFromRequest(r)
	if sess == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "No active session. Refresh the page and try again."})
		return
	}
	if !validateCSRF(sess, r.Header.Get("X-CSRF-Token")) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "CSRF validation failed."})
		return
	}
	if s.notifyWebhook == nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "Notifications are not available."})
		return
	}
	err := s.notifyWebhook.Test(r.Context())
	if errors.Is(err, notify.ErrDisabled) {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "Notifications are not enabled. Enable the webhook and save before sending a test."})
		return
	}
	if err != nil {
		s.logger.Warn("test notification failed", "error", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "Test notification failed: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "Test notification delivered."})
}
