package cloud

import (
	"context"
	"fmt"

	// "io"
	"os"
	"path/filepath"

	"golang.org/x/oauth2/google"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/option"
)

func UploadToDrive(filename string) error {
	ctx := context.Background()

	credentials, err := os.ReadFile("credentials.json")
	if err != nil {
		return fmt.Errorf("failed to read credentials: %w", err)
	}

	config, err := google.JWTConfigFromJSON(credentials, drive.DriveFileScope)
	if err != nil {
		return fmt.Errorf("failed to create config from credentials: %w", err)
	}

	service, err := drive.NewService(ctx, option.WithHTTPClient(config.Client(ctx)))
	if err != nil {
		return fmt.Errorf("failed to create drive service: %w", err)
	}

	file, err := os.Open(filename)
	if err != nil {
		return fmt.Errorf("failed to open the file: %w", err)
	}
	defer file.Close()

	fileMetaData := &drive.File{
		Name: filepath.Base(filename),
	}

	driveFile, err := service.Files.Create(fileMetaData).
		Media(file).
		ProgressUpdater(func(now, size int64) {
			fmt.Printf("Uploaded %d of %d bytes\n", now, size)
		}).Do()
	if err != nil {
		return fmt.Errorf("failed to upload file to drive: %w", err)
	}

	fmt.Printf("file uploaded successfully. File ID: %s\n", driveFile.Id)
	return nil
}
