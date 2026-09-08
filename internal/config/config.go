package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/urfave/cli/v2"
	"gopkg.in/yaml.v3"
)

// configSaveMu serializes Config.Save calls within the process (see Save).
var configSaveMu sync.Mutex

type SourceType string

const (
	SourceTypeLocal  SourceType = "local"
	SourceTypeGitHub SourceType = "github"
)

type CloudConfig struct {
	Enabled         bool   `yaml:"enabled"`
	CredentialsFile string `yaml:"credentialsFile"`
}

type Source struct {
	Type  SourceType `yaml:"type"`
	Name  string     `yaml:"name"`
	Path  string     `yaml:"path"`
	Token string     `yaml:"token"`
}

type GitHubConfig struct {
	ClientID    string `yaml:"clientId"`
	RedirectURL string `yaml:"redirectUrl,omitempty"`
}

// CleanupHistoryDefaultLimit is the conservative default cap on persisted
// cleanup-history entries. Old config files without the field get this value.
const CleanupHistoryDefaultLimit = 50

// MaxWebhookURLLength bounds the webhook URL so a damaged or oversized config
// cannot produce unbounded outbound requests or oversized error strings.
const MaxWebhookURLLength = 2048

// NotificationConfig controls how completed retention-cleanup runs are
// delivered to external targets. Notifications are disabled by default; every
// field is conservative so an unconfigured install never makes outbound
// requests. Goal: cleanup success/failure must never depend on a notification.
type NotificationConfig struct {
	Enabled bool `yaml:"enabled"`
	// Type is the notification target; currently only "webhook".
	Type string `yaml:"type"`
	// WebhookURL is the destination for webhook notifications. Only http/https
	// are accepted; other schemes (and embedded userinfo credentials) are
	// rejected by Validate so a config mistake cannot turn into an arbitrary
	// outbound call or leak credentials through error messages.
	WebhookURL string `yaml:"webhookUrl,omitempty"`
	// WebhookAuthHeader is the optional HTTP header carrying WebhookAuthToken
	// (e.g. "Authorization" or "X-Webhook-Token").
	WebhookAuthHeader string `yaml:"webhookAuthHeader,omitempty"`
	// WebhookAuthToken is a secret sent in WebhookAuthHeader. It is expanded
	// from the environment at delivery time (never at load/save), NEVER logged,
	// and NEVER returned by any API. Saving keeps the raw representation
	// (e.g. "${WEBHOOK_TOKEN}" or a literal) so a settings save can never write
	// the resolved secret back to disk.
	WebhookAuthToken string `yaml:"webhookAuthToken,omitempty"`
	// WebhookTimeoutSeconds bounds a single webhook POST. 0 means the default
	// (see notify.DefaultTimeout).
	WebhookTimeoutSeconds int `yaml:"webhookTimeoutSeconds,omitempty"`
	// NotifyOnSuccess sends notifications for successful runs.
	NotifyOnSuccess bool `yaml:"notifyOnSuccess"`
	// NotifyOnFailure sends notifications for failed runs. Default true when
	// notifications are enabled (failures are the important case).
	NotifyOnFailure bool `yaml:"notifyOnFailure"`
}

// defaultNotificationType is the only supported notification target.
const defaultNotificationType = "webhook"

// RepositoryRetentionOverride overrides specific retention dimensions for a
// single protected repository. The repository is identified by its immutable
// GitHub repository ID (githubId), the canonical identity used throughout the
// cloud state model. Nil fields inherit the global policy; explicit zero values
// mean "keep all" for that dimension (matching global semantics). A repository
// without an override always uses the global policy exactly.
type RepositoryRetentionOverride struct {
	RepositoryID  int64 `yaml:"repositoryId"`
	KeepLocal     *int  `yaml:"keepLocal,omitempty"`
	KeepLocalDays *int  `yaml:"keepLocalDays,omitempty"`
	KeepJobs      *int  `yaml:"keepJobs,omitempty"`
	KeepDriveDays *int  `yaml:"keepDriveDays,omitempty"`
	// DriveRetention nil inherits the global switch. True requires a positive
	// keepDriveDays (override or global) — enforced by Validate so an override
	// can never silently disable the drive-safety constraint.
	DriveRetention *bool `yaml:"driveRetentionEnabled,omitempty"`
}

