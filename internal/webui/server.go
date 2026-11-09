// Package webui serves a browser-based control panel for ngxsetup: the same
// operations the CLI performs, reachable by anyone who can open a web page
// rather than only whoever has SSH access to the box.
//
// It deliberately does not reimplement any provisioning logic. Every handler
// either reads directly from a fresh provision.Ctx (status, site list,
// facts) or calls the exact same exported method the matching CLI command
// calls (AddSite, RemoveSite, ApplySecurity...) — the web UI is a second
// front end on the existing engine, not a second engine.
//
// Access is by a per-run sign-in token printed in the operator's terminal
// (see auth.go): `ngxsetup web` is designed to be started in an operator's
// own active terminal session and to die with it — never installed as a
// systemd service, never left running unattended — and only someone who can
// see that terminal can sign in. It binds to every interface by default
// (DefaultBind) and prints a sign-in URL per address; pass --bind 127.0.0.1
// to keep it reachable only through an SSH tunnel.
package webui

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"ngxsetup/internal/logx"
	"ngxsetup/internal/stats"
	"ngxsetup/internal/system"
)

// Config configures the server.
type Config struct {
	Bind string // address to listen on, e.g. "0.0.0.0" or "127.0.0.1"
	Port int    // 0 means let the OS assign an ephemeral port
}

// Server holds everything that has to survive across requests: the stats
// sampler (which computes CPU% and request-rate as deltas against its
// previous call, so it has to be the same instance from one poll to the next
// rather than rebuilt per-request) and open log-tail cursors.
type Server struct {
	cfg      Config
	sampler  *stats.Sampler
	auditLog *os.File
	migrate  *MigrateManager

	// loginToken goes into the printed URL; session is the cookie value a
	// browser receives in exchange for it. Both are random per run.
	loginToken string
	session    string
}

// New builds a Server. It does not touch the network yet — call Serve for
// that.
func New(cfg Config) (*Server, error) {
	// A throwaway Ctx purely to get a DB client for the stats sampler, which
	// must be one long-lived instance for its CPU%/request-rate deltas to
	// mean anything across successive /api/stats polls — unlike every other
	// handler, which builds its own fresh Ctx per request the same way each
	// CLI invocation is its own process. The database being unreachable at
	// startup should not stop the server from starting: NewSampler(nil)
	// simply reports DB size as unavailable, the same graceful degradation
	// `top` already relies on.
	var sampler *stats.Sampler
	if c, err := newCtx(context.Background(), false); err == nil {
		sampler = stats.NewSampler(c.DBClient())
	} else {
		sampler = stats.NewSampler(nil)
	}

	token, err := randomSecret(32)
	if err != nil {
		return nil, err
	}
	session, err := randomSecret(32)
	if err != nil {
		return nil, err
	}
	return &Server{cfg: cfg, sampler: sampler, migrate: &MigrateManager{}, loginToken: token, session: session}, nil
}

// Serve starts the listener and blocks until ctx is cancelled — Ctrl+C, the
// terminal closing (SIGHUP), or the process being asked to stop, all of
// which the caller is expected to have wired into ctx. Returns the URL it
// ended up reachable at before blocking — one per interface address for a
// wildcard bind — via the urlReady channel, so the caller can print them once
// the OS has actually assigned the port.
func (s *Server) Serve(ctx context.Context, urlReady chan<- []ListenURL) error {
	cert, err := loadOrCreateCert(s.cfg.Bind)
	if err != nil {
		return fmt.Errorf("preparing TLS certificate: %w", err)
	}

	listenAddr := fmt.Sprintf("%s:%d", s.cfg.Bind, s.cfg.Port)
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", listenAddr, err)
	}
	tlsLn := tls.NewListener(ln, &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	})

	if f, err := openAuditLog(); err == nil {
		s.auditLog = f
		defer f.Close()
	} else {
		logx.Warn("web UI audit log could not be opened: %v", err)
	}

	port := ln.Addr().(*net.TCPAddr).Port

	// Bound to every interface but the firewall's default-deny policy still
	// blocks the port until something explicitly allows it — confirmed live:
	// the listener came up correctly (curl from the box itself worked) while
	// every request from off-box got no response at all, because ufw had no
	// rule for the port this command picked. A loopback-only bind has no
	// such problem and gets no rule.
	if !IsLoopbackBind(s.cfg.Bind) {
		openFirewallPort(ctx, port)
		defer closeFirewallPort(port)
	}

	if urlReady != nil {
		urlReady <- listenURLs(s.cfg.Bind, port, s.loginPath())
	}

	srv := &http.Server{
		Handler:           s.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		// Long enough for a large database restore upload or a long-held
		// live log tail; short enough that a stalled connection cannot pin
		// resources indefinitely.
		ReadTimeout:  30 * time.Minute,
		WriteTimeout: 30 * time.Minute,
	}

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(tlsLn) }()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}

