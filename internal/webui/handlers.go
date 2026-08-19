package webui

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"ngxsetup/internal/build"
	"ngxsetup/internal/logx"
	"ngxsetup/internal/provision"
	"ngxsetup/internal/security"
	"ngxsetup/internal/state"
	"ngxsetup/internal/stats"
)

// ---- read-only dashboard endpoints -------------------------------------------

// handleVersion serves the values the sidebar footer credits — this binary's
// version (only meaningful on a release build; see internal/build) and the
// fixed maintainer/repository identity. A tiny endpoint rather than
// server-side templating index.html, matching how every other bit of data
// the frontend needs is already fetched over the API instead of baked into
// the embedded HTML at compile time.
func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"version":    build.Version,
		"maintainer": build.Maintainer,
		"repo_url":   build.RepoURL,
	})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	c, err := newCtx(r.Context(), false)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	rows, err := c.Status()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rows": rows})
}

func (s *Server) handleFacts(w http.ResponseWriter, r *http.Request) {
	c, err := newCtx(r.Context(), false)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"machine":  c.Facts.Describe(),
		"plan":     c.Plan.Summary(),
		"explain":  c.Plan.Explain(),
		"warnings": c.Plan.Warnings,
	})
}

func (s *Server) handleDoctor(w http.ResponseWriter, r *http.Request) {
	c, err := newCtx(r.Context(), false)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	checks := c.Diagnose()
	failures, warnings := 0, 0
	for _, ch := range checks {
		switch ch.Status {
		case provision.StatusFail:
			failures++
		case provision.StatusWarn:
			warnings++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"checks":   checks,
		"failures": failures,
		"warnings": warnings,
	})
}

// handleStats serves one sampling tick for the live dashboard — the web
// equivalent of `ngxsetup top`. The frontend polls this every couple of
// seconds; the sampler (a Server field, not request-local) is what makes the
// CPU%/request-rate numbers meaningful deltas rather than always reading as
// the average since boot.
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	c, err := newCtx(r.Context(), false)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	sites := make([]stats.Site, 0, len(c.State.Sites))
	for _, site := range c.State.Sites {
		if !site.Enabled {
			continue
		}
		sites = append(sites, stats.Site{
			Slug:       site.Slug,
			Domain:     site.Domain,
			DBName:     site.DBName,
			AccessLog:  fmt.Sprintf("/var/log/nginx/%s.access.log", site.Slug),
			MaxWorkers: c.Plan.PHP.MaxChildren,
			SocketPath: site.SocketPath,
		})
	}
	samples := s.sampler.Sample(r.Context(), sites)
	out := make([]map[string]any, 0, len(samples))
	for _, sample := range samples {
		row := map[string]any{
			"slug":                     sample.Slug,
			"domain":                   sample.Domain,
			"cpu_percent":              sample.CPUPercent,
			"memory_mb":                sample.MemoryMB,
			"workers":                  sample.Workers,
			"max_workers":              sample.MaxWorkers,
			"req_per_sec":              sample.ReqPerSec,
			"cache_hit_pct":            sample.CacheHitPercent,
			"total_requests":           sample.TotalRequests,
			"db_size_mb":               sample.DBSizeMB,
			"fpm_listen_queue":         sample.FPMListenQueue,
			"fpm_max_children_reached": sample.FPMMaxChildrenReached,
			"fpm_slow_requests":        sample.FPMSlowRequests,
			"healthy":                  sample.Healthy(),
		}
		if sample.Err != nil {
			row["error"] = sample.Err.Error()
		}
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{"sites": out})
}

