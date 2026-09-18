package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// configSaveMu serializes Config.Save calls within the process (see Save).
var configSaveMu sync.Mutex

type CloudConfig struct {
	Enabled         bool   `yaml:"enabled"`
	CredentialsFile string `yaml:"credentialsFile"`
}

type GitHubConfig struct {
	ClientID    string `yaml:"clientId"`
	RedirectURL string `yaml:"redirectUrl,omitempty"`
}

// DriveOAuthConfig holds the non-secret Google OAuth application settings for
// the Drive connection. The client secret is intentionally NOT part of user
// configuration: like GitHub, it is supplied via the GITSAFE_DRIVE_CLIENT_SECRET
// environment variable and never persisted, logged, or exposed.
type DriveOAuthConfig struct {
	ClientID    string `yaml:"clientId,omitempty"`
	RedirectURL string `yaml:"redirectUrl,omitempty"`
}

type Config struct {
	// Days is the staleness threshold: a discovered GitHub repository whose
	// last push is at least this many days old is flagged "stale" so the user
	// can decide whether to protect/back it up. 0 makes every repository stale.
	Days       int              `yaml:"days"`
	OutputPath string           `yaml:"outputPath"`
	Cloud      CloudConfig      `yaml:"cloud"`
	GitHub     GitHubConfig     `yaml:"github,omitempty"`
	DriveOAuth DriveOAuthConfig `yaml:"driveOAuth,omitempty"`
	BaseURL    string           `yaml:"baseUrl,omitempty"`
}

func Defaults() Config {
	return Config{
		Days:       30,
		OutputPath: "./backups",
		Cloud: CloudConfig{
			Enabled:         false,
			CredentialsFile: "credentials.json",
		},
	}
}

func Load(path string) (Config, error) {
	cfg := Defaults()
	if path == "" {
		cfg.expandEnv()
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
	return cfg, nil
}

// Save persists the editable settings to a YAML file. The values are stored
// verbatim so that a later Load() re-runs environment expansion.
func (c Config) Save(path string) error {
	// Serialize single-process writers: an HTTP endpoint can trigger a save
	// concurrently with another, and racing renames of the same destination are
	// not safe on Windows. Cross-process writers remain the operator's
	// responsibility; the temp+rename still guarantees no torn file either way.
	configSaveMu.Lock()
	defer configSaveMu.Unlock()

	type cloudOut struct {
		Enabled         bool   `yaml:"enabled"`
		CredentialsFile string `yaml:"credentialsFile"`
	}
	doc := struct {
		OutputPath string           `yaml:"outputPath"`
		Days       int              `yaml:"days"`
		Cloud      cloudOut         `yaml:"cloud"`
		GitHub     GitHubConfig     `yaml:"github,omitempty"`
		DriveOAuth DriveOAuthConfig `yaml:"driveOAuth,omitempty"`
	}{
		OutputPath: c.OutputPath,
		Days:       c.Days,
		Cloud:      cloudOut{Enabled: c.Cloud.Enabled, CredentialsFile: c.Cloud.CredentialsFile},
		GitHub:     c.GitHub,
		DriveOAuth: c.DriveOAuth,
	}

	data, err := yaml.Marshal(doc)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	// Write atomically: produce a temp file in the same directory, sync it to
	// disk, then rename over the target. A failed or interrupted write never
	// leaves a corrupt config behind, and a crash between sync and rename leaves
	// an untouched previous config. A unique temp name also means two concurrent
	// saves (e.g. from different browser tabs) cannot collide on a shared scratch
	// file.
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

func (c Config) Validate() error {
	if c.OutputPath == "" {
		return errors.New("outputPath is required")
	}
	if c.Cloud.Enabled && c.Cloud.CredentialsFile == "" {
		return errors.New("cloud.credentialsFile is required when cloud is enabled")
	}
	return nil
}

func (c *Config) expandEnv() {
	c.OutputPath = os.ExpandEnv(c.OutputPath)
	c.Cloud.CredentialsFile = os.ExpandEnv(c.Cloud.CredentialsFile)
	c.BaseURL = os.ExpandEnv(c.BaseURL)
}

// BaseURLOrDefault returns the configured base URL, defaulting to the local
// development address. The URL must include the scheme (http/https) and
// should not have a trailing slash.
func (c *Config) BaseURLOrDefault() string {
	if c.BaseURL != "" {
		return strings.TrimSuffix(c.BaseURL, "/")
	}
	return "http://127.0.0.1:8080"
}