// openFirewallPort allows this port through ufw, if ufw is installed. Not an
// error if ufw is absent or not managing the firewall — that just means
// nothing was blocking the port in the first place.
func openFirewallPort(ctx context.Context, port int) {
	r := system.Runner{}
	if !r.Look("ufw") {
		return
	}
	label := fmt.Sprintf("%d/tcp", port)
	if err := r.Run(ctx, "ufw", "allow", label, "comment", "ngxsetup web"); err != nil {
		logx.Warn("could not open port %d in the firewall: %v", port, err)
		logx.Warn("if this machine is reachable and the page still does not load, allow it manually: ufw allow %s", label)
		return
	}
	logx.Info("opened port %d in the firewall (ufw) for this session", port)
}

// closeFirewallPort reverses openFirewallPort on a clean shutdown, so a
// random port from a past session does not accumulate as a standing
// firewall exception forever. Uses its own background context: by the time
// this runs, the context Serve was called with is already cancelled — that
// is what triggered the shutdown in the first place.
func closeFirewallPort(port int) {
	r := system.Runner{}
	if !r.Look("ufw") {
		return
	}
	label := fmt.Sprintf("%d/tcp", port)
	if err := r.Run(context.Background(), "ufw", "delete", "allow", label); err != nil {
		logx.Debug("could not remove the firewall rule for port %d: %v", port, err)
	}
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /", s.handleIndex)
	mux.HandleFunc("GET /app.js", s.handleAsset("app.js", "text/javascript; charset=utf-8"))
	mux.HandleFunc("GET /app.css", s.handleAsset("app.css", "text/css; charset=utf-8"))
	mux.HandleFunc("GET /vendor/tailwind.css", s.handleAsset("vendor/tailwind.css", "text/css; charset=utf-8"))
	mux.HandleFunc("GET /vendor/chart.js", s.handleAsset("vendor/chart.js", "text/javascript; charset=utf-8"))
	mux.HandleFunc("GET /vendor/fontawesome.css", s.handleAsset("vendor/fontawesome.css", "text/css; charset=utf-8"))
	mux.HandleFunc("GET /vendor/webfonts/{file}", s.handleWebfont)

	// mut additionally requires the header only same-origin JS can set, for
	// anything that changes the machine — see requireFetchHeader.
	mut := s.requireFetchHeader

	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("GET /api/version", s.handleVersion)
	mux.HandleFunc("GET /api/facts", s.handleFacts)
	mux.HandleFunc("GET /api/doctor", s.handleDoctor)
	mux.HandleFunc("GET /api/stats", s.handleStats)
	mux.HandleFunc("GET /api/system-stats", s.handleSystemStats)

	mux.HandleFunc("GET /api/tune", s.handleTunePreview)
	mux.HandleFunc("POST /api/tune/apply", mut(s.handleTuneApply))

	mux.HandleFunc("GET /api/sites", s.handleSitesList)
	mux.HandleFunc("POST /api/sites", mut(s.handleSiteAdd))
	mux.HandleFunc("GET /api/sites/{domain}", s.handleSiteInfo)
	mux.HandleFunc("GET /api/sites/{domain}/activity", s.handleSiteActivity)
	mux.HandleFunc("DELETE /api/sites/{domain}", mut(s.handleSiteRemove))
	mux.HandleFunc("POST /api/sites/{domain}/enable", mut(s.handleSiteEnable(true)))
	mux.HandleFunc("POST /api/sites/{domain}/disable", mut(s.handleSiteEnable(false)))
	mux.HandleFunc("POST /api/sites/{domain}/fix-perms", mut(s.handleSiteFixPerms))
	mux.HandleFunc("GET /api/sites/{domain}/wp", s.handleWPOverview)
	mux.HandleFunc("POST /api/sites/{domain}/wp/run", mut(s.handleWPRun))
	mux.HandleFunc("POST /api/sites/{domain}/wp/reset-password", mut(s.handleWPResetPassword))

	mux.HandleFunc("POST /api/security/scan", mut(s.handleSecurityScan))
	mux.HandleFunc("GET /api/security/patch-plan", s.handleSecurityPatchPlan)
	mux.HandleFunc("POST /api/security/patch", mut(s.handleSecurityPatch))
	mux.HandleFunc("POST /api/security/install-clamav", mut(s.handleSecurityInstallClamAV))

	mux.HandleFunc("GET /api/backups", s.handleBackupsList)
	mux.HandleFunc("GET /api/backups/download", s.handleBackupDownload)
	mux.HandleFunc("DELETE /api/backups", mut(s.handleBackupDelete))
	mux.HandleFunc("POST /api/backups", mut(s.handleBackupCreate))
	mux.HandleFunc("POST /api/restore", mut(s.handleRestore))

	mux.HandleFunc("GET /api/borg/status", s.handleBorgStatus)
	mux.HandleFunc("POST /api/borg/ssh-key/generate", mut(s.handleBorgSSHKeyGenerate))
	mux.HandleFunc("POST /api/borg/setup", mut(s.handleBorgSetup))
	mux.HandleFunc("POST /api/borg/backup", mut(s.handleBorgBackup))
	mux.HandleFunc("GET /api/borg/archives", s.handleBorgArchives)
	mux.HandleFunc("DELETE /api/borg/archives", mut(s.handleBorgDeleteArchive))
	mux.HandleFunc("POST /api/borg/restore", mut(s.handleBorgRestore))
	mux.HandleFunc("POST /api/borg/schedule", mut(s.handleBorgSchedule))

	mux.HandleFunc("GET /api/db-manage/databases", s.handleDBManageDatabases)
	mux.HandleFunc("GET /api/db-manage/users", s.handleDBManageUsersList)
	mux.HandleFunc("POST /api/db-manage/users", mut(s.handleDBManageUserCreate))
	mux.HandleFunc("POST /api/db-manage/users/drop", mut(s.handleDBManageUserDrop))
	mux.HandleFunc("POST /api/db-manage/grant", mut(s.handleDBManageGrant))
	mux.HandleFunc("POST /api/db-manage/revoke", mut(s.handleDBManageRevoke))
	mux.HandleFunc("GET /api/db-manage/databases/{db}/tables", s.handleDBManageTables)
	mux.HandleFunc("GET /api/db-manage/databases/{db}/tables/{table}/rows", s.handleDBManageTableRows)
	mux.HandleFunc("POST /api/db-manage/databases/{db}/tables/{table}/rows", mut(s.handleDBManageRowUpdate))

	mux.HandleFunc("GET /api/config", s.handleConfigGet)
	mux.HandleFunc("POST /api/config", mut(s.handleConfigSet))

	mux.HandleFunc("POST /api/cache/purge", mut(s.handleCachePurge))
	mux.HandleFunc("GET /api/cache/stats", s.handleCacheStats)

	mux.HandleFunc("POST /api/ssl/issue", mut(s.handleSSLIssue))
	mux.HandleFunc("POST /api/ssl/renew", mut(s.handleSSLRenew))

	mux.HandleFunc("POST /api/setup", mut(s.handleSetup))
	mux.HandleFunc("POST /api/secure", mut(s.handleSecure))

	mux.HandleFunc("GET /api/uninstall/plan", s.handleUninstallPlan)
	mux.HandleFunc("POST /api/uninstall", mut(s.handleUninstall))

	mux.HandleFunc("GET /api/logs/sources", s.handleLogSources)
	mux.HandleFunc("GET /api/logs/tail", s.handleLogTail)

	mux.HandleFunc("POST /api/migrate/discover", mut(s.handleMigrateDiscover))
	mux.HandleFunc("POST /api/migrate/start", mut(s.handleMigrateStart))
	mux.HandleFunc("GET /api/migrate/status", s.handleMigrateStatus)
	mux.HandleFunc("POST /api/migrate/cancel", mut(s.handleMigrateCancel))

	return s.logRequests(s.requireAuth(mux))
}