// handleSystemStats serves one host-wide sampling tick for the Live Stats
// page's machine-level graphs (CPU, memory, disk, load, nginx connections,
// database performance) — the counterpart to handleStats, which is per-site.
// Both share the same long-lived s.sampler instance so CPU% and queries/sec
// are meaningful deltas from one poll to the next rather than averages since
// boot.
func (s *Server) handleSystemStats(w http.ResponseWriter, r *http.Request) {
	stat := s.sampler.SampleSystem(r.Context())
	out := map[string]any{
		"timestamp":         stat.Timestamp,
		"cpu_percent":       stat.CPUPercent,
		"cores":             stat.Cores,
		"mem_total_mb":      stat.MemTotalMB,
		"mem_used_mb":       stat.MemUsedMB,
		"mem_avail_mb":      stat.MemAvailMB,
		"mem_used_percent":  stat.MemUsedPercent,
		"swap_mb":           stat.SwapMB,
		"disk_path":         stat.DiskPath,
		"disk_total_mb":     stat.DiskTotalMB,
		"disk_used_mb":      stat.DiskUsedMB,
		"disk_used_percent": stat.DiskUsedPercent,
		"load1":             stat.Load1,
		"load5":             stat.Load5,
		"load15":            stat.Load15,
		"nginx": map[string]any{
			"active":   stat.Nginx.Active,
			"accepts":  stat.Nginx.Accepts,
			"handled":  stat.Nginx.Handled,
			"requests": stat.Nginx.Requests,
			"reading":  stat.Nginx.Reading,
			"writing":  stat.Nginx.Writing,
			"waiting":  stat.Nginx.Waiting,
		},
		"db": map[string]any{
			"threads_connected":       stat.DB.ThreadsConnected,
			"threads_running":         stat.DB.ThreadsRunning,
			"queries_per_sec":         stat.DB.QueriesPerSec,
			"slow_queries":            stat.DB.SlowQueries,
			"uptime_sec":              stat.DB.UptimeSec,
			"max_used_connections":    stat.DB.MaxUsedConnections,
			"buffer_pool_hit_percent": stat.DB.BufferPoolHitPercent,
		},
	}
	if stat.NginxErr != nil {
		out["nginx_error"] = stat.NginxErr.Error()
	}
	if stat.DBErr != nil {
		out["db_error"] = stat.DBErr.Error()
	}
	writeJSON(w, http.StatusOK, out)
}

// ---- tuning -------------------------------------------------------------------

func (s *Server) handleTunePreview(w http.ResponseWriter, r *http.Request) {
	opts := provision.Options{Profile: r.URL.Query().Get("profile")}
	c, err := provision.New(r.Context(), opts)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"machine":  c.Facts.Describe(),
		"plan":     c.Plan.Summary(),
		"explain":  c.Plan.Explain(),
		"warnings": c.Plan.Warnings,
	})
}

type tuneApplyRequest struct {
	Profile   string `json:"profile"`
	WorkerMB  int    `json:"worker_mb"`
	ReserveMB int    `json:"reserve_mb"`
	Save      bool   `json:"save"`
}

