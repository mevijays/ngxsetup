package webui

import (
	"net/http"
)

type migrateDiscoverRequest struct {
	Host       string `json:"host"`
	Port       int    `json:"port"`
	User       string `json:"user"`
	PrivateKey string `json:"private_key"`
}

// handleMigrateDiscover connects to a remote host and lists what it could
// migrate — no site is touched on either end by this call, it is read-only
// discovery. Mutating (needs the same X-Requested-With guard as everything
// else that isn't a plain GET) because it does write the operator's
// private key to a temp file on this machine, however briefly, and because
// a page silently probing arbitrary hosts on the operator's behalf is
// exactly the class of thing that guard exists to prevent.
func (s *Server) handleMigrateDiscover(w http.ResponseWriter, r *http.Request) {
	var req migrateDiscoverRequest
	if err := readJSON(r, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	sites, err := s.migrate.Discover(r.Context(), DiscoverRequest{
		Host: req.Host, Port: req.Port, User: req.User, PrivateKey: req.PrivateKey,
	})
	if err != nil {
		writeJSONError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sites": sites})
}

type migrateStartRequest struct {
	Domains    []string `json:"domains"`
	TLS        bool     `json:"tls"`
	SelfSigned bool     `json:"self_signed"`
}

// handleMigrateStart begins migrating the selected domains in the
// background and returns immediately — the migration itself can run for
// hours on a large site, far longer than any one HTTP request should be
// held open for. The frontend polls handleMigrateStatus for progress.
func (s *Server) handleMigrateStart(w http.ResponseWriter, r *http.Request) {
	var req migrateStartRequest
	if err := readJSON(r, &req); err != nil || len(req.Domains) == 0 {
		writeJSONError(w, http.StatusBadRequest, "select at least one domain to migrate")
		return
	}
	if err := s.migrate.StartMigration(req.Domains, MigrateOptions{TLS: req.TLS, SelfSigned: req.SelfSigned}); err != nil {
		writeJSONError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleMigrateStatus serves the running (or most recently finished) job's
// progress — every selected site's state/step/percent, plus the combined
// log, polled by the frontend every couple of seconds the same way
// /api/stats and /api/logs/tail already are.
func (s *Server) handleMigrateStatus(w http.ResponseWriter, r *http.Request) {
	status := s.migrate.Status()
	if status == nil {
		writeJSON(w, http.StatusOK, map[string]any{"running": false, "sites": []any{}, "log": []any{}})
		return
	}
	writeJSON(w, http.StatusOK, status)
}

// handleMigrateCancel stops the running job. Whatever site was mid-transfer
// is rolled back the same way a failed site always is — never left half
// registered.
func (s *Server) handleMigrateCancel(w http.ResponseWriter, r *http.Request) {
	s.migrate.Cancel()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