// RetentionConfig controls the deterministic retention/cleanup policy. All-zero
// fields / a disabled switch keep everything: retention is purely opt-in so an
// unconfigured (or freshly-upgraded) install never deletes data unintentionally.
type RetentionConfig struct {
	// KeepLocal is the maximum number of local backups (bundles + records) to
	// retain per protected repository. 0 means no count-based limit.
	KeepLocal int `yaml:"keepLocal"`
	// KeepLocalDays is the maximum age in days a local backup may be before it
	// is eligible for removal. 0 means no age-based limit.
	KeepLocalDays int `yaml:"keepLocalDays"`
	// KeepJobs is the maximum number of terminal (completed/failed/interrupted)
	// jobs to retain per protected repository. 0 means keep all job history.
	KeepJobs int `yaml:"keepJobs"`
	// KeepDriveDays is the minimum age in days a Drive copy must be before it is
	// eligible for removal. 0 means never remove Drive copies, regardless of
	// DriveRetention.
	KeepDriveDays int `yaml:"keepDriveDays"`
	// DriveRetention gates ANY Drive deletion. It must be true AND KeepDriveDays
	// greater than zero for Drive copies to ever be removed.
	DriveRetention bool `yaml:"driveRetentionEnabled"`
	// ScheduledCleanupEnabled turns on periodic (scheduled) cleanup. Disabled by
	// default so an unconfigured or freshly-upgraded install only cleans up when
	// the user explicitly enables scheduling.
	ScheduledCleanupEnabled bool `yaml:"scheduledCleanupEnabled"`
	// ScheduledCleanupIntervalDays is how often a scheduled cleanup runs, in
	// days. It must be greater than zero whenever ScheduledCleanupEnabled is
	// true. 0 means no interval (treated as invalid when enabled).
	ScheduledCleanupIntervalDays int `yaml:"scheduledCleanupIntervalDays"`
	// CleanupHistoryLimit bounds the persisted cleanup-history entries (newest
	// kept). 0 disables history recording; missing in old configs defaults to
	// CleanupHistoryDefaultLimit.
	CleanupHistoryLimit int `yaml:"cleanupHistoryLimit"`
}

type Config struct {
	RootPath            string                        `yaml:"rootPath"`
	Days                int                           `yaml:"days"`
	OutputPath          string                        `yaml:"outputPath"`
	BackupHistory       bool                          `yaml:"backupHistory"`
	Retention           RetentionConfig               `yaml:"retention"`
	Cloud               CloudConfig                   `yaml:"cloud"`
	Sources             []Source                      `yaml:"sources"`
	GitHub              GitHubConfig                  `yaml:"github,omitempty"`
	Notifications       NotificationConfig            `yaml:"notifications"`
	RepositoryOverrides []RepositoryRetentionOverride `yaml:"repositoryRetentionOverrides"`
}

func Defaults() Config {
	return Config{
		Days:          60,
		OutputPath:    "./backups",
		BackupHistory: true,
		Retention: RetentionConfig{
			KeepLocal:                    0,
			KeepLocalDays:                0,
			KeepJobs:                     0,
			KeepDriveDays:                0,
			DriveRetention:               false,
			ScheduledCleanupEnabled:      false,
			ScheduledCleanupIntervalDays: 0,
			CleanupHistoryLimit:          CleanupHistoryDefaultLimit,
		},
		Cloud: CloudConfig{
			Enabled:         false,
			CredentialsFile: "credentials.json",
		},
		Notifications: NotificationConfig{
			Type:            defaultNotificationType,
			NotifyOnFailure: true,
		},
	}
}

func Load(path string) (Config, error) {
	cfg := Defaults()
	if path == "" {
		cfg.expandEnv()
		cfg.normalizeSources()
		return cfg, nil
	}

	content, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, nil
		}
		return Config{}, fmt.Errorf("read config file %q: %w", path, err)
	}

	if err := yaml.Unmarshal(content, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config file %q: %w", path, err)
	}

	cfg.expandEnv()
	cfg.normalizeSources()
	return cfg, nil
}

