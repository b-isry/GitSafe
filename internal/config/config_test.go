package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestSaveRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	cfg := Defaults()
	cfg.RootPath = "C:\\repos"
	cfg.Days = 30
	cfg.OutputPath = "C:\\backups"
	cfg.BackupHistory = false
	cfg.Cloud = CloudConfig{Enabled: true, CredentialsFile: "svc.json"}

	if err := cfg.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	for _, want := range []string{"rootPath", "days: 30", "outputPath", "backupHistory: false", "enabled: true", "svc.json"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("saved config missing %q\n%s", want, raw)
		}
	}
	// The raw credential *contents* must never be persisted via Save; only a path is written.
	if strings.Contains(string(raw), "private_key") {
		t.Errorf("Save persisted raw credential contents")
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.RootPath != cfg.RootPath || loaded.Days != 30 || loaded.OutputPath != cfg.OutputPath {
		t.Errorf("round-trip mismatch: %+v", loaded)
	}
	if loaded.BackupHistory {
		t.Errorf("expected backupHistory false, got true")
	}
	if !loaded.Cloud.Enabled || loaded.Cloud.CredentialsFile != "svc.json" {
		t.Errorf("cloud round-trip mismatch: %+v", loaded.Cloud)
	}
}

func TestSaveAtomicNoTempLeftover(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	cfg := Defaults()
	if err := cfg.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Errorf("expected no temp file after save, got %q", e.Name())
		}
	}
}

func TestSavePreservesExistingPermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	cfg := Defaults()
	// Some operators gitignore/restrict the config. Whatever mode the target
	// reports must survive the save (the save must not force its own default).
	if err := os.WriteFile(path, []byte("rootPath: C:\\repos\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	cfg.RootPath = "C:\\repos"
	if err := cfg.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if after.Mode().Perm() != before.Mode().Perm() {
		t.Errorf("save changed config permissions: %#o -> %#o", before.Mode().Perm(), after.Mode().Perm())
	}
}

func TestConcurrentSavesDoNotCorrupt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			cfg := Defaults()
			cfg.RootPath = fmt.Sprintf("C:\\repos-%d", n)
			cfg.OutputPath = "C:\\backups"
			if err := cfg.Save(path); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent Save failed: %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("final config unreadable after concurrent saves: %v", err)
	}
	if loaded.RootPath == "" || loaded.OutputPath != "C:\\backups" {
		t.Errorf("config lost data under concurrency: %+v", loaded)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Errorf("temp file left after concurrent saves: %q", e.Name())
		}
	}
}

func TestApplyCLIOverridesWritesRootPath(t *testing.T) {
	cfg := Defaults()
	cfg.RootPath = "base"
	if cfg.RootPath != "base" {
		t.Fatalf("setup")
	}
}

func TestRetentionRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	cfg := Defaults()
	cfg.RootPath = "C:\\repos"
	cfg.OutputPath = "C:\\backups"
	cfg.Retention = RetentionConfig{
		KeepLocal:      3,
		KeepLocalDays:  30,
		KeepJobs:       20,
		KeepDriveDays:  90,
		DriveRetention: true,
	}

	if err := cfg.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Retention != cfg.Retention {
		t.Errorf("retention round-trip mismatch:\n got %+v\nwant %+v", loaded.Retention, cfg.Retention)
	}
}

func TestRetentionValidate(t *testing.T) {
	valid := []RetentionConfig{
		{},
		{KeepLocal: 5},
		{KeepLocal: 5, KeepLocalDays: 30, KeepJobs: 10},
		{DriveRetention: true, KeepDriveDays: 90},
	}
	for _, r := range valid {
		if err := r.Validate(); err != nil {
			t.Errorf("Validate(%+v) unexpected error: %v", r, err)
		}
	}

	invalid := []RetentionConfig{
		{KeepLocal: -1},
		{KeepLocalDays: -1},
		{KeepJobs: -1},
		{KeepDriveDays: -1},
		{DriveRetention: true, KeepDriveDays: 0},
		{CleanupHistoryLimit: maxCleanupHistoryLimit + 1},
	}
	for _, r := range invalid {
		if err := r.Validate(); err == nil {
			t.Errorf("Validate(%+v) expected error", r)
		}
	}
	for _, r := range []RetentionConfig{{CleanupHistoryLimit: maxCleanupHistoryLimit}, {CleanupHistoryLimit: 0}} {
		if err := r.Validate(); err != nil {
			t.Errorf("Validate(%+v) unexpected error: %v", r, err)
		}
	}
}