func (s *Server) handleTuneApply(w http.ResponseWriter, r *http.Request) {
	var req tuneApplyRequest
	if err := readJSON(r, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	output, err := runCaptured(func() error {
		opts := provision.Options{Profile: req.Profile, AvgPHPWorkerMB: req.WorkerMB, ReserveMB: req.ReserveMB}
		c, err := provision.New(r.Context(), opts)
		if err != nil {
			return err
		}
		if err := provision.RequireSetup(c); err != nil {
			return err
		}
		if err := c.Transaction("Applying nginx configuration", c.ApplyNginx, c.ValidateNginx); err != nil {
			return err
		}
		if err := c.Transaction("Applying PHP configuration", c.ApplyPHP, c.ValidatePHP); err != nil {
			return err
		}
		if err := c.Transaction("Applying database configuration", c.ApplyDB, c.ValidateDB); err != nil {
			return err
		}
		if err := c.Transaction("Applying kernel and service limits", c.ApplySystem, nil); err != nil {
			return err
		}
		if err := c.ReloadServices(); err != nil {
			return err
		}
		if req.Save {
			c.Config.Profile = string(c.Plan.Profile)
			if req.WorkerMB > 0 {
				c.Config.AvgPHPWorkerMB = req.WorkerMB
			}
			if req.ReserveMB > 0 {
				c.Config.ReserveMB = req.ReserveMB
			}
			if err := c.Config.Save(); err != nil {
				return err
			}
		}
		c.State.Profile = string(c.Plan.Profile)
		c.State.Touch()
		return c.State.Save()
	})
	writeActionResult(w, output, err, nil)
}

// ---- sites ----------------------------------------------------------------

func (s *Server) handleSitesList(w http.ResponseWriter, r *http.Request) {
	c, err := newCtx(r.Context(), false)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// c.State.Sites is nil (not empty) on a freshly provisioned box with no
	// sites yet, which encoding/json renders as JSON null — the same
	// "null.map() crashes the page" hazard fixed in handleBackupsList
	// above, and the highest-impact instance of it: the frontend calls
	// .map() on this array unguarded in every view that offers a site
	// picker, not just one page.
	sites := c.State.Sites
	if sites == nil {
		sites = []state.Site{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"sites": sites})
}

func (s *Server) handleSiteInfo(w http.ResponseWriter, r *http.Request) {
	c, err := newCtx(r.Context(), false)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	site, err := c.State.Find(r.PathValue("domain"))
	if err != nil {
		writeJSONError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, site)
}

type siteAddRequest struct {
	Domain        string `json:"domain"`
	Aliases       string `json:"aliases"`
	WordPress     bool   `json:"wordpress"`
	TLS           bool   `json:"tls"`
	SelfSigned    bool   `json:"self_signed"`
	Install       bool   `json:"install"`
	AdminUser     string `json:"admin_user"`
	AdminEmail    string `json:"admin_email"`
	Title         string `json:"title"`
	NoCache       bool   `json:"no_cache"`
	AllowFileMods bool   `json:"allow_file_mods"`
}

func (s *Server) handleSiteAdd(w http.ResponseWriter, r *http.Request) {
	var req siteAddRequest
	if err := readJSON(r, &req); err != nil || req.Domain == "" {
		writeJSONError(w, http.StatusBadRequest, "a domain is required")
		return
	}
	if req.TLS && req.SelfSigned {
		writeJSONError(w, http.StatusBadRequest, "tls and self_signed are mutually exclusive")
		return
	}
	if req.Install && !req.WordPress {
		writeJSONError(w, http.StatusBadRequest, "install requires wordpress")
		return
	}
	if req.Install && req.AdminEmail == "" {
		writeJSONError(w, http.StatusBadRequest, "install requires an admin email")
		return
	}

	var created *state.Site
	output, err := runCaptured(func() error {
		c, err := newCtx(r.Context(), false)
		if err != nil {
			return err
		}
		if err := provision.RequireSetup(c); err != nil {
			return err
		}
		rec, err := c.AddSite(provision.SiteRequest{
			Domain:        req.Domain,
			Aliases:       splitCSV(req.Aliases),
			WordPress:     req.WordPress,
			TLS:           req.TLS,
			SelfSigned:    req.SelfSigned,
			InstallWP:     req.Install,
			AdminUser:     req.AdminUser,
			AdminEmail:    req.AdminEmail,
			Title:         req.Title,
			DisableCache:  req.NoCache,
			AllowFileMods: req.AllowFileMods,
		})
		created = rec
		return err
	})
	writeActionResult(w, output, err, created)
}

type siteRemoveRequest struct {
	PurgeFiles    bool   `json:"purge_files"`
	PurgeDB       bool   `json:"purge_db"`
	ConfirmDomain string `json:"confirm_domain"`
}

func (s *Server) handleSiteRemove(w http.ResponseWriter, r *http.Request) {
	domain := r.PathValue("domain")
	var req siteRemoveRequest
	if err := readJSON(r, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	// The browser's confirm dialog already asked once; echoing the domain
	// name back is the same "type it to prove you meant it" pattern the CLI
	// gets for free from an interactive terminal, applied to a form instead.
	if req.PurgeFiles || req.PurgeDB {
		if !constantTimeEqual(req.ConfirmDomain, domain) {
			writeJSONError(w, http.StatusBadRequest, "confirm_domain must exactly match the domain being removed")
			return
		}
	}
	output, err := runCaptured(func() error {
		c, err := newCtx(r.Context(), false)
		if err != nil {
			return err
		}
		return c.RemoveSite(domain, req.PurgeFiles, req.PurgeDB)
	})
	writeActionResult(w, output, err, nil)
}

func (s *Server) handleSiteEnable(enabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		domain := r.PathValue("domain")
		output, err := runCaptured(func() error {
			c, err := newCtx(r.Context(), false)
			if err != nil {
				return err
			}
			return c.SetEnabled(domain, enabled)
		})
		writeActionResult(w, output, err, nil)
	}
}

func (s *Server) handleSiteFixPerms(w http.ResponseWriter, r *http.Request) {
	domain := r.PathValue("domain")
	output, err := runCaptured(func() error {
		c, err := newCtx(r.Context(), false)
		if err != nil {
			return err
		}
		return c.FixPermissions([]string{domain})
	})
	writeActionResult(w, output, err, nil)
}

// ---- security ---------------------------------------------------------------

type securityRequest struct {
	Domain string `json:"domain"`
}

func securityTargetsWeb(c *provision.Ctx, domain string) ([]security.Target, error) {
	var sites []state.Site
	if domain != "" {
		st, err := c.State.Find(domain)
		if err != nil {
			return nil, err
		}
		sites = []state.Site{*st}
	} else {
		sites = c.State.Sites
	}
	targets := make([]security.Target, 0, len(sites))
	for _, st := range sites {
		if !st.WordPress {
			continue
		}
		targets = append(targets, security.Target{Domain: st.Domain, User: st.User, Root: c.Path(st.Root)})
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("no WordPress sites to act on")
	}
	return targets, nil
}

func (s *Server) handleSecurityScan(w http.ResponseWriter, r *http.Request) {
	var req securityRequest
	_ = readJSON(r, &req)

	output, err := runCaptured(func() error {
		c, err := newCtx(r.Context(), false)
		if err != nil {
			return err
		}
		targets, err := securityTargetsWeb(c, req.Domain)
		if err != nil {
			return err
		}
		scanner := security.Scanner{Runner: c.Runner, YARARulesDir: c.Config.SecurityYARARulesDir}
		critical := 0
		for _, target := range targets {
			logx.Section("Scanning %s", target.Domain)
			report, err := scanner.Scan(r.Context(), target)
			if err != nil {
				logx.Error("%v", err)
				continue
			}
			if len(report.LayersRun) > 0 {
				logx.Info("layers run: %s", strings.Join(report.LayersRun, ", "))
			}
			for layer, reason := range report.LayersSkipped {
				logx.Warn("%s skipped: %s", layer, reason)
			}
			if report.Clean() {
				logx.Change("no critical or warning findings")
			}
			for _, f := range report.Findings {
				line := f.Title
				if f.Path != "" {
					line += " (" + f.Path + ")"
				}
				switch f.Severity {
				case security.Critical:
					logx.Error("%s", line)
					critical++
				case security.Warning:
					logx.Warn("%s", line)
				default:
					logx.Info("%s", line)
				}
				logx.Info("    %s", f.Detail)
				if f.Fix != "" {
					logx.Info("    -> %s", f.Fix)
				}
			}
		}
		if critical > 0 {
			return fmt.Errorf("%d critical finding(s) — see above", critical)
		}
		return nil
	})
	writeActionResult(w, output, err, nil)
}

// singleSecurityTarget resolves exactly one WordPress site by domain — what
// both the patch-plan and the (now per-site) patch-apply endpoints need,
// since plugin and theme lists only mean anything for one specific site.
func singleSecurityTarget(c *provision.Ctx, domain string) (security.Target, error) {
	if domain == "" {
		return security.Target{}, fmt.Errorf("a domain is required")
	}
	targets, err := securityTargetsWeb(c, domain)
	if err != nil {
		return security.Target{}, err
	}
	return targets[0], nil
}

// handleSecurityPatchPlan reports what is outdated for one site — current
// and latest version for core, and for every plugin and theme wp-cli
// reports an update for — without changing anything, so the web UI can let
// an operator choose what to actually patch instead of committing to
// "everything" sight unseen.
func (s *Server) handleSecurityPatchPlan(w http.ResponseWriter, r *http.Request) {
	domain := r.URL.Query().Get("domain")
	c, err := newCtx(r.Context(), false)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	target, err := singleSecurityTarget(c, domain)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	wp := security.WPCLI{Runner: c.Runner, User: target.User, Path: target.Root}
	if !wp.Available() {
		writeJSONError(w, http.StatusUnprocessableEntity, "wp-cli is not installed")
		return
	}
	plan, err := wp.PlanPatch(r.Context(), domain)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	resp := map[string]any{
		"domain":       domain,
		"core_current": plan.CoreCurrentVersion,
		"core_latest":  plan.CoreUpdate, // "" means already current
		"plugins":      plan.Plugins,
		"themes":       plan.Themes,
	}
	writeJSON(w, http.StatusOK, resp)
}

type securityPatchApplyRequest struct {
	Domain  string   `json:"domain"`
	Core    bool     `json:"core"`
	Plugins []string `json:"plugins"`
	Themes  []string `json:"themes"`
}

// handleSecurityPatch applies only the items the operator selected on the
// patch-plan view — never "everything outdated," which the old single
// "Patch now" button used to commit to sight unseen.
func (s *Server) handleSecurityPatch(w http.ResponseWriter, r *http.Request) {
	var req securityPatchApplyRequest
	if err := readJSON(r, &req); err != nil || req.Domain == "" {
		writeJSONError(w, http.StatusBadRequest, "a domain is required")
		return
	}

	output, err := runCaptured(func() error {
		c, err := newCtx(r.Context(), false)
		if err != nil {
			return err
		}
		target, err := singleSecurityTarget(c, req.Domain)
		if err != nil {
			return err
		}
		wp := security.WPCLI{Runner: c.Runner, User: target.User, Path: target.Root}
		if !wp.Available() {
			return fmt.Errorf("wp-cli is not installed")
		}
		logx.Section("Checking %s", req.Domain)
		full, err := wp.PlanPatch(r.Context(), req.Domain)
		if err != nil {
			return err
		}
		plan := full.Select(req.Core, req.Plugins, req.Themes)
		if plan.Empty() {
			return fmt.Errorf("nothing selected is still outdated — reload the update list and try again")
		}
		for _, line := range plan.Describe() {
			logx.Bullet("%s", line)
		}
		result := wp.ApplyPatch(r.Context(), &plan)
		if result.CoreUpdated {
			logx.Change("%s: WordPress core updated to %s", req.Domain, plan.CoreUpdate)
		}
		if result.CoreErr != nil {
			logx.Error("%s: core update failed: %v", req.Domain, result.CoreErr)
		}
		for _, p := range result.PluginsUpdated {
			logx.Change("%s: plugin %s updated", req.Domain, p)
		}
		for name, e := range result.PluginErrs {
			logx.Error("%s: plugin %s update failed: %v", req.Domain, name, e)
		}
		for _, t := range result.ThemesUpdated {
			logx.Change("%s: theme %s updated", req.Domain, t)
		}
		for name, e := range result.ThemeErrs {
			logx.Error("%s: theme %s update failed: %v", req.Domain, name, e)
		}
		if !result.Success() {
			return fmt.Errorf("one or more selected updates failed — see above")
		}
		return nil
	})
	writeActionResult(w, output, err, nil)
}

// handleSecurityInstallClamAV is the one-click alternative to an operator
// SSHing in to run `apt install clamav` after a scan reports it missing.
func (s *Server) handleSecurityInstallClamAV(w http.ResponseWriter, r *http.Request) {
	output, err := runCaptured(func() error {
		c, err := newCtx(r.Context(), false)
		if err != nil {
			return err
		}
		return c.InstallClamAV()
	})
	writeActionResult(w, output, err, nil)
}

// ---- backup / restore ---------------------------------------------------------

func (s *Server) handleBackupsList(w http.ResponseWriter, r *http.Request) {
	c, err := newCtx(r.Context(), false)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	files, err := c.ListBackups("")
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// ListBackups returns a nil slice (not an empty one) when the directory
	// has no .sql files yet, which encoding/json renders as JSON null —
	// fine for a Go caller (nil ranges the same as empty), but the frontend
	// unconditionally calls .map() on this array to populate the restore
	// form's dropdown, and null.map() throws, taking down the whole
	// Backups page. A zero-length backup directory is a completely normal
	// state (e.g. right after every local backup has been downloaded and
	// deleted in favour of borg), so this is not an edge case worth letting
	// break the page.
	if files == nil {
		files = []provision.BackupFile{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"backups": files})
}

// resolveBackupPath turns a client-supplied path (whatever ListBackups
// reported, but never trusted as such) into a real, verified file beneath
// the backup directory — or an error safe to show a client. Shared by
// download and delete, the two handlers that touch a specific backup file
// by name, so a crafted path is rejected the same way in both rather than
// two independently-maintained checks that could drift.
func resolveBackupPath(c *provision.Ctx, requested string) (string, os.FileInfo, error) {
	if requested == "" {
		return "", nil, fmt.Errorf("path is required")
	}
	backupDir, err := filepath.Abs(c.Path(provision.DefaultBackupDir))
	if err != nil {
		return "", nil, err
	}
	candidate := requested
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(c.Path(provision.DefaultBackupDir), candidate)
	} else {
		candidate = c.Path(candidate)
	}
	full, err := filepath.Abs(candidate)
	if err != nil || (full != backupDir && !strings.HasPrefix(full, backupDir+string(filepath.Separator))) {
		return "", nil, fmt.Errorf("path is not a recognised backup")
	}
	info, err := os.Stat(full)
	if err != nil || info.IsDir() || filepath.Ext(full) != ".sql" {
		return "", nil, fmt.Errorf("backup not found")
	}
	return full, info, nil
}

