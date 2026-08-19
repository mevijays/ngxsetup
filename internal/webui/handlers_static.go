package webui

import (
	"net/http"
	"path/filepath"
	"strings"
)

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(indexHTML)
}

func (s *Server) handleAsset(name, contentType string) http.HandlerFunc {
	body, err := staticFS.ReadFile("static/" + name)
	return func(w http.ResponseWriter, r *http.Request) {
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", contentType)
		// These never change without a binary rebuild — the browser is
		// welcome to cache them for the life of the tab.
		w.Header().Set("Cache-Control", "public, max-age=3600")
		w.Write(body)
	}
}

// handleWebfont serves Font Awesome's font files. A dedicated handler rather
// than one more handleAsset registration per file because the set of font
// files is fixed but their names aren't worth hard-coding one route each
// for; {file} is validated against the embedded directory listing itself,
// so this can never be tricked into reading anything outside static/vendor.
func (s *Server) handleWebfont(w http.ResponseWriter, r *http.Request) {
	file := r.PathValue("file")
	if strings.Contains(file, "/") || strings.Contains(file, "..") {
		http.NotFound(w, r)
		return
	}
	body, err := staticFS.ReadFile("static/vendor/webfonts/" + file)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	switch filepath.Ext(file) {
	case ".woff2":
		w.Header().Set("Content-Type", "font/woff2")
	case ".woff":
		w.Header().Set("Content-Type", "font/woff")
	case ".ttf":
		w.Header().Set("Content-Type", "font/ttf")
	default:
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Write(body)
}