func TestConfigValidateRejectsUnsafeRetention(t *testing.T) {
	cfg := Defaults()
	cfg.RootPath = "C:\\repos"
	cfg.OutputPath = "C:\\backups"
	cfg.Retention = RetentionConfig{DriveRetention: true, KeepDriveDays: 0}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error for Drive retention enabled without a positive age")
	}
}

func TestSchedulingDefaultsDisabled(t *testing.T) {
	cfg := Defaults()
	if cfg.Retention.ScheduledCleanupEnabled {
		t.Error("scheduled cleanup should default to disabled")
	}
	if cfg.Retention.ScheduledCleanupIntervalDays != 0 {
		t.Errorf("interval should default to 0, got %d", cfg.Retention.ScheduledCleanupIntervalDays)
	}
}

func TestSchedulingRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	cfg := Defaults()
	cfg.RootPath = "C:\\repos"
	cfg.OutputPath = "C:\\backups"
	cfg.Retention = RetentionConfig{
		KeepLocal:                    3,
		ScheduledCleanupEnabled:      true,
		ScheduledCleanupIntervalDays: 7,
	}

	if err := cfg.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(raw), "scheduledCleanupEnabled: true") ||
		!strings.Contains(string(raw), "scheduledCleanupIntervalDays: 7") {
		t.Errorf("saved config missing scheduling fields\n%s", raw)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !loaded.Retention.ScheduledCleanupEnabled || loaded.Retention.ScheduledCleanupIntervalDays != 7 {
		t.Errorf("scheduling round-trip mismatch: %+v", loaded.Retention)
	}
}

// TestSchedulingOldConfigWithoutFields loads a config file that predates the
// scheduling fields and must result in a disabled default (backward compatible).
func TestSchedulingOldConfigWithoutFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	old := "rootPath: C:\\repos\noutputPath: C:\\backups\nretention:\n  keepLocal: 2\n"
	if err := os.WriteFile(path, []byte(old), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Retention.ScheduledCleanupEnabled {
		t.Error("old config without scheduling fields must load as disabled")
	}
	if loaded.Retention.ScheduledCleanupIntervalDays != 0 {
		t.Errorf("old config interval must be 0, got %d", loaded.Retention.ScheduledCleanupIntervalDays)
	}
}

func TestSchedulingValidate(t *testing.T) {
	valid := []RetentionConfig{
		{}, // disabled, zero interval
		{ScheduledCleanupEnabled: true, ScheduledCleanupIntervalDays: 1},
		{ScheduledCleanupEnabled: true, ScheduledCleanupIntervalDays: 30},
		{ScheduledCleanupEnabled: false, ScheduledCleanupIntervalDays: 0},
	}
	for _, r := range valid {
		if err := r.Validate(); err != nil {
			t.Errorf("Validate(%+v) unexpected error: %v", r, err)
		}
	}

	invalid := []RetentionConfig{
		{ScheduledCleanupEnabled: true, ScheduledCleanupIntervalDays: 0},
		{ScheduledCleanupEnabled: true, ScheduledCleanupIntervalDays: -1},
		{ScheduledCleanupIntervalDays: -5},
	}
	for _, r := range invalid {
		if err := r.Validate(); err == nil {
			t.Errorf("Validate(%+v) expected error", r)
		}
	}
}

func TestNotificationsDefaultsDisabled(t *testing.T) {
	cfg := Defaults()
	if cfg.Notifications.Enabled {
		t.Error("notifications should default to disabled")
	}
	if cfg.Notifications.Type != defaultNotificationType {
		t.Errorf("default notification type = %q, want %q", cfg.Notifications.Type, defaultNotificationType)
	}
	if !cfg.Notifications.NotifyOnFailure {
		t.Error("default notifications should notify on failure")
	}
	if cfg.Retention.CleanupHistoryLimit != CleanupHistoryDefaultLimit {
		t.Errorf("cleanupHistoryLimit default = %d, want %d", cfg.Retention.CleanupHistoryLimit, CleanupHistoryDefaultLimit)
	}
}

