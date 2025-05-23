package main

import (
	"fmt"
	"log"
	"os"


	"github.com/b-isry/gitsafe/archiver"
	"github.com/b-isry/gitsafe/cloud"
	"github.com/b-isry/gitsafe/scanner"
	"github.com/urfave/cli/v2"
)

func main() {
	app := &cli.App{
		Name: "GitSafe",
		Usage: "Backup stale git rpos by zipping and optionally uploading to Googl Drive",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name: "root",
				Usage: "Root directory to scan for git repos",
				Required: true,
			},
			&cli.IntFlag{
				Name: "days",
				Usage: "Days since last commit to consider a repo stale",
				Value: 60,
			},
			&cli.StringFlag{
				Name: "out",
				Usage: "Output directory for archived repos",
				Value: "./backups",
			},
			&cli.BoolFlag{
				Name: "cloud",
				Usage: "Upload zipped repos  to Google Drive",
				Value: false,
			},
		},
		Action: func(ctx *cli.Context) error {
			root := ctx.String("root")
			days := ctx.Int("days")
			outdir := ctx.String("out")
			uploadToCloud := ctx.Bool("cloud")
				
			log.Printf("Scanning %s for stale repos (>%d days)...\n", root, days)

			staleRepos, err := scanner.FindStaleRepos(root, days)
			if err != nil{
				return fmt.Errorf("failed to scan repos: %v", err)
			}

			if len(staleRepos) == 0{
				fmt.Println("No stale repos found.")
				return nil
			}
			for _, repo := range staleRepos {
				fmt.Printf("Archiving: %s\n", repo)
				zipPath, err := archiver.ZipRepo(repo, outdir)
				if err != nil {
					log.Printf("failed to zip %s: %v\n", repo, err)
					continue
				}

				fmt.Printf("Zipped to %s\n", zipPath)

				if uploadToCloud {
					fmt.Printf("Uploading to drive: %s\n", zipPath)
					err = cloud.UploadToDrive(zipPath)
					if err != nil {
						log.Printf("Upload failed: %v\n", err)
					} else {
						fmt.Println("Uploaded successfully.")
					}
				}
			}
			fmt.Println("Done.")
			return nil
		},
	}
	if err := app.Run(os.Args); err != nil{
		log.Fatal(err)
	}
}