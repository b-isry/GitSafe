package main

import (
	"log/slog"
	"os"

	"github.com/b-isry/gitsafe/internal/archiver"
	"github.com/b-isry/gitsafe/internal/cloud"
	"github.com/b-isry/gitsafe/internal/config"
	"github.com/b-isry/gitsafe/internal/scanner"
	"github.com/urfave/cli/v2"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	app := &cli.App{
		Name:  "GitSafe",
		Usage: "Disaster recovery backup for stale git repositories",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  "config",
				Usage: "Path to YAML config file",
				Value: "config.yaml",
			},
			&cli.StringFlag{
				Name:  "root",
				Usage: "Root directory to scan for git repos (overrides config)",
			},
			&cli.IntFlag{
				Name:  "days",
				Usage: "Days since last commit to consider a repo stale",
				Value: 60,
			},
			&cli.StringFlag{
				Name:  "out",
				Usage: "Output directory for backup bundles",
				Value: "./backups",
			},
			&cli.BoolFlag{
				Name:  "cloud",
				Usage: "Upload backups to Google Drive (overrides config)",
				Value: false,
			},
			&cli.BoolFlag{
				Name:  "backup-history",
				Usage: "Preserve full git history in backup bundles (overrides config)",
				Value: true,
			},
		},
		Action: func(ctx *cli.Context) error {
			cfg, err := config.Load(ctx.String("config"))
			if err != nil {
				return err
			}
			config.ApplyCLIOverrides(ctx, &cfg)

			if err := cfg.Validate(); err != nil {
				return err
			}
			if err := archiver.Validate(); err != nil {
				return err
			}

			logger.Info("starting repository scan", "rootPath", cfg.RootPath, "days", cfg.Days)
			staleRepos, err := scanner.FindStaleRepos(cfg.RootPath, cfg.Days, logger)
			if err != nil {
				return err
			}

			if len(staleRepos) == 0 {
				logger.Info("no stale repositories found")
				return nil
			}

			for _, repo := range staleRepos {
				bundlePath, err := archiver.BundleRepo(repo, cfg.OutputPath, cfg.BackupHistory, logger)
				if err != nil {
					logger.Error("failed to create backup bundle", "repo", repo, "error", err)
					continue
				}

				if cfg.Cloud.Enabled {
					err = cloud.UploadToDrive(ctx.Context, bundlePath, cfg.Cloud, logger)
					if err != nil {
						logger.Error("cloud upload failed", "bundlePath", bundlePath, "error", err)
					}
				}
			}

			logger.Info("backup run completed", "staleRepos", len(staleRepos))
			return nil
		},
	}

	if err := app.Run(os.Args); err != nil {
		logger.Error("gitsafe failed", "error", err)
		os.Exit(1)
	}
}