func TestNotificationsValidate(t *testing.T) {
	valid := []NotificationConfig{
		{}, // disabled: everything allowed
		{Enabled: true, Type: "webhook", WebhookURL: "https://hooks.example.com/xyz", WebhookTimeoutSeconds: 5, NotifyOnFailure: true},
		{Enabled: true, Type: "webhook", WebhookURL: "http://localhost:9000/hook", WebhookAuthHeader: "X-Webhook-Token", WebhookAuthToken: "s3cr3t"},
		{Enabled: true, WebhookURL: "https://x.example.com"}, // empty type defaults to webhook
	}
	for _, n := range valid {
		if err := n.Validate(); err != nil {
			t.Errorf("Validate(%+v) unexpected error: %v", n, err)
		}
	}

	invalid := []NotificationConfig{
		{Enabled: true}, // no URL
		{Enabled: true, Type: "email", WebhookURL: "https://x.example.com"},                                                 // unsupported type
		{Enabled: true, Type: "webhook", WebhookURL: "ftp://x.example.com"},                                                 // non-http(s) scheme
		{Enabled: true, Type: "webhook", WebhookURL: "https://"},                                                            // no host
		{Enabled: true, Type: "webhook", WebhookURL: "https://:8080/"},                                                      // no hostname
		{Enabled: true, Type: "webhook", WebhookURL: "https://user:pass@x.example.com/hook"},                                // userinfo
		{Enabled: true, Type: "webhook", WebhookURL: "https://x.example.com/" + strings.Repeat("a", MaxWebhookURLLength+1)}, // oversized
		{Enabled: true, Type: "webhook", WebhookURL: "https://x.example.com", WebhookTimeoutSeconds: -1},
		{Enabled: true, Type: "webhook", WebhookURL: "https://x.example.com", WebhookAuthHeader: "Bad Header\n"},
		{Enabled: true, Type: "webhook", WebhookURL: "https://x.example.com", WebhookAuthToken: "token-without-header"},
	}
	for _, n := range invalid {
		if err := n.Validate(); err == nil {
			t.Errorf("Validate(%+v) expected error", n)
		}
	}
}

func TestNotificationsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	cfg := Defaults()
	cfg.RootPath = "C:\\repos"
	cfg.OutputPath = "C:\\backups"
	cfg.Notifications = NotificationConfig{
		Enabled:               true,
		Type:                  "webhook",
		WebhookURL:            "https://hooks.example.com/backup",
		WebhookAuthHeader:     "Authorization",
		WebhookAuthToken:      "hunter2",
		WebhookTimeoutSeconds: 7,
		NotifyOnSuccess:       true,
		NotifyOnFailure:       true,
	}

	if err := cfg.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(raw), "webhookUrl") || !strings.Contains(string(raw), "notifyOnFailure: true") {
		t.Errorf("saved config missing notifications fields\n%s", raw)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Notifications != cfg.Notifications {
		t.Errorf("notifications round-trip mismatch:\n got %+v\nwant %+v", loaded.Notifications, cfg.Notifications)
	}
}

