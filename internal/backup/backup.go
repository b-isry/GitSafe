// Package backup drives a single repository backup: create a full-history git
// bundle via the archiver and optionally upload it to Google Drive, reporting
// real state and progress as it runs.
package backup

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/b-isry/gitsafe/internal/archiver"
	"github.com/b-isry/gitsafe/internal/cloud"
	"github.com/b-isry/gitsafe/internal/config"
)

// State is the lifecycle of a backup run.
type State string

const (
	StateIdle     State = "idle"
	StateCreating State = "creating"
	StateUpload   State = "uploading"
	StateDone     State = "completed"
	StateFailed   State = "failed"
)

// Result is the outcome of a backup run. Metadata is only populated once the
// bundle has actually been created on disk.
type Result struct {
	State         State       `json:"state"`
	RepoPath      string      `json:"repoPath"`
	OutputPath    string      `json:"outputPath"`
	BundlePath    string      `json:"bundlePath"`
	BundleName    string      `json:"bundleName"`
	BundleSize    int64       `json:"bundleSize"`
	Cloud         CloudResult `json:"cloud"`
	Error         string      `json:"error,omitempty"`
	StartedAt     time.Time   `json:"startedAt"`
	FinishedAt    time.Time   `json:"finishedAt,omitempty"`
	BackupHistory bool        `json:"backupHistory"`
}

type CloudResult struct {
	Enabled     bool   `json:"enabled"`
	Status      string `json:"status"` // none | uploading | uploaded | failed
	BytesDone   int64  `json:"bytesDone"`
	BytesTotal  int64  `json:"bytesTotal"`
	DriveFileID string `json:"driveFileId,omitempty"`
	Error       string `json:"error,omitempty"`
}

type OnProgress func(now, total int64)

type Runner interface {
	Run(ctx context.Context, req Request, onProgress OnProgress) Result
}

type Request struct {
	RepoPath      string
	OutputPath    string
	BackupHistory bool
	Cloud         config.CloudConfig
}

type runner struct {
	logger *slog.Logger
}

func New(logger *slog.Logger) Runner {
	if logger == nil {
		logger = slog.Default()
	}
	return &runner{logger: logger}
}

func (r *runner) Run(ctx context.Context, req Request, onProgress OnProgress) Result {
	res := Result{
		State:         StateCreating,
		RepoPath:      req.RepoPath,
		OutputPath:    req.OutputPath,
		StartedAt:     time.Now(),
		BackupHistory: req.BackupHistory,
		Cloud:         CloudResult{Enabled: req.Cloud.Enabled, Status: "none"},
	}

	bundlePath, err := archiver.BundleRepo(req.RepoPath, req.OutputPath, req.BackupHistory, r.logger)
	if err != nil {
		r.fail(&res, "create bundle", err)
		return res
	}
	res.BundlePath = bundlePath
	res.BundleName = base(bundlePath)
	if info, err := os.Stat(bundlePath); err == nil {
		res.BundleSize = info.Size()
	}

	if !req.Cloud.Enabled {
		res.FinishedAt = time.Now()
		res.State = StateDone
		return res
	}

	return r.upload(ctx, req, res, onProgress)
}

func (r *runner) upload(ctx context.Context, req Request, res Result, onProgress OnProgress) Result {
	res.State = StateUpload
	res.Cloud.Status = "uploading"

	report := func(now, total int64) {
		res.Cloud.BytesDone = now
		res.Cloud.BytesTotal = total
		if onProgress != nil {
			onProgress(now, total)
		}
	}

	driveFile, err := cloud.UploadFile(ctx, req.Cloud, res.BundlePath, r.logger, report)
	if err != nil {
		res.Cloud.Status = "failed"
		res.Cloud.Error = err.Error()
		r.fail(&res, "upload to drive", err)
		return res
	}

	res.Cloud.Status = "uploaded"
	res.Cloud.DriveFileID = driveFile
	res.FinishedAt = time.Now()
	res.State = StateDone
	return res
}

func (r *runner) fail(res *Result, stage string, err error) {
	res.State = StateFailed
	res.Error = stage + ": " + err.Error()
	r.logger.Error("backup failed", "stage", stage, "error", err)
}

func base(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' || p[i] == '\\' {
			return p[i+1:]
		}
	}
	return p
}
