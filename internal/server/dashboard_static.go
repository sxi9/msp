package server

import (
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/sxi9/msp/web"
)

// handleDownload is GET /dl/{file...}: one file from cfg.AgentDir (agent
// binaries, SHA256SUMS, install scripts, the APK). No directory listing, no
// subdirectories, no traversal; 404 when AgentDir is unset or the file is
// missing.
func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	if s.cfg.AgentDir == "" || name == "" || strings.Contains(name, "/") || strings.Contains(name, "\\") ||
		strings.Contains(name, "..") || strings.HasPrefix(name, ".") {
		writeError(w, http.StatusNotFound, "not_found", "")
		return
	}
	f, err := os.Open(filepath.Join(s.cfg.AgentDir, name))
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "")
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		writeError(w, http.StatusNotFound, "not_found", "")
		return
	}
	if ct := dashContentType(name); ct != "" {
		w.Header().Set("Content-Type", ct)
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, name, info.ModTime(), f)
}

// handleSPA is the catch-all: paths under /api or /dl that reached it are
// unknown routes (404 JSON); everything else is served from the embedded
// web/dist with index.html as the fallback so hash and deep links both load
// the dashboard.
func (s *Server) handleSPA(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	if strings.HasPrefix(p, "/api/") || p == "/api" || strings.HasPrefix(p, "/dl/") || p == "/dl" {
		writeError(w, http.StatusNotFound, "not_found", "")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "")
		return
	}
	dist, err := fs.Sub(web.FS, "dist")
	if err != nil {
		s.log.Error("web dist", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "")
		return
	}
	name := strings.TrimPrefix(path.Clean("/"+p), "/")
	if name == "" || name == "." {
		name = "index.html"
	}
	body, err := fs.ReadFile(dist, name)
	if err != nil {
		// Not a bundled asset (or a directory): a dashboard route, so the
		// shell loads and the client-side router takes over.
		name = "index.html"
		body, err = fs.ReadFile(dist, name)
		if err != nil {
			s.log.Error("read index.html", "err", err)
			writeError(w, http.StatusInternalServerError, "internal", "")
			return
		}
	}
	ct := dashContentType(name)
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		w.Write(body)
	}
}

// dashContentType returns the Content-Type for a served file name, with
// fixed values for the dashboard's own asset types.
func dashContentType(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".html":
		return "text/html; charset=utf-8"
	case ".js", ".mjs":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".json", ".map":
		return "application/json; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".ico":
		return "image/x-icon"
	case ".apk":
		return "application/vnd.android.package-archive"
	case ".sh", ".ps1", ".txt":
		return "text/plain; charset=utf-8"
	case ".exe":
		return "application/vnd.microsoft.portable-executable"
	case "":
		if name == "SHA256SUMS" {
			return "text/plain; charset=utf-8"
		}
		return ""
	}
	return mime.TypeByExtension(path.Ext(name))
}