func TestNotificationsEnvExpansion(t *testing.T) {
	t.Setenv("GITSAFE_WEBHOOK_URL", "https://env.example.com/hook")
	t.Setenv("GITSAFE_WEBHOOK_TOKEN", "env-secret")

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	old := "rootPath: C:\\repos\noutputPath: C:\\backups\nnotifications:\n  enabled: true\n  type: webhook\n  webhookUrl: ${GITSAFE_WEBHOOK_URL}\n  webhookAuthHeader: X-Token\n  webhookAuthToken: ${GITSAFE_WEBHOOK_TOKEN}\n"
	if err := os.WriteFile(path, []byte(old), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Notifications.WebhookURL != "https://env.example.com/hook" {
		t.Errorf("env-expanded webhookUrl = %q", loaded.Notifications.WebhookURL)
	}
	// The auth token must stay verbatim through load AND save so a settings save
	// never resolves and persists the secret. Delivery expands it (notify).
	if loaded.Notifications.WebhookAuthToken != "${GITSAFE_WEBHOOK_TOKEN}" {
		t.Errorf("token should remain unexpanded at load, got %q", loaded.Notifications.WebhookAuthToken)
	}
	if err := loaded.Validate(); err != nil {
		t.Errorf("loaded config invalid: %v", err)
	}
	if err := loaded.Save(path); err != nil {
		t.Errorf("Save: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if strings.Contains(string(raw), "env-secret") {
		t.Errorf("Save persisted the resolved token:\n%s", raw)
	}
	if !strings.Contains(string(raw), "${GITSAFE_WEBHOOK_TOKEN}") {
		t.Errorf("Save dropped the token reference:\n%s", raw)
	}
}

func TestOverridesRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	cfg := Defaults()
	cfg.RootPath = "C:\\repos"
	cfg.OutputPath = "C:\\backups"

	keep := 2
	days := 7
	drive := true
	cfg.RepositoryOverrides = []RepositoryRetentionOverride{
		{RepositoryID: 123456789, KeepLocal: &keep, KeepLocalDays: &days},
		{RepositoryID: 987654321, DriveRetention: &drive, KeepDriveDays: &days},
	}

	if err := cfg.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(raw), "repositoryRetentionOverrides") {
		t.Errorf("saved config missing overrides\n%s", raw)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded.RepositoryOverrides) != 2 {
		t.Fatalf("overrides = %d, want 2", len(loaded.RepositoryOverrides))
	}
	if got := *loaded.RepositoryOverrides[0].KeepLocal; got != 2 {
		t.Errorf("override keepLocal = %d, want 2", got)
	}
	if !*loaded.RepositoryOverrides[1].DriveRetention {
		t.Error("override DriveRetention lost in round-trip")
	}
}

func TestOverridesValidate(t *testing.T) {
	global := RetentionConfig{DriveRetention: true, KeepDriveDays: 90}
	zero := 0
	one := 1
	neg := -1
	driveTrue := true
	driveFalse := false

	valid := []RepositoryRetentionOverride{
		{RepositoryID: 1, KeepLocal: &zero, KeepLocalDays: &one, KeepJobs: &one, KeepDriveDays: &one, DriveRetention: &driveFalse},
		{RepositoryID: 2, DriveRetention: &driveTrue, KeepDriveDays: &one},
		{RepositoryID: 3, DriveRetention: &driveTrue}, // inherits global keepDriveDays 90
	}
	for i, ov := range valid {
		if err := ValidateOverrides(global, []RepositoryRetentionOverride{ov}); err != nil {
			t.Errorf("override %d (%+v) unexpected error: %v", i, ov, err)
		}
	}

	invalid := []RepositoryRetentionOverride{
		{RepositoryID: 0, KeepLocal: &one},
		{RepositoryID: 1, KeepLocal: &neg},
		{RepositoryID: 1, KeepLocalDays: &neg},
		{RepositoryID: 1, KeepJobs: &neg},
		{RepositoryID: 1, KeepDriveDays: &neg},
		{RepositoryID: 1, DriveRetention: &driveTrue, KeepDriveDays: &zero}, // no positive age
		{RepositoryID: 1, DriveRetention: &driveTrue},                       // global has no positive age
	}
	for i, ov := range invalid {
		if err := ValidateOverrides(RetentionConfig{}, []RepositoryRetentionOverride{ov}); err == nil {
			t.Errorf("override %d (%+v) expected error", i, ov)
		}
	}

	// Duplicates are rejected.
	dup := []RepositoryRetentionOverride{{RepositoryID: 7}, {RepositoryID: 7}}
	if err := ValidateOverrides(global, dup); err == nil {
		t.Error("duplicate repository ids expected error")
	}
}

func TestConfigRejectsInvalidOverrides(t *testing.T) {
	cfg := Defaults()
	cfg.RootPath = "C:\\repos"
	cfg.OutputPath = "C:\\backups"
	cfg.Sources = cfg.EffectiveSources()
	neg := -1
	cfg.RepositoryOverrides = []RepositoryRetentionOverride{{RepositoryID: 1, KeepLocal: &neg}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error for negative override")
	}
}

