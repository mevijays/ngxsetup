package webui

import (
	"net/http"
	"strconv"

	"ngxsetup/internal/system"
)

func (s *Server) handleLogSources(w http.ResponseWriter, r *http.Request) {
	c, err := newCtx(r.Context(), false)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sources": logSources(c)})
}

const defaultTailLines = 200
const maxTailLines = 2000

// handleLogTail serves both the "snapshot" (last N lines) and "live" (new
// lines since a byte offset) views the frontend's log viewer offers.
// Deliberately not a WebSocket or SSE stream: the frontend already polls
// /api/stats on a timer for the same reason, and a second polling endpoint
// here costs nothing extra on the server between polls, unlike a held-open
// connection per viewer — which is the concrete way this stays "light" per
// the request that asked for it.
//
// Query params: source (required), mode ("snapshot", default, or "live"),
// lines (snapshot line count, default 200), offset (live mode's resume
// point, from a previous response's "offset").
func (s *Server) handleLogTail(w http.ResponseWriter, r *http.Request) {
	c, err := newCtx(r.Context(), false)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	key := r.URL.Query().Get("source")
	src, ok := findLogSource(c, key)
	if !ok {
		writeJSONError(w, http.StatusNotFound, "unknown log source")
		return
	}

	lines := defaultTailLines
	if v := r.URL.Query().Get("lines"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			lines = n
		}
	}
	if lines > maxTailLines {
		lines = maxTailLines
	}

	// journald-backed sources (the database's own error log, when the distro
	// package logs there instead of to a file — confirmed live: Ubuntu's
	// mariadb-server does exactly this) have no byte-offset concept to
	// resume from. Every poll re-reads the last N entries and the frontend
	// replaces its view wholesale for these rather than appending.
	if src.Kind == "journal" {
		if !src.Exists {
			writeJSONError(w, http.StatusNotFound, "log source is not available")
			return
		}
		runner := system.Runner{}
		out, err := runner.Output(r.Context(), "journalctl", "-u", src.Unit, "-n", strconv.Itoa(lines), "--no-pager", "--output=cat")
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "reading journal: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"source": key, "kind": "journal", "replace": true, "lines": splitNonEmpty(out), "offset": 0,
		})
		return
	}

	if !src.Exists {
		writeJSONError(w, http.StatusNotFound, "log file does not exist yet")
		return
	}

	mode := r.URL.Query().Get("mode")
	if mode == "live" {
		offset, _ := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
		newLines, newOffset, err := tailFrom(src.Path, offset)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "reading log: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"source": key, "kind": "file", "replace": false, "lines": newLines, "offset": newOffset,
		})
		return
	}

	tail, size, err := snapshotTail(src.Path, lines)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "reading log: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"source": key, "kind": "file", "replace": true, "lines": tail, "offset": size,
	})
}

func splitNonEmpty(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}