// ---- middleware --------------------------------------------------------------

// requireFetchHeader is a CSRF-style guard on top of the SameSite=Strict
// session cookie: a second, independent reason another page open in the
// operator's browser cannot fire a background request (a hidden
// auto-submitting form, a stray fetch()) at this server. A cross-site
// request cannot set a custom header without triggering a CORS preflight,
// and this server sends no Access-Control-Allow-Origin for any preflight to
// succeed against, so requiring this header proves the request came from
// this app's own JavaScript.
func (s *Server) requireFetchHeader(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Requested-With") != "ngxsetup-web" {
			writeJSONError(w, http.StatusForbidden, "missing required header")
			return
		}
		h(w, r)
	}
}

func (s *Server) logRequests(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: 200}
		h.ServeHTTP(sw, r)
		s.audit(fmt.Sprintf("%s %s %s -> %d (%s)", remoteAddr(r), r.Method, r.URL.Path, sw.status, time.Since(start).Round(time.Millisecond)))
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (s *Server) audit(line string) {
	if s.auditLog == nil {
		return
	}
	fmt.Fprintf(s.auditLog, "%s %s\n", time.Now().UTC().Format(time.RFC3339), line)
}

const auditLogPath = "/var/log/ngxsetup-web.log"

func openAuditLog() (*os.File, error) {
	return os.OpenFile(auditLogPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
}

// constantTimeEqual is used for the confirmation-text checks on destructive
// actions (typing a domain name, typing UNINSTALL) — not a secret, but there
// is no reason to compare it any other way.
func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// remoteAddr strips the port from a request's address, for the audit log —
// the port is a new random number on every connection and carries no
// identity.
func remoteAddr(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
