package config

import (
	"errors"
	"fmt"
	"os"

	"github.com/urfave/cli/v2"
	"gopkg.in/yaml.v3"
)

type CloudConfig struct {
	Enabled         bool   `yaml:"enabled"`
	CredentialsFile string `yaml:"credentialsFile"`
}

type Config struct {
	RootPath      string      `yaml:"rootPath"`
	Days          int         `yaml:"days"`
	OutputPath    string      `yaml:"outputPath"`
	BackupHistory bool        `yaml:"backupHistory"`
	Cloud         CloudConfig `yaml:"cloud"`
}

func Defaults() Config {
	return Config{
		Days:          60,
		OutputPath:    "./backups",
		BackupHistory: true,
		Cloud: CloudConfig{
			Enabled:         false,
			CredentialsFile: "credentials.json",
		},
	}
}

func Load(path string) (Config, error) {
	cfg := Defaults()
	if path == "" {
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

	return cfg, nil
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
}

func (c Config) Validate() error {
	if c.RootPath == "" {
		return errors.New("rootPath is required (config or --root)")
	}
	if c.Days < 0 {
		return errors.New("days must be >= 0")
	}
	if c.OutputPath == "" {
		return errors.New("outputPath is required (config or --out)")
	}
	if c.Cloud.Enabled && c.Cloud.CredentialsFile == "" {
		return errors.New("cloud.credentialsFile is required when cloud is enabled")
	}
	return nil
}
