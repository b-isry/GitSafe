package server

import (
	"net/http"
	"os"
	"strings"

	"github.com/b-isry/gitsafe/internal/config"
)

// SetupView is the data rendered by the standalone first-run page.
type SetupView struct {
	Path string
}

func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	s.renderStandalone(w, "setup", SetupView{})
}

// handleSetupSubmit persists the chosen root folder and returns success
// immediately. Repository discovery is kicked off in the background (via
// ApplySettings) so the request never blocks on a scan; the dashboard the user
// lands on shows scan progress until the repositories are ready.
func (s *Server) handleSetupSubmit(w http.ResponseWriter, r *http.Request) {
	var in struct {
		RootPath string `json:"rootPath"`
	}
	if err := decodeJSONBody(w, r, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "Invalid request. Please try again."})
		return
	}
	in.RootPath = strings.TrimSpace(in.RootPath)

	if in.RootPath == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "error": "Choose a folder before continuing."})
		return
	}
	fi, err := os.Stat(in.RootPath)
	if err != nil || !fi.IsDir() {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "error": "That folder does not exist or is not accessible."})
		return
	}

	// Persist the chosen root and re-apply the freshly loaded config so the
	// running app reflects exactly what was written to disk.
	next := s.app.Config
	next.RootPath = in.RootPath
	if len(next.Sources) == 0 {
		next.Sources = next.EffectiveSources()
	}
	if err := next.Validate(); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if err := next.Save(s.configPath); err != nil {
		s.logger.Error("save first-run config", "path", s.configPath, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "Could not save the configuration file. Check permissions and try again."})
		return
	}
	loaded, err := config.Load(s.configPath)
	if err != nil {
		s.logger.Error("reload first-run config", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "The directory was saved but could not be re-read."})
		return
	}
	// ApplySettings clears the scan cache and starts a detached background
	// scan, so the response below is delivered without waiting for it.
	s.app.ApplySettings(loaded)

	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "redirect": "/"})
}
