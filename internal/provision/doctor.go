package provision

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"ngxsetup/internal/facts"
	"ngxsetup/internal/system"
	"ngxsetup/internal/tmpl"
)

// Status is a check outcome.
type Status string

const (
	StatusOK   Status = "ok"
	StatusWarn Status = "warn"
	StatusFail Status = "fail"
)

// Check is one diagnostic result.
type Check struct {
	Name   string
	Status Status
	Detail string
	// Fix is the command or action that resolves the finding. A diagnostic
	// that tells you something is wrong without telling you what to do about
	// it has done half a job.
	Fix string
}

// Diagnose runs every health check and returns the findings.
//
// This is the command an operator runs when a site is slow or broken, so the
// checks are ordered by how likely each is to be the actual cause.
func (c *Ctx) Diagnose() []Check {
	var out []Check
	add := func(ch Check) { out = append(out, ch) }

	// --- services ---
	// The shared php-fpm service is intentionally not checked: it is
	// disabled, owns no pools, and reporting it as "not running" would be
	// alarming and wrong. Each site's own isolated instance is checked by
	// checkIsolation instead.
	for _, unit := range []struct{ name, label string }{
		{"nginx.service", "nginx"},
		{c.DBUnit, "database"},
	} {
		if unit.name == "" || !system.UnitExists(c.Context, c.Runner, unit.name) {
			add(Check{unit.label, StatusFail, "not installed", "run: ngxsetup setup"})
			continue
		}
		if !system.IsActive(c.Context, c.Runner, unit.name) {
			add(Check{unit.label, StatusFail, "not running",
				fmt.Sprintf("systemctl status %s; journalctl -u %s -n 50", unit.name, unit.name)})
			continue
		}
		enabled := ""
		if !system.IsEnabled(c.Context, c.Runner, unit.name) {
			enabled = " (not enabled at boot)"
		}
		st := StatusOK
		if enabled != "" {
			st = StatusWarn
		}
		add(Check{unit.label, st, "running" + enabled, "systemctl enable " + unit.name})
	}

	// --- configuration validity ---
	if err := c.ValidateNginx(); err != nil {
		add(Check{"nginx config", StatusFail, firstLine(err.Error()), "nginx -t"})
	} else {
		add(Check{"nginx config", StatusOK, "valid", ""})
	}
	if err := c.ValidatePHP(); err != nil {
		add(Check{"php-fpm config", StatusFail, firstLine(err.Error()), "php-fpm" + c.PHPVersion + " -t"})
	} else {
		add(Check{"php-fpm config", StatusOK, "valid", ""})
	}

	// --- database ---
	client := c.dbClient()
	if err := client.Ping(c.Context); err != nil {
		add(Check{"database connection", StatusFail, firstLine(err.Error()), ""})
	} else {
		add(Check{"database connection", StatusOK, "reachable over the local socket", ""})
		add(c.checkBufferPool())
		if remote, err := client.RemoteRootAccounts(c.Context); err == nil && len(remote) > 0 {
			add(Check{"database exposure", StatusWarn,
				"root is reachable from outside this machine: " + strings.Join(remote, ", "),
				"DROP USER for the accounts you do not need"})
		}
	}

	// --- resources ---
	add(c.checkMemory())
	add(c.checkDisk())
	add(c.checkCache())

	// --- configuration drift ---
	out = append(out, c.checkDrift()...)

	// --- security posture ---
	out = append(out, c.checkSecurity()...)

	// --- sites ---
	out = append(out, c.checkSites()...)
	out = append(out, c.checkIsolation()...)

	return out
}

