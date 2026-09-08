package server

import (
	"net/http"
	"os"
	"strings"

	"github.com/b-isry/gitsafe/internal/config"
)

// SettingsView is the mutable configuration surface shown by the Settings page.
// It exposes only the fields the user can edit and never carries raw credential
// contents — the credentials file is represented by its path and an existence
// status only.
type SettingsView struct {
	RootPath         string
	RootName         string
	Days             int
	OutputPath       string
	BackupHistory    bool
	CloudEnabled     bool
	CredentialsFile  string
	CredentialsFound bool
	Retention        config.RetentionConfig
	Scanning         bool
}

// settingsView builds the view model from the current app configuration,
// resolving whether the configured credentials file is actually present.
func settingsView(cfg config.Config, output string, scanning bool) SettingsView {
	return SettingsView{
		RootPath:         cfg.RootPath,
		RootName:         baseName(cfg.RootPath),
		Days:             cfg.Days,
		OutputPath:       cfg.OutputPath,
		BackupHistory:    cfg.BackupHistory,
		CloudEnabled:     cfg.Cloud.Enabled,
		CredentialsFile:  cfg.Cloud.CredentialsFile,
		CredentialsFound: credentialsFileExists(cfg.Cloud.CredentialsFile),
		Retention:        cfg.Retention,
		Scanning:         scanning,
	}
}

func credentialsFileExists(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	if _, err := os.Stat(os.ExpandEnv(path)); err != nil {
		return false
	}
	return true
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	scanning, _, _ := s.app.ScanStatus()
	s.render(w, "settings", renderData{
		pageData: pageData{Active: "settings", PageTitle: "Settings"},
		Settings: settingsView(s.app.Config, s.app.OutputPath, scanning),
	})
}

// settingsInput is the payload accepted from the settings form. Field names map
// 1:1 onto the existing config model.
type settingsInput struct {
	RootPath        string `json:"rootPath"`
	Days            int    `json:"days"`
	OutputPath      string `json:"outputPath"`
	BackupHistory   bool   `json:"backupHistory"`
	CloudEnabled    bool   `json:"cloudEnabled"`
	CredentialsFile string `json:"credentialsFile"`

	KeepLocal      int  `json:"keepLocal"`
	KeepLocalDays  int  `json:"keepLocalDays"`
	KeepJobs       int  `json:"keepJobs"`
	KeepDriveDays  int  `json:"keepDriveDays"`
	DriveRetention bool `json:"driveRetentionEnabled"`
}

type settingsResponse struct {
	OK       bool              `json:"ok"`
	Message  string            `json:"message,omitempty"`
	Errors   map[string]string `json:"errors,omitempty"`
	Settings *SettingsView     `json:"settings,omitempty"`
	Scanning bool              `json:"scanning"`
}

func (s *Server) handleSaveSettings(w http.ResponseWriter, r *http.Request) {
	var in settingsInput
	if err := decodeJSONBody(w, r, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, settingsResponse{
			OK:      false,
			Message: "Invalid request. Please check the form and try again.",
		})
		return
	}

	in.RootPath = strings.TrimSpace(in.RootPath)
	in.OutputPath = strings.TrimSpace(in.OutputPath)
	in.CredentialsFile = strings.TrimSpace(in.CredentialsFile)

	errs := validateSettings(in)

	if len(errs) > 0 {
		writeJSON(w, http.StatusUnprocessableEntity, settingsResponse{
			OK:      false,
			Message: "Please fix the highlighted fields.",
			Errors:  errs,
		})
		return
	}

	// Build the next config from the current app config, overriding only the
	// editable settings so every other field (and the synthesized sources) is
	// preserved.
	next := s.app.Config
	next.RootPath = in.RootPath
	next.Days = in.Days
	next.OutputPath = in.OutputPath
	next.BackupHistory = in.BackupHistory
	next.Cloud.Enabled = in.CloudEnabled
	next.Cloud.CredentialsFile = in.CredentialsFile
	next.Retention = config.RetentionConfig{
		KeepLocal:      in.KeepLocal,
		KeepLocalDays:  in.KeepLocalDays,
		KeepJobs:       in.KeepJobs,
		KeepDriveDays:  in.KeepDriveDays,
		DriveRetention: in.DriveRetention,
	}

	// Mirror config.Load's normalization: derive sources from the root path so
	// validation holds regardless of whether the in-memory config was built by
	// Load (which normally synthesizes sources) or directly.
	if len(next.Sources) == 0 {
		next.Sources = next.EffectiveSources()
	}

	if err := next.Validate(); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, settingsResponse{
			OK:      false,
			Message: err.Error(),
		})
		return
	}

	if err := next.Save(s.configPath); err != nil {
		s.logger.Error("save config", "path", s.configPath, "error", err)
		writeJSON(w, http.StatusInternalServerError, settingsResponse{
			OK:      false,
			Message: "Could not write the configuration file. Check file permissions and try again.",
		})
		return
	}

	// Reload from disk so the running app reflects exactly what was persisted
	// (re-running environment expansion), then apply it to the live app.
	loaded, err := config.Load(s.configPath)
	if err != nil {
		s.logger.Error("reload config after save", "path", s.configPath, "error", err)
		writeJSON(w, http.StatusInternalServerError, settingsResponse{
			OK:      false,
			Message: "The configuration was saved but could not be re-read.",
		})
		return
	}
	s.app.ApplySettings(loaded)

	if err := os.MkdirAll(loaded.OutputPath, 0o755); err != nil {
		s.logger.Warn("create output directory", "path", loaded.OutputPath, "error", err)
	}

	// ApplySettings already started a detached background re-scan; report its
	// state so the UI can show "Scanning repositories…" without the save
	// request ever waiting on the scan itself.
	scanning, _, _ := s.app.ScanStatus()
	view := settingsView(loaded, loaded.OutputPath, scanning)
	writeJSON(w, http.StatusOK, settingsResponse{
		OK:       true,
		Message:  "Settings saved.",
		Settings: &view,
		Scanning: scanning,
	})
}

// validateSettings returns a map of field -> human-readable problem. Keys match
// the form field names so the UI can highlight the offending field.
func validateSettings(in settingsInput) map[string]string {
	errs := map[string]string{}

	if in.RootPath == "" {
		errs["rootPath"] = "Root path is required."
	} else if fi, err := os.Stat(in.RootPath); err != nil || !fi.IsDir() {
		errs["rootPath"] = "Root path must be an existing directory."
	}

	if in.Days < 0 {
		errs["days"] = "Stale threshold must be 0 or greater."
	}

	if in.OutputPath == "" {
		errs["outputPath"] = "Output path is required."
	}

	if in.CloudEnabled && strings.TrimSpace(in.CredentialsFile) == "" {
		errs["credentialsFile"] = "A credentials file is required when Google Drive is enabled."
		errs["cloudEnabled"] = "Either provide a credentials file or disable Google Drive."
	}

	r := config.RetentionConfig{
		KeepLocal:      in.KeepLocal,
		KeepLocalDays:  in.KeepLocalDays,
		KeepJobs:       in.KeepJobs,
		KeepDriveDays:  in.KeepDriveDays,
		DriveRetention: in.DriveRetention,
	}
	if err := r.Validate(); err != nil {
		errs["retention"] = err.Error()
	}

	return errs
}
