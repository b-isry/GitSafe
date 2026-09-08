package server

import (
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// folderEntry is a single navigation target. Only directories are ever listed;
// file names and contents are never exposed.
type folderEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type foldersResponse struct {
	Path     string        `json:"path"`
	Name     string        `json:"name"`
	Parent   string        `json:"parent"`
	IsTop    bool          `json:"isTop"`
	Entries  []folderEntry `json:"entries"`
	ErrorMsg string        `json:"error,omitempty"`
}

// handleListFolders is the server-side directory browser. It returns the
// directories inside a given folder so the UI can present the classic
// "navigate into a folder, select it" flow without the browser ever reading
// arbitrary local paths. It is restricted to loopback clients by the Routes
// wiring and only ever lists directory names.
func (s *Server) handleListFolders(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimSpace(r.URL.Query().Get("path"))
	res := foldersResponse{Path: path, Name: baseName(path), Parent: parentOf(path)}

	if path == "" {
		// Top level: offer the available drives on Windows, "/" elsewhere.
		switch runtime.GOOS {
		case "windows":
			res.IsTop = true
			res.Entries = drives()
		default:
			res.IsTop = true
			res.Parent = ""
			res.Entries = []folderEntry{{Name: "/", Path: "/"}}
		}
		writeJSON(w, http.StatusOK, res)
		return
	}

	entries, err := os.ReadDir(path)
	if err != nil {
		res.ErrorMsg = "That folder cannot be read. It may be locked or restricted."
		writeJSON(w, http.StatusOK, res)
		return
	}

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		res.Entries = append(res.Entries, folderEntry{
			Name: e.Name(),
			Path: filepath.Join(path, e.Name()),
		})
	}
	sort.Slice(res.Entries, func(i, j int) bool {
		return strings.ToLower(res.Entries[i].Name) < strings.ToLower(res.Entries[j].Name)
	})
	writeJSON(w, http.StatusOK, res)
}

// baseName is the human-readable last segment of a path, or the path itself
// (e.g. "C:") when there is no parent.
func baseName(p string) string {
	if p == "" {
		return ""
	}
	clean := filepath.Clean(p)
	base := filepath.Base(clean)
	if base == "." || base == string(filepath.Separator) || base == clean {
		return clean
	}
	return base
}

// parentOf returns the parent directory of p, or "" at a drive / filesystem
// root so the UI can fall back to the top-level drive listing.
func parentOf(p string) string {
	if p == "" {
		return ""
	}
	parent := filepath.Dir(filepath.Clean(p))
	if parent == filepath.Clean(p) {
		return ""
	}
	return parent
}

// drives enumerates the mounted Windows drives by probing each letter.
func drives() []folderEntry {
	var out []folderEntry
	for c := 'A'; c <= 'Z'; c++ {
		root := string(c) + ":\\"
		if _, err := os.Stat(root); err == nil {
			out = append(out, folderEntry{Name: string(c) + ":", Path: root})
		}
	}
	return out
}