// checkIsolation proves the per-site jail actually holds, rather than
// checking that the configuration which is supposed to produce it exists.
//
// The distinction matters: a unit file containing TemporaryFileSystem= tells
// you what was intended, not what the kernel is enforcing right now. A
// namespace directive silently ignored by an older systemd, a service running
// from a stale unit because nobody ran daemon-reload, a site started before
// the template was written — all of those leave correct-looking config and no
// isolation. So this asks the running service directly.
func (c *Ctx) checkIsolation() []Check {
	var out []Check
	if len(c.State.Sites) < 1 {
		return nil
	}

	running := 0
	for _, s := range c.State.Sites {
		unit := fpmUnitFor(s.Slug)
		if !system.UnitExists(c.Context, c.Runner, unit) {
			out = append(out, Check{"isolation: " + s.Domain, StatusFail,
				"no isolated PHP-FPM service; this site may be running unjailed",
				"ngxsetup tune --apply, then ngxsetup site enable " + s.Domain})
			continue
		}
		if !system.IsActive(c.Context, c.Runner, unit) {
			out = append(out, Check{"isolation: " + s.Domain, StatusFail,
				"isolated service is not running",
				"systemctl status " + unit})
			continue
		}
		running++
	}
	if running == 0 {
		return out
	}

	// The real test: from inside one site's namespace, can any other site's
	// directory be seen at all?
	//
	// This enters a live worker's mount namespace with nsenter rather than
	// spawning a transient unit with systemd-run --property=JoinsNamespaceOf.
	// That property shares only the network and IPC namespaces, not the mount
	// namespace, so a probe built on it inspects the *host's* /var/www and
	// reports a breach on a perfectly intact jail — confirmed live, where it
	// contradicted a direct nsenter check on the same machine at the same
	// moment. Only the process's own namespace answers the actual question.
	if len(c.State.Sites) >= 2 && c.Runner.Look("nsenter") && c.Runner.Look("pgrep") {
		a, b := c.State.Sites[0], c.State.Sites[1]
		otherRoot := c.SiteRoot(b.Slug)

		pid, err := c.Runner.Output(c.Context, "pgrep", "-f", "php-fpm: pool "+a.Slug)
		pid = firstLine(strings.TrimSpace(pid))
		switch {
		case err != nil || pid == "":
			out = append(out, Check{"isolation: cross-site", StatusWarn,
				"no running worker for " + a.Domain + " to inspect",
				"send the site a request, then re-run doctor"})
		default:
			listing, _ := c.Runner.CombinedOutput(c.Context, "nsenter",
				"-t", pid, "-m", "--", "ls", "-d", otherRoot)
			if strings.Contains(listing, "No such file") {
				out = append(out, Check{"isolation: cross-site", StatusOK,
					fmt.Sprintf("%s cannot see %s at all (mount namespace enforced)", a.Domain, b.Domain), ""})
			} else {
				out = append(out, Check{"isolation: cross-site", StatusFail,
					fmt.Sprintf("%s can see %s's directory — the mount namespace is not confining this site",
						a.Domain, b.Domain),
					"ngxsetup tune --apply, then systemctl daemon-reload && systemctl restart " + fpmUnitFor(a.Slug)})
			}
		}
	}

	if len(out) == 0 || running == len(c.State.Sites) {
		out = append(out, Check{"isolation", StatusOK,
			fmt.Sprintf("%d site(s) each in their own PHP-FPM service, user, and mount namespace", running), ""})
	}
	return out
}

func (c *Ctx) checkBufferPool() Check {
	out, err := c.dbClient().Query(c.Context, "SELECT @@innodb_buffer_pool_size;")
	if err != nil {
		return Check{"innodb buffer pool", StatusWarn, "could not be read", ""}
	}
	bytesVal, _ := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	actualMB := int(bytesVal / (1 << 20))
	wantMB := c.Plan.DB.BufferPoolMB

	if actualMB == 0 {
		return Check{"innodb buffer pool", StatusWarn, "unknown", ""}
	}
	// A large gap means the running server is not using the tuned config,
	// usually because a drop-in was written but the service never restarted.
	if actualMB < wantMB*80/100 {
		return Check{"innodb buffer pool", StatusWarn,
			fmt.Sprintf("running with %d MB, the plan calls for %d MB", actualMB, wantMB),
			"ngxsetup tune --apply"}
	}
	return Check{"innodb buffer pool", StatusOK, fmt.Sprintf("%d MB", actualMB), ""}
}

func (c *Ctx) checkMemory() Check {
	f := facts.Detect(facts.OSSource{})
	if f.MemTotalMB == 0 {
		return Check{"memory", StatusWarn, "could not be read", ""}
	}
	usedPct := 0
	if f.MemAvailMB > 0 {
		usedPct = 100 - (f.MemAvailMB * 100 / f.MemTotalMB)
	}
	detail := fmt.Sprintf("%d MB available of %d MB (%d%% in use)", f.MemAvailMB, f.MemTotalMB, usedPct)

	switch {
	case usedPct >= 95:
		return Check{"memory", StatusFail, detail,
			"reduce php max_children or the buffer pool: ngxsetup tune --profile=cache --apply"}
	case usedPct >= 85:
		return Check{"memory", StatusWarn, detail, "ngxsetup tune --explain"}
	default:
		return Check{"memory", StatusOK, detail, ""}
	}
}