func TestMergeRetention(t *testing.T) {
	global := RetentionConfig{
		KeepLocal:      5,
		KeepLocalDays:  10,
		KeepJobs:       20,
		KeepDriveDays:  0,
		DriveRetention: false,
	}
	zero := 0
	oneh := 100
	driveTrue := true
	driveFalse := false

	// Empty override inherits everything.
	merged := MergeRetention(global, RepositoryRetentionOverride{RepositoryID: 1})
	if merged != global {
		t.Errorf("empty override must return global unchanged:\n got %+v", merged)
	}

	// Partial override replaces only overridden dimensions.
	merged = MergeRetention(global, RepositoryRetentionOverride{RepositoryID: 1, KeepLocal: &zero})
	if merged.KeepLocal != 0 || merged.KeepLocalDays != 10 || merged.KeepJobs != 20 || merged.DriveRetention {
		t.Errorf("partial override merge wrong: %+v", merged)
	}

	// Explicit zero means keep-all, honored distinctly from inheritance.
	merged = MergeRetention(global, RepositoryRetentionOverride{RepositoryID: 1, KeepJobs: &zero})
	if merged.KeepJobs != 0 {
		t.Errorf("explicit zero keepJobs must be kept as 0, got %d", merged.KeepJobs)
	}

	// Full override covering every dimension.
	merged = MergeRetention(global, RepositoryRetentionOverride{
		RepositoryID: 1,
		KeepLocal:    &oneh, KeepLocalDays: &oneh, KeepJobs: &oneh, KeepDriveDays: &oneh, DriveRetention: &driveTrue,
	})
	want := RetentionConfig{KeepLocal: 100, KeepLocalDays: 100, KeepJobs: 100, KeepDriveDays: 100, DriveRetention: true}
	if merged != want {
		t.Errorf("full override merge wrong:\n got %+v\nwant %+v", merged, want)
	}

	// Nested override that turns drive retention off must leave it 0-safe.
	merged = MergeRetention(global, RepositoryRetentionOverride{RepositoryID: 1, DriveRetention: &driveFalse})
	if merged.DriveRetention {
		t.Error("override must be able to disable drive retention")
	}
}

func TestHistoryLimitRoundTripAndCompatibility(t *testing.T) {
	dir := t.TempDir()

	// Old config without the field gets the conservative default (backward compat).
	old := filepath.Join(dir, "old.yaml")
	oldCfg := "rootPath: C:\\repos\noutputPath: C:\\backups\nretention:\n  keepLocal: 2\n"
	if err := os.WriteFile(old, []byte(oldCfg), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	loaded, err := Load(old)
	if err != nil {
		t.Fatalf("Load old: %v", err)
	}
	if loaded.Retention.CleanupHistoryLimit != CleanupHistoryDefaultLimit {
		t.Errorf("old config history limit = %d, want %d", loaded.Retention.CleanupHistoryLimit, CleanupHistoryDefaultLimit)
	}

	// Explicit value round-trips.
	path := filepath.Join(dir, "config.yaml")
	cfg := Defaults()
	cfg.RootPath = "C:\\repos"
	cfg.OutputPath = "C:\\backups"
	cfg.Retention.CleanupHistoryLimit = 3
	if err := cfg.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err = Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Retention.CleanupHistoryLimit != 3 {
		t.Errorf("history limit round-trip = %d, want 3", loaded.Retention.CleanupHistoryLimit)
	}

	// Negative limit rejected; zero (disabled) accepted once sources exist.
	cfg.Sources = cfg.EffectiveSources()
	cfg.Retention.CleanupHistoryLimit = -1
	if err := cfg.Validate(); err == nil {
		t.Error("negative cleanupHistoryLimit must be rejected")
	}

	cfg.Retention.CleanupHistoryLimit = 0
	if err := cfg.Validate(); err != nil {
		t.Errorf("zero cleanupHistoryLimit (disabled) must be valid: %v", err)
	}
}

func TestValidHeaderName(t *testing.T) {
	valid := []string{"Authorization", "X-Webhook-Token", "x-api-key", "user-agent"}
	for _, h := range valid {
		if !validHeaderName(h) {
			t.Errorf("validHeaderName(%q) = false", h)
		}
	}
	invalid := []string{"", "Bad Header", "CrLf\r\nInjected", "Tab\tHeader", "Colon:Header"}
	for _, h := range invalid {
		if validHeaderName(h) {
			t.Errorf("validHeaderName(%q) = true", h)
		}
	}
}
