package cloud

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/b-isry/gitsafe/internal/config"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/option"
)

// Operations against the Drive API run under a bounded whole-operation
// deadline so a stalled provider (or an operator-misconfigured proxy) can never
// wedge a backup job or cleanup run forever. Uploads get a generous window for
// large bundles over slow connections; deletions are quick single requests.
const (
	driveUploadTimeout = 30 * time.Minute
	driveDeleteTimeout = 2 * time.Minute
)

// UploadToDrive uploads filename to Google Drive, reporting real byte progress
// through onProgress. It preserves the previous behavior when onProgress is nil.
func UploadToDrive(ctx context.Context, filename string, cloudCfg config.CloudConfig, logger *slog.Logger, onProgress func(now, total int64)) error {
	_, err := UploadFile(ctx, cloudCfg, filename, logger, onProgress)
	return err
}

// UploadFile uploads filename to Google Drive and returns the created file ID.
// Real byte progress is reported through onProgress (nil is allowed).
func UploadFile(ctx context.Context, cloudCfg config.CloudConfig, filename string, logger *slog.Logger, onProgress func(now, total int64)) (string, error) {
	if logger == nil {
		logger = slog.Default()
	}

	ctx, cancel := context.WithTimeout(ctx, driveUploadTimeout)
	defer cancel()

	credentials, err := os.ReadFile(cloudCfg.CredentialsFile)
	if err != nil {
		return "", fmt.Errorf("read credentials %q: %w", cloudCfg.CredentialsFile, err)
	}

	jwtConfig, err := google.JWTConfigFromJSON(credentials, drive.DriveFileScope)
	if err != nil {
		return "", fmt.Errorf("parse credentials %q: %w", cloudCfg.CredentialsFile, err)
	}

	service, err := drive.NewService(ctx, option.WithHTTPClient(jwtConfig.Client(ctx)))
	if err != nil {
		return "", fmt.Errorf("create drive service: %w", err)
	}

	file, err := os.Open(filename)
	if err != nil {
		return "", fmt.Errorf("open backup file %q: %w", filename, err)
	}
	defer file.Close()

	fileMetadata := &drive.File{Name: filepath.Base(filename)}
	updater := func(now, size int64) {
		if onProgress != nil {
			onProgress(now, size)
		}
		logger.Info("drive upload progress", "file", filename, "uploadedBytes", now, "totalBytes", size)
	}
	driveFile, err := service.Files.Create(fileMetadata).
		Media(file).
		ProgressUpdater(updater).
		Do()
	if err != nil {
		return "", fmt.Errorf("upload file to drive: %w", err)
	}

	logger.Info("drive upload successful", "file", filename, "driveFileID", driveFile.Id)
	return driveFile.Id, nil
}

// driveFileIDValid rejects clearly non-Drive identifiers (things GitSafe would
// never have recorded) so a corrupt entry can never be turned into a delete
// request. Real Drive file IDs are opaque base64url strings of ~33 characters.
var driveFileIDValid = regexp.MustCompile(`^[A-Za-z0-9_-]{10,100}$`)

// DeleteFile permanently deletes a Drive file by ID using the service-account
// credentials. It returns nil only when Drive confirms the deletion. This is
// used by retention cleanup; callers must only pass IDs GitSafe itself created
// (i.e. those recorded in BackupRecord.DriveFileID) so only its own backups are
// ever targeted.
func DeleteFile(ctx context.Context, cloudCfg config.CloudConfig, fileID string, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	if !driveFileIDValid.MatchString(fileID) {
		return fmt.Errorf("refusing drive delete for invalid file id %q", fileID)
	}

	ctx, cancel := context.WithTimeout(ctx, driveDeleteTimeout)
	defer cancel()

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

	if err := service.Files.Delete(fileID).Do(); err != nil {
		return fmt.Errorf("delete drive file %q: %w", fileID, err)
	}

	logger.Info("drive delete successful", "driveFileID", fileID)
	return nil
}
