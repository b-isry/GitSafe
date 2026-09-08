package main

import (
	"log/slog"
	"os"

	"github.com/b-isry/gitsafe/internal/archiver"
	"github.com/b-isry/gitsafe/internal/cloud"
	"github.com/b-isry/gitsafe/internal/config"
	"github.com/b-isry/gitsafe/internal/providers"
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

			sources := cfg.EffectiveSources()
			logger.Info("starting backup run", "sources", len(sources), "days", cfg.Days, "outputPath", cfg.OutputPath)

			createdBundles := 0
			for _, source := range sources {
				switch source.Type {
				case config.SourceTypeLocal:
					staleRepos, err := scanner.FindStaleRepos(source.Path, cfg.Days, logger)
					if err != nil {
						logger.Error("local source scan failed", "sourceName", source.Name, "path", source.Path, "error", err)
						continue
					}

					if len(staleRepos) == 0 {
						logger.Info("no stale local repositories found", "sourceName", source.Name, "path", source.Path)
						continue
					}

					for _, repo := range staleRepos {
						bundlePath, err := archiver.BundleRepo(repo, cfg.OutputPath, cfg.BackupHistory, logger)
						if err != nil {
							logger.Error("failed to create local backup bundle", "repo", repo, "error", err)
							continue
						}
						createdBundles++
						uploadBundleIfEnabled(ctx, logger, cfg, bundlePath)
					}

				case config.SourceTypeGitHub:
					provider := providers.NewGitHubProvider(source.Token)
					repositories, err := provider.GetRepositories()
					if err != nil {
						logger.Error("github source fetch failed", "sourceName", source.Name, "error", err)
						continue
					}

					if len(repositories) == 0 {
						logger.Info("no repositories returned from github source", "sourceName", source.Name)
						continue
					}

					for _, repo := range repositories {
						bundlePath, err := archiver.BundleRemoteRepo(repo.CloneURL, cfg.OutputPath)
						if err != nil {
							logger.Error("failed to create remote backup bundle", "sourceName", source.Name, "repository", repo.Name, "cloneURL", repo.CloneURL, "error", err)
							continue
						}

						logger.Info("remote backup bundle created", "sourceName", source.Name, "repository", repo.Name, "bundlePath", bundlePath)
						createdBundles++
						uploadBundleIfEnabled(ctx, logger, cfg, bundlePath)
					}

				default:
					logger.Error("skipping unsupported source type", "sourceName", source.Name, "type", source.Type)
				}
			}

			logger.Info("backup run completed", "bundlesCreated", createdBundles)
			return nil
		},
	}

	if err := app.Run(os.Args); err != nil {
		logger.Error("gitsafe failed", "error", err)
		os.Exit(1)
	}
}

func uploadBundleIfEnabled(ctx *cli.Context, logger *slog.Logger, cfg config.Config, bundlePath string) {
	if !cfg.Cloud.Enabled {
		return
	}

	if err := cloud.UploadToDrive(ctx.Context, bundlePath, cfg.Cloud, logger, nil); err != nil {
		logger.Error("cloud upload failed", "bundlePath", bundlePath, "error", err)
	}
}