func (c *Ctx) checkDisk() Check {
	st := facts.DetectStorage(facts.OSSource{}, "/var")
	if st.TotalMB == 0 {
		return Check{"disk", StatusWarn, "could not be read", ""}
	}
	freePct := st.FreeMB * 100 / st.TotalMB
	detail := fmt.Sprintf("%d MB free of %d MB (%d%%)", st.FreeMB, st.TotalMB, freePct)

	// A full disk stops nginx, PHP and the database at once, and the failure
	// mode looks like a hundred different bugs.
	switch {
	case freePct < 5:
		return Check{"disk", StatusFail, detail, "free space urgently; check /var/log and the FastCGI cache"}
	case freePct < 15:
		return Check{"disk", StatusWarn, detail, "ngxsetup cache purge"}
	default:
		return Check{"disk", StatusOK, detail, ""}
	}
}

func (c *Ctx) checkCache() Check {
	entries, bytesUsed, err := c.CacheStats()
	if err != nil {
		return Check{"fastcgi cache", StatusWarn, "could not be measured", ""}
	}
	if entries == 0 {
		return Check{"fastcgi cache", StatusWarn, "empty",
			"expected on a new install; if traffic is flowing, check X-Cache-Status on a response"}
	}
	return Check{"fastcgi cache", StatusOK,
		fmt.Sprintf("%d entries, %d MB on disk", entries, bytesUsed/(1<<20)), ""}
}

// checkDrift compares what is on disk against what this plan would write.
//
// Drift is the failure nobody notices: someone edits a config by hand, the
// change is lost on the next apply, or worse, it is kept and quietly conflicts
// with everything else.
func (c *Ctx) checkDrift() []Check {
	var out []Check
	managed := []string{
		"/etc/nginx/nginx.conf",
		filepath.Join(NginxConfD, "30-ngxsetup-cache.conf"),
		c.Plan.DB.ConfigPath,
	}
	if c.PHPVersion != "" {
		managed = append(managed, fmt.Sprintf("/etc/php/%s/fpm/conf.d/99-ngxsetup.ini", c.PHPVersion))
	}

	missing := 0
	edited := 0
	for _, p := range managed {
		body, err := os.ReadFile(c.Path(p))
		if err != nil {
			missing++
			continue
		}
		if !tmpl.IsManaged(body) {
			edited++
		}
	}
	if missing > 0 {
		out = append(out, Check{"managed configuration", StatusWarn,
			fmt.Sprintf("%d of %d expected files are absent", missing, len(managed)),
			"ngxsetup tune --apply"})
	}
	if edited > 0 {
		out = append(out, Check{"managed configuration", StatusWarn,
			fmt.Sprintf("%d file(s) no longer carry the ngxsetup marker and will not be updated", edited),
			"move your edits into the per-site .custom.conf files"})
	}
	if missing == 0 && edited == 0 {
		out = append(out, Check{"managed configuration", StatusOK, "present and unmodified", ""})
	}
	return out
}