// Save persists the editable settings to a YAML file. Only the fields exposed
// by the Settings UI are written; synthesized sources and any transient
// normalization are never persisted. The values are stored verbatim so that a
// later Load() re-runs environment expansion.
func (c Config) Save(path string) error {
	// Serialize single-process writers: several HTTP endpoints (settings,
	// setup, retention) can trigger a save concurrently, and racing renames of
	// the same destination are not safe on Windows. Cross-process writers remain
	// the operator's responsibility; the temp+rename still guarantees no torn
	// file either way.
	configSaveMu.Lock()
	defer configSaveMu.Unlock()

	type cloudOut struct {
		Enabled         bool   `yaml:"enabled"`
		CredentialsFile string `yaml:"credentialsFile"`
	}
	doc := struct {
		RootPath                     string                        `yaml:"rootPath"`
		Days                         int                           `yaml:"days"`
		OutputPath                   string                        `yaml:"outputPath"`
		BackupHistory                bool                          `yaml:"backupHistory"`
		Retention                    RetentionConfig               `yaml:"retention"`
		Cloud                        cloudOut                      `yaml:"cloud"`
		GitHub                       GitHubConfig                  `yaml:"github,omitempty"`
		Notifications                NotificationConfig            `yaml:"notifications"`
		RepositoryRetentionOverrides []RepositoryRetentionOverride `yaml:"repositoryRetentionOverrides"`
	}{
		RootPath:                     c.RootPath,
		Days:                         c.Days,
		OutputPath:                   c.OutputPath,
		BackupHistory:                c.BackupHistory,
		Retention:                    c.Retention,
		Cloud:                        cloudOut{Enabled: c.Cloud.Enabled, CredentialsFile: c.Cloud.CredentialsFile},
		GitHub:                       c.GitHub,
		Notifications:                c.Notifications,
		RepositoryRetentionOverrides: c.RepositoryOverrides,
	}

	data, err := yaml.Marshal(doc)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	// Write atomically: produce a temp file in the same directory, sync it to
	// disk, then rename over the target. A failed or interrupted write never
	// leaves a corrupt config behind, and a crash between sync and rename leaves
	// an untouched previous config. A unique temp name also means two concurrent
	// saves (e.g. settings + retention from different browser tabs) cannot
	// collide on a shared scratch file.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".gitsafe-config-*.tmp")
	if err != nil {
		return fmt.Errorf("create config temp file in %q: %w", filepath.Dir(path), err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("write config file %q: %w", path, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("sync config file %q: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("close config file %q: %w", path, err)
	}
	// Keep the previous mode (e.g. a gitignored or permission-restricted file)
	// rather than forcing 0644 on every save.
	if info, err := os.Stat(path); err == nil {
		_ = os.Chmod(tmpName, info.Mode().Perm())
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("finalize config file %q: %w", path, err)
	}
	return nil
}

func ApplyCLIOverrides(ctx *cli.Context, cfg *Config) {
	if ctx.IsSet("root") {
		cfg.RootPath = ctx.String("root")
	}
	if ctx.IsSet("days") {
		cfg.Days = ctx.Int("days")
	}
	if ctx.IsSet("out") {
		cfg.OutputPath = ctx.String("out")
	}
	if ctx.IsSet("cloud") {
		cfg.Cloud.Enabled = ctx.Bool("cloud")
	}
	if ctx.IsSet("backup-history") {
		cfg.BackupHistory = ctx.Bool("backup-history")
	}
	if ctx.IsSet("root") {
		cfg.overrideLocalSource(ctx.String("root"))
	}
}

func (c Config) Validate() error {
	if c.Days < 0 {
		return errors.New("days must be >= 0")
	}
	if c.OutputPath == "" {
		return errors.New("outputPath is required (config or --out)")
	}
	if c.Cloud.Enabled && c.Cloud.CredentialsFile == "" {
		return errors.New("cloud.credentialsFile is required when cloud is enabled")
	}
	if err := c.Retention.Validate(); err != nil {
		return err
	}
	if err := c.Notifications.Validate(); err != nil {
		return err
	}
	if err := ValidateOverrides(c.Retention, c.RepositoryOverrides); err != nil {
		return err
	}
	if len(c.Sources) == 0 {
		return errors.New("at least one source is required (sources or rootPath)")
	}
	for i, src := range c.Sources {
		switch src.Type {
		case SourceTypeLocal:
			if src.Path == "" {
				return fmt.Errorf("sources[%d].path is required for local source", i)
			}
		case SourceTypeGitHub:
			if src.Token == "" {
				return fmt.Errorf("sources[%d].token is required for github source", i)
			}
		default:
			return fmt.Errorf("sources[%d].type %q is not supported", i, src.Type)
		}
	}
	return nil
}

// Validate rejects unsafe retention values. Retention is destructive, so any
// negative count/age (which would invert the policy into an unbounded delete) is
// refused, and Drive retention must be explicitly enabled with a positive age
// before any Drive file can be targeted.
func (r RetentionConfig) Validate() error {
	if r.KeepLocal < 0 {
		return errors.New("retention.keepLocal must be >= 0 (0 = keep all)")
	}
	if r.KeepLocalDays < 0 {
		return errors.New("retention.keepLocalDays must be >= 0 (0 = keep all)")
	}
	if r.KeepJobs < 0 {
		return errors.New("retention.keepJobs must be >= 0 (0 = keep all)")
	}
	if r.KeepDriveDays < 0 {
		return errors.New("retention.keepDriveDays must be >= 0 (0 = never delete Drive copies)")
	}
	if r.DriveRetention && r.KeepDriveDays <= 0 {
		return errors.New("retention.driveRetentionEnabled requires keepDriveDays > 0")
	}
	if r.ScheduledCleanupIntervalDays < 0 {
		return errors.New("retention.scheduledCleanupIntervalDays must be >= 0")
	}
	if r.ScheduledCleanupEnabled && r.ScheduledCleanupIntervalDays <= 0 {
		return errors.New("retention.scheduledCleanupEnabled requires scheduledCleanupIntervalDays > 0")
	}
	if r.CleanupHistoryLimit < 0 {
		return errors.New("retention.cleanupHistoryLimit must be >= 0 (0 = disabled)")
	}
	if r.CleanupHistoryLimit > maxCleanupHistoryLimit {
		return fmt.Errorf("retention.cleanupHistoryLimit must be <= %d", maxCleanupHistoryLimit)
	}
	return nil
}

// maxCleanupHistoryLimit caps how many cleanup-history entries may be
// persisted, so a damaged config cannot make the history log unbounded.
const maxCleanupHistoryLimit = 10000

// validHeaderName reports whether s is a legal single HTTP header field name
// (RFC 9110 tchar list) so a stale config value cannot smuggle CRLF injection.
func validHeaderName(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			continue
		}
		if strings.ContainsRune("!#$%&'*+-.^_`|~", c) {
			continue
		}
		return false
	}
	return true
}

func (n NotificationConfig) Validate() error {
	if !n.Enabled {
		return nil
	}
	if n.Type == "" {
		n.Type = defaultNotificationType
	}
	if n.Type != defaultNotificationType {
		return fmt.Errorf("notifications.type %q is not supported (only %q)", n.Type, defaultNotificationType)
	}
	if n.WebhookURL == "" {
		return errors.New("notifications.webhookUrl is required when notifications are enabled")
	}
	u, err := url.Parse(n.WebhookURL)
	if err != nil {
		return fmt.Errorf("notifications.webhookUrl is not a valid URL: %v", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("notifications.webhookUrl must use http or https")
	}
	if u.Host == "" || u.Hostname() == "" {
		return errors.New("notifications.webhookUrl must include a host")
	}
	if u.User != nil {
		return errors.New("notifications.webhookUrl must not contain userinfo (username or password)")
	}
	if len(n.WebhookURL) > MaxWebhookURLLength {
		return fmt.Errorf("notifications.webhookUrl must not exceed %d characters", MaxWebhookURLLength)
	}
	if n.WebhookTimeoutSeconds < 0 {
		return errors.New("notifications.webhookTimeoutSeconds must be >= 0")
	}
	if n.WebhookAuthHeader != "" && !validHeaderName(n.WebhookAuthHeader) {
		return fmt.Errorf("notifications.webhookAuthHeader %q is not a valid HTTP header name", n.WebhookAuthHeader)
	}
	if n.WebhookAuthHeader == "" && n.WebhookAuthToken != "" {
		return errors.New("notifications.webhookAuthToken requires webhookAuthHeader")
	}
	return nil
}

// ValidateOverrides rejects override entries that could invert the retention
// policy. The global RetentionConfig is needed for cross-override safety checks
// (e.g. a drive override requiring a positive effective keepDriveDays).
func ValidateOverrides(global RetentionConfig, overrides []RepositoryRetentionOverride) error {
	seen := map[int64]bool{}
	for i, ov := range overrides {
		if ov.RepositoryID <= 0 {
			return fmt.Errorf("repositoryRetentionOverrides[%d].repositoryId must be a positive GitHub repository id", i)
		}
		if seen[ov.RepositoryID] {
			return fmt.Errorf("repositoryRetentionOverrides[%d].repositoryId %d is duplicated", i, ov.RepositoryID)
		}
		seen[ov.RepositoryID] = true
		if ov.KeepLocal != nil && *ov.KeepLocal < 0 {
			return fmt.Errorf("repositoryRetentionOverrides[%d].keepLocal must be >= 0 (0 = keep all)", i)
		}
		if ov.KeepLocalDays != nil && *ov.KeepLocalDays < 0 {
			return fmt.Errorf("repositoryRetentionOverrides[%d].keepLocalDays must be >= 0 (0 = keep all)", i)
		}
		if ov.KeepJobs != nil && *ov.KeepJobs < 0 {
			return fmt.Errorf("repositoryRetentionOverrides[%d].keepJobs must be >= 0 (0 = keep all)", i)
		}
		if ov.KeepDriveDays != nil && *ov.KeepDriveDays < 0 {
			return fmt.Errorf("repositoryRetentionOverrides[%d].keepDriveDays must be >= 0 (0 = never delete Drive copies)", i)
		}
		if ov.DriveRetention != nil && *ov.DriveRetention {
			effective := global.KeepDriveDays
			if ov.KeepDriveDays != nil {
				effective = *ov.KeepDriveDays
			}
			if effective <= 0 {
				return fmt.Errorf("repositoryRetentionOverrides[%d] enables drive retention but has no positive keepDriveDays (override or global)", i)
			}
		}
	}
	return nil
}

// MergeRetention computes the effective policy for a repository by layering a
// single override on top of the global policy. Nil override fields inherit the
// global value; set fields (including explicit zero) fully replace it. This is
// deliberately an additive seam over the existing engine: a repository without
// an override gets the global policy unchanged.
func MergeRetention(global RetentionConfig, ov RepositoryRetentionOverride) RetentionConfig {
	merged := global
	if ov.KeepLocal != nil {
		merged.KeepLocal = *ov.KeepLocal
	}
	if ov.KeepLocalDays != nil {
		merged.KeepLocalDays = *ov.KeepLocalDays
	}
	if ov.KeepJobs != nil {
		merged.KeepJobs = *ov.KeepJobs
	}
	if ov.KeepDriveDays != nil {
		merged.KeepDriveDays = *ov.KeepDriveDays
	}
	if ov.DriveRetention != nil {
		merged.DriveRetention = *ov.DriveRetention
	}
	return merged
}

func (c Config) EffectiveSources() []Source {
	if len(c.Sources) > 0 {
		return c.Sources
	}
	if c.RootPath == "" {
		return nil
	}
	return []Source{
		{
			Type: SourceTypeLocal,
			Name: "local-default",
			Path: c.RootPath,
		},
	}
}

func (c *Config) expandEnv() {
	c.OutputPath = os.ExpandEnv(c.OutputPath)
	c.Cloud.CredentialsFile = os.ExpandEnv(c.Cloud.CredentialsFile)
	c.RootPath = os.ExpandEnv(c.RootPath)
	for i := range c.Sources {
		c.Sources[i].Path = os.ExpandEnv(c.Sources[i].Path)
		c.Sources[i].Token = strings.TrimSpace(os.ExpandEnv(c.Sources[i].Token))
	}
	c.Notifications.WebhookURL = os.ExpandEnv(c.Notifications.WebhookURL)
	// WebhookAuthToken is deliberately NOT expanded here: Save() persists the
	// in-memory value, so expanding at load would write the resolved secret back
	// to disk on the next settings save. The token is expanded at delivery time
	// (notify.resolve), where unset variables resolve to "" just like Load-time
	// expansion would.
}

func (c *Config) normalizeSources() {
	if len(c.Sources) == 0 && c.RootPath != "" {
		c.Sources = []Source{
			{
				Type: SourceTypeLocal,
				Name: "local-default",
				Path: c.RootPath,
			},
		}
	}
}

func (c *Config) overrideLocalSource(rootPath string) {
	for i := range c.Sources {
		if c.Sources[i].Type == SourceTypeLocal {
			c.Sources[i].Path = rootPath
			return
		}
	}
	c.Sources = append(c.Sources, Source{
		Type: SourceTypeLocal,
		Name: "local-override",
		Path: rootPath,
	})
}
