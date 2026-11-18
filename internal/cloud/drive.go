package cloud

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/oauth2"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

// Operations against the Drive API run under a bounded whole-operation deadline
// so a stalled provider (or an operator-misconfigured proxy) can never wedge a
// backup job forever. Uploads get a generous window for large bundles over slow
// connections.
const driveUploadTimeout = 30 * time.Minute

// NewServiceFromToken builds a Drive API client authenticated with an OAuth
// access token. Access tokens are short-lived and are minted on demand from the
// connected account's stored refresh token (see the server drive layer).
func NewServiceFromToken(ctx context.Context, accessToken string) (*drive.Service, error) {
	if accessToken == "" {
		return nil, fmt.Errorf("drive access token is empty")
	}
	client := oauth2.NewClient(ctx, oauth2.StaticTokenSource(&oauth2.Token{
		AccessToken: accessToken,
		TokenType:   "Bearer",
	}))
	service, err := drive.NewService(ctx, option.WithHTTPClient(client))
	if err != nil {
		return nil, fmt.Errorf("create drive service: %w", err)
	}
	return service, nil
}

// UploadFile uploads filename to Google Drive inside parentID (the GitSafe
// storage folder) and returns the created file ID. Real byte progress is
// reported through onProgress (nil is allowed).
func UploadFile(ctx context.Context, service *drive.Service, filename, parentID string, logger *slog.Logger, onProgress func(now, total int64)) (string, error) {
	return UploadOrReplaceFile(ctx, service, filename, parentID, "", logger, onProgress)
}

// UploadOrReplaceFile stores filename in Drive. When existingID names a live
// (not trashed) file GitSafe can still see, its content is replaced in place
// so re-backups do not pile up copies; otherwise (no previous file, the user
// deleted it, or it is in the trash) a new file is created in parentID. On a
// failed replace the previous Drive file is left untouched.
func UploadOrReplaceFile(ctx context.Context, service *drive.Service, filename, parentID, existingID string, logger *slog.Logger, onProgress func(now, total int64)) (string, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if service == nil {
		return "", fmt.Errorf("drive service is nil")
	}

	ctx, cancel := context.WithTimeout(ctx, driveUploadTimeout)
	defer cancel()

	updater := func(now, size int64) {
		if onProgress != nil {
			onProgress(now, size)
		}
		logger.Debug("drive upload progress", "file", filepath.Base(filename), "uploadedBytes", now, "totalBytes", size)
	}

	if existingID != "" {
		replace, err := replaceable(ctx, service, existingID)
		if err != nil {
			return "", err
		}
		if replace {
			file, err := os.Open(filename)
			if err != nil {
				return "", fmt.Errorf("open backup file %q: %w", filename, err)
			}
			defer file.Close()
			updated, err := service.Files.Update(existingID, &drive.File{Name: filepath.Base(filename)}).
				Media(file).
				ProgressUpdater(updater).
				Fields("id").
				Context(ctx).
				Do()
			if err == nil {
				logger.Info("drive backup replaced", "file", filepath.Base(filename), "driveFileID", updated.Id)
				return updated.Id, nil
			}
			if !IsNotFound(err) {
				return "", fmt.Errorf("replace drive file: %w", err)
			}
			// Deleted between the check and the update: fall through and create.
			if _, serr := file.Seek(0, 0); serr != nil {
				return "", fmt.Errorf("rewind backup file: %w", serr)
			}
		}
		logger.Info("previous drive backup missing or trashed; creating a new file", "previousDriveFileID", existingID)
	}

	file, err := os.Open(filename)
	if err != nil {
		return "", fmt.Errorf("open backup file %q: %w", filename, err)
	}
	defer file.Close()

	fileMetadata := &drive.File{Name: filepath.Base(filename)}
	if parentID != "" {
		fileMetadata.Parents = []string{parentID}
	}
	driveFile, err := service.Files.Create(fileMetadata).
		Media(file).
		ProgressUpdater(updater).
		Fields("id").
		Context(ctx).
		Do()
	if err != nil {
		return "", fmt.Errorf("upload file to drive: %w", err)
	}

	logger.Info("drive upload successful", "file", filepath.Base(filename), "driveFileID", driveFile.Id)
	return driveFile.Id, nil
}

// replaceable reports whether existingID is a live file that can be updated in
// place. A missing (404) or trashed file is not replaceable.
func replaceable(ctx context.Context, service *drive.Service, existingID string) (bool, error) {
	f, err := service.Files.Get(existingID).Fields("id,trashed").Context(ctx).Do()
	if err != nil {
		if IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("look up previous drive backup: %w", err)
	}
	return !f.Trashed, nil
}

// IsNotFound reports whether err is a Drive API 404.
func IsNotFound(err error) bool {
	var gerr *googleapi.Error
	return errors.As(err, &gerr) && gerr.Code == http.StatusNotFound
}

// folderName is the Drive folder GitSafe creates in the connected account to
// hold every backup bundle.
const folderName = "GitSafe"

// EnsureGitSafeFolder returns the ID of the GitSafe storage folder in the
// connected account's Drive, creating it (in the Drive root) on first use. The
// folder is where every backup bundle is uploaded.
func EnsureGitSafeFolder(ctx context.Context, service *drive.Service) (string, error) {
	if service == nil {
		return "", fmt.Errorf("drive service is nil")
	}
	// Find an existing folder first; name collisions are resolved by picking the
	// newest match. There is no folder if the account was never connected here.
	list, err := service.Files.List().
		Q("name='" + folderName + "' and mimeType='application/vnd.google-apps.folder' and trashed=false").
		Fields("files(id,createdTime)").
		OrderBy("createdTime desc").
		PageSize(1).
		Context(ctx).
		Do()
	if err != nil {
		return "", fmt.Errorf("find git safe folder: %w", err)
	}
	if len(list.Files) > 0 {
		return list.Files[0].Id, nil
	}

	created, err := service.Files.Create(&drive.File{
		Name:     folderName,
		MimeType: "application/vnd.google-apps.folder",
	}).Context(ctx).Do()
	if err != nil {
		return "", fmt.Errorf("create git safe folder: %w", err)
	}
	return created.Id, nil
}