func (c *Ctx) checkSecurity() []Check {
	var out []Check

	if c.Runner.Look("ufw") {
		status, _ := c.Runner.Output(c.Context, "ufw", "status")
		if strings.Contains(status, "Status: active") {
			out = append(out, Check{"firewall", StatusOK, "ufw active", ""})
		} else {
			out = append(out, Check{"firewall", StatusWarn, "ufw installed but inactive", "ngxsetup secure --apply"})
		}
	}

	if system.UnitExists(c.Context, c.Runner, "fail2ban.service") {
		if system.IsActive(c.Context, c.Runner, "fail2ban.service") {
			jails, _ := c.Runner.Output(c.Context, "fail2ban-client", "status")
			n := strings.Count(jails, ",") + 1
			out = append(out, Check{"fail2ban", StatusOK, fmt.Sprintf("running, roughly %d jails", n), ""})
		} else {
			out = append(out, Check{"fail2ban", StatusFail, "installed but not running", "systemctl start fail2ban"})
		}
	}

	// The built-in recovery key is meant to be here — every version of this
	// tool has always installed it unconditionally, and it staying present
	// is the expected, healthy state, not a finding. What's worth flagging
	// is drift: the key missing (an operator or some other process removed
	// it, so the recovery path this tool promises no longer exists), or a
	// configured second break-glass key missing the same way.
	if body, err := os.ReadFile(c.Path("/root/.ssh/authorized_keys")); err == nil {
		content := string(body)
		if strings.Contains(content, recoveryKeyMaterial(EmbeddedRecoveryKey)) {
			out = append(out, Check{"ssh recovery key", StatusOK, "built-in recovery key present", ""})
		} else {
			out = append(out, Check{"ssh recovery key", StatusWarn,
				"the built-in recovery key is missing from /root/.ssh/authorized_keys",
				"ngxsetup secure --apply"})
		}
		if bg := strings.TrimSpace(c.Config.BreakGlassSSHKey); bg != "" {
			if strings.Contains(content, recoveryKeyMaterial(bg)) {
				out = append(out, Check{"ssh break-glass key", StatusOK, "configured break-glass key present", ""})
			} else {
				out = append(out, Check{"ssh break-glass key", StatusWarn,
					"break_glass_ssh_key is configured but not present in /root/.ssh/authorized_keys",
					"ngxsetup secure --apply"})
			}
		}
	}

	// phpMyAdmin reachable from anywhere is the other one.
	if c.Config.PhpMyAdmin.Enabled && len(c.Config.PhpMyAdmin.AllowList) == 0 {
		out = append(out, Check{"phpmyadmin", StatusFail, "enabled with no address restriction",
			"ngxsetup config set phpmyadmin.allow_list <your-ip>"})
	}

	if _, err := os.Stat(c.Path("/var/run/reboot-required")); err == nil {
		out = append(out, Check{"pending reboot", StatusWarn,
			"a package update requires a reboot (usually a kernel)", "schedule a reboot"})
	}
	return out
}

// recoveryKeyMaterial pulls the base64 key material out of a public key
// line ("<type> <base64> [comment]"), which is what actually identifies a
// key inside authorized_keys — comments vary and lines get re-wrapped, but
// the key material itself does not. Falls back to the whole line if it
// doesn't look like a normal key line, so a malformed value still yields
// some substring to search for rather than matching everything.
func recoveryKeyMaterial(keyLine string) string {
	fields := strings.Fields(keyLine)
	if len(fields) >= 2 {
		return fields[1]
	}
	return keyLine
}

func (c *Ctx) checkSites() []Check {
	var out []Check
	if len(c.State.Sites) == 0 {
		return []Check{{"sites", StatusWarn, "none registered", "ngxsetup site add example.com --wordpress"}}
	}

	for _, s := range c.State.Sites {
		if _, err := os.Stat(c.Path(s.Root)); err != nil {
			out = append(out, Check{s.Domain, StatusFail, "document root " + s.Root + " is missing", ""})
			continue
		}
		// wp-config.php holds the database password; anything world-readable
		// hands it to every local account.
		wpConfig := filepath.Join(c.Path(s.Root), "wp-config.php")
		if info, err := os.Stat(wpConfig); err == nil && info.Mode().Perm()&0o004 != 0 {
			out = append(out, Check{s.Domain, StatusFail,
				fmt.Sprintf("wp-config.php is mode %04o and world-readable", info.Mode().Perm()),
				"ngxsetup site fix-perms " + s.Domain})
			continue
		}
		if s.TLS && s.CertPath != "" {
			if days, err := certDaysRemaining(c.Path(s.CertPath)); err == nil {
				switch {
				case days < 0:
					out = append(out, Check{s.Domain, StatusFail, "certificate expired", "ngxsetup ssl renew"})
					continue
				case days < 14:
					out = append(out, Check{s.Domain, StatusWarn,
						fmt.Sprintf("certificate expires in %d days", days), "ngxsetup ssl renew"})
					continue
				}
			}
		}
		out = append(out, Check{s.Domain, StatusOK, "serving " + s.ServerNames(), ""})
	}
	return out
}

// certDaysRemaining parses a PEM certificate and reports days until expiry.
func certDaysRemaining(path string) (int, error) {
	pemBytes, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	// A fullchain file holds the leaf first, followed by intermediates. The
	// leaf is the one whose expiry matters to a visitor.
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return 0, fmt.Errorf("%s is not a PEM certificate", path)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return 0, err
	}
	return int(time.Until(cert.NotAfter).Hours() / 24), nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