// handleBackupDownload streams one backup file to the browser.
func (s *Server) handleBackupDownload(w http.ResponseWriter, r *http.Request) {
	c, err := newCtx(r.Context(), false)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	full, info, err := resolveBackupPath(c, r.URL.Query().Get("path"))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/sql")
	w.Header().Set("Content-Disposition", "attachment; filename=\""+filepath.Base(full)+"\"")
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	http.ServeFile(w, r, full)
}

// handleBackupDelete removes one backup file. Irreversible, so it is behind
// the same X-Requested-With mutation guard every other destructive endpoint
// is — the browser side additionally asks for confirmation before ever
// sending this request.
func (s *Server) handleBackupDelete(w http.ResponseWriter, r *http.Request) {
	c, err := newCtx(r.Context(), false)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var req struct {
		Path string `json:"path"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	full, _, err := resolveBackupPath(c, req.Path)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := os.Remove(full); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	logx.Change("deleted backup %s", filepath.Base(full))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

type backupRequest struct {
	Domain string `json:"domain"`
}

func (s *Server) handleBackupCreate(w http.ResponseWriter, r *http.Request) {
	var req backupRequest
	_ = readJSON(r, &req)

	var data any
	output, err := runCaptured(func() error {
		c, err := newCtx(r.Context(), false)
		if err != nil {
			return err
		}
		if req.Domain != "" {
			res, err := c.BackupDatabase(req.Domain, "")
			if err != nil {
				return err
			}
			logx.Change("backed up %s to %s (%.1f MB)", res.Domain, res.Path, res.SizeMB)
			data = res
			return nil
		}
		results, err := c.BackupAllDatabases("")
		if err != nil {
			return err
		}
		failed := 0
		for _, res := range results {
			if res.Err != nil {
				logx.Error("%s: %v", res.Domain, res.Err)
				failed++
				continue
			}
			logx.Change("%s -> %s (%.1f MB)", res.Domain, res.Path, res.SizeMB)
		}
		data = results
		if failed > 0 {
			return fmt.Errorf("%d database backup(s) failed — see above", failed)
		}
		return nil
	})
	writeActionResult(w, output, err, data)
}

// maxRestoreUploadBytes bounds an uploaded dump. Generous for a WordPress
// database (even a large WooCommerce catalogue is typically well under this),
// but finite so a request body cannot pin arbitrary memory/disk.
const maxRestoreUploadBytes = 2 << 30 // 2 GiB

func (s *Server) handleRestore(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeJSONError(w, http.StatusBadRequest, "expected a multipart form with a 'file' upload")
		return
	}
	domain := r.FormValue("domain")
	existingPath := r.FormValue("existing_path") // pick from an already-listed backup instead of uploading
	noSafety := r.FormValue("no_safety_backup") == "true"
	confirmDomain := r.FormValue("confirm_domain")
	if domain == "" {
		writeJSONError(w, http.StatusBadRequest, "a domain is required")
		return
	}
	if !constantTimeEqual(confirmDomain, domain) {
		writeJSONError(w, http.StatusBadRequest, "confirm_domain must exactly match the domain being restored")
		return
	}

	sqlPath := existingPath
	var tmpFile string
	if sqlPath == "" {
		file, header, err := r.FormFile("file")
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "attach a .sql file, or set existing_path to a listed backup")
			return
		}
		defer file.Close()
		if header.Size > maxRestoreUploadBytes {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "file is too large")
			return
		}
		tmp, err := os.CreateTemp("", "ngxsetup-restore-*.sql")
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		defer os.Remove(tmp.Name())
		defer tmp.Close()
		if _, err := io.Copy(tmp, io.LimitReader(file, maxRestoreUploadBytes+1)); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "saving upload: "+err.Error())
			return
		}
		tmp.Close()
		sqlPath = tmp.Name()
		tmpFile = tmp.Name()
	}
	if tmpFile != "" {
		defer os.Remove(tmpFile)
	}

	var data any
	output, err := runCaptured(func() error {
		c, err := newCtx(r.Context(), false)
		if err != nil {
			return err
		}
		// existingPath is relative to DefaultBackupDir when it came from the
		// listing endpoint; resolve it the same way ListBackups reported it.
		path := sqlPath
		if existingPath != "" && !filepath.IsAbs(existingPath) {
			path = c.Path(existingPath)
		}
		res, err := c.RestoreDatabase(domain, path, noSafety)
		data = res
		return err
	})
	writeActionResult(w, output, err, data)
}

// ---- config -----------------------------------------------------------------

func (s *Server) handleConfigGet(w http.ResponseWriter, r *http.Request) {
	c, err := newCtx(r.Context(), false)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rows": provision.ConfigRows(c)})
}

type configSetRequest struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

func (s *Server) handleConfigSet(w http.ResponseWriter, r *http.Request) {
	var req configSetRequest
	if err := readJSON(r, &req); err != nil || req.Key == "" {
		writeJSONError(w, http.StatusBadRequest, "key is required")
		return
	}
	output, err := runCaptured(func() error {
		c, err := newCtx(r.Context(), false)
		if err != nil {
			return err
		}
		if err := provision.SetConfigKey(c, req.Key, req.Value); err != nil {
			return err
		}
		if err := c.Config.Save(); err != nil {
			return err
		}
		logx.Change("set %s = %s", req.Key, req.Value)
		logx.Info("Apply tuning-affecting changes with the Tuning page.")
		return nil
	})
	writeActionResult(w, output, err, nil)
}

// ---- cache / ssl --------------------------------------------------------------

type domainRequest struct {
	Domain string `json:"domain"`
}

func (s *Server) handleCachePurge(w http.ResponseWriter, r *http.Request) {
	var req domainRequest
	_ = readJSON(r, &req)
	output, err := runCaptured(func() error {
		c, err := newCtx(r.Context(), false)
		if err != nil {
			return err
		}
		if err := c.PurgeCache(req.Domain); err != nil {
			return err
		}
		logx.Change("cache purged")
		return nil
	})
	writeActionResult(w, output, err, nil)
}

func (s *Server) handleCacheStats(w http.ResponseWriter, r *http.Request) {
	c, err := newCtx(r.Context(), false)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	entries, bytesUsed, err := c.CacheStats()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"entries":      entries,
		"disk_mb":      bytesUsed / (1 << 20),
		"capacity_mb":  c.Plan.Nginx.CacheMaxSizeMB,
		"keys_zone_mb": c.Plan.Nginx.CacheKeysZoneMB,
	})
}

func (s *Server) handleSSLIssue(w http.ResponseWriter, r *http.Request) {
	var req domainRequest
	if err := readJSON(r, &req); err != nil || req.Domain == "" {
		writeJSONError(w, http.StatusBadRequest, "a domain is required")
		return
	}
	output, err := runCaptured(func() error {
		c, err := newCtx(r.Context(), false)
		if err != nil {
			return err
		}
		return c.UpgradeToTLS(req.Domain)
	})
	writeActionResult(w, output, err, nil)
}

func (s *Server) handleSSLRenew(w http.ResponseWriter, r *http.Request) {
	output, err := runCaptured(func() error {
		c, err := newCtx(r.Context(), false)
		if err != nil {
			return err
		}
		return c.RenewCertificates()
	})
	writeActionResult(w, output, err, nil)
}

// ---- setup / secure -----------------------------------------------------------

type setupRequest struct {
	Database     string `json:"database"`
	SkipPackages bool   `json:"skip_packages"`
	Redis        bool   `json:"redis"`
}

func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	var req setupRequest
	_ = readJSON(r, &req)
	if req.Database == "" {
		req.Database = "mariadb"
	}
	if req.Database != "mariadb" && req.Database != "mysql" {
		writeJSONError(w, http.StatusBadRequest, "database must be mariadb or mysql")
		return
	}
	output, err := runCaptured(func() error {
		c, err := newCtx(r.Context(), false)
		if err != nil {
			return err
		}
		return c.Setup(provision.SetupRequest{
			Database:     req.Database,
			SkipPackages: req.SkipPackages,
			InstallRedis: req.Redis,
		})
	})
	writeActionResult(w, output, err, nil)
}

type secureRequest struct {
	RefreshCloudflare bool   `json:"refresh_cloudflare"`
	PMAUser           string `json:"pma_user"`
	PMAPassword       string `json:"pma_password"`
}

func (s *Server) handleSecure(w http.ResponseWriter, r *http.Request) {
	var req secureRequest
	_ = readJSON(r, &req)

	output, err := runCaptured(func() error {
		c, err := newCtx(r.Context(), false)
		if err != nil {
			return err
		}
		if req.PMAUser != "" {
			if req.PMAPassword == "" {
				return fmt.Errorf("a password is required to set a phpMyAdmin credential")
			}
			if err := c.SetPhpMyAdminCredential(req.PMAUser, req.PMAPassword); err != nil {
				return err
			}
		}
		if req.RefreshCloudflare {
			c.Config.TrustCloudflare = true
		}
		if err := c.Transaction("Hardening", func() error {
			if err := c.ApplySecurity(); err != nil {
				return err
			}
			if req.RefreshCloudflare {
				return c.ApplyNginx()
			}
			return c.ApplyPhpMyAdmin()
		}, c.ValidateNginx); err != nil {
			return err
		}
		return c.ReloadServices()
	})
	writeActionResult(w, output, err, nil)
}

// ---- uninstall ------------------------------------------------------------------

type uninstallRequest struct {
	PurgeSites    bool   `json:"purge_sites"`
	PurgePackages bool   `json:"purge_packages"`
	Confirm       string `json:"confirm"`
}

func (s *Server) handleUninstallPlan(w http.ResponseWriter, r *http.Request) {
	c, err := newCtx(r.Context(), false)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	purgeSites, _ := strconv.ParseBool(r.URL.Query().Get("purge_sites"))
	purgePackages, _ := strconv.ParseBool(r.URL.Query().Get("purge_packages"))
	plan := c.PlanUninstall(provision.UninstallOptions{PurgeSites: purgeSites, PurgePackages: purgePackages})
	writeJSON(w, http.StatusOK, map[string]any{"lines": plan.Describe()})
}

// requiredUninstallPhrase is what the operator must type to confirm — the web
// equivalent of the CLI's interactive `[y/N]` prompt, deliberately harder to
// trigger by accident than a single click.
const requiredUninstallPhrase = "UNINSTALL"

func (s *Server) handleUninstall(w http.ResponseWriter, r *http.Request) {
	var req uninstallRequest
	if err := readJSON(r, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if !constantTimeEqual(req.Confirm, requiredUninstallPhrase) {
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("type %q to confirm", requiredUninstallPhrase))
		return
	}
	output, err := runCaptured(func() error {
		c, err := newCtx(r.Context(), false)
		if err != nil {
			return err
		}
		return c.Uninstall(provision.UninstallOptions{PurgeSites: req.PurgeSites, PurgePackages: req.PurgePackages})
	})
	writeActionResult(w, output, err, nil)
}

// ---- helpers --------------------------------------------------------------

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
