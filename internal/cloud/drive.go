package cloud

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/b-isry/gitsafe/internal/config"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/option"
)

func UploadToDrive(ctx context.Context, filename string, cloudCfg config.CloudConfig, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}

	credentials, err := os.ReadFile(cloudCfg.CredentialsFile)
	if err != nil {
		return fmt.Errorf("read credentials %q: %w", cloudCfg.CredentialsFile, err)
	}

	jwtConfig, err := google.JWTConfigFromJSON(credentials, drive.DriveFileScope)
	if err != nil {
		return fmt.Errorf("parse credentials %q: %w", cloudCfg.CredentialsFile, err)
	}

	service, err := drive.NewService(ctx, option.WithHTTPClient(jwtConfig.Client(ctx)))
	if err != nil {
		return fmt.Errorf("create drive service: %w", err)
	}

	file, err := os.Open(filename)
	if err != nil {
		return fmt.Errorf("open backup file %q: %w", filename, err)
	}
	defer file.Close()

	fileMetadata := &drive.File{Name: filepath.Base(filename)}
	driveFile, err := service.Files.Create(fileMetadata).
		Media(file).
		ProgressUpdater(func(now, size int64) {
			logger.Info("drive upload progress", "file", filename, "uploadedBytes", now, "totalBytes", size)
		}).
		Do()
	if err != nil {
		return fmt.Errorf("upload file to drive: %w", err)
	}

	logger.Info("drive upload successful", "file", filename, "driveFileID", driveFile.Id)
	return nil
}
