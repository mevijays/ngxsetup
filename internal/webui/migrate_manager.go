package webui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"ngxsetup/internal/logx"
	"ngxsetup/internal/migrate"
	"ngxsetup/internal/provision"
)

// discoveredSite is one vhost found on the remote host, including the
// database credentials MigrateManager needs later to dump it — never sent
// to the browser as-is; see (*discoveredSite).public.
type discoveredSite struct {
	migrate.NginxVHost
	migrate.WPConfigInfo
	WordPress    bool // wp-config.php was found and understood
	AlreadyLocal bool // this domain is already registered on this machine
}

func (d discoveredSite) public() map[string]any {
	reason := ""
	switch {
	case d.AlreadyLocal:
		reason = "a site with this domain already exists on this machine"
	case !d.WordPress:
		reason = "wp-config.php was not found (or not readable) at " + d.Root
	}
	return map[string]any{
		"domain":       d.Domain,
		"aliases":      d.Aliases,
		"root":         d.Root,
		"db_name":      d.DBName,
		"db_user":      d.DBUser,
		"table_prefix": d.TablePrefix,
		"migratable":   d.WordPress && !d.AlreadyLocal,
		"reason":       reason,
	}
}

// migrateSiteStatus is one site's live progress, polled by the frontend.
type migrateSiteStatus struct {
	Domain  string `json:"domain"`
	State   string `json:"state"` // pending, running, success, failed
	Step    string `json:"step"`
	Percent int    `json:"percent"`
	Error   string `json:"error,omitempty"`
}

// migrateJob is one migration run's whole state, from the moment sites are
// selected until the last one finishes.
type migrateJob struct {
	mu        sync.Mutex
	running   bool
	startedAt time.Time
	sites     map[string]*migrateSiteStatus
	order     []string
	log       []string
	cancel    context.CancelFunc
}

// maxJobLogLines bounds the in-memory log so a very large, very chatty
// migration (rsync's own progress lines especially) cannot grow this
// without limit for as long as the web UI process runs.
const maxJobLogLines = 4000

func newMigrateJob(domains []string) *migrateJob {
	j := &migrateJob{running: true, startedAt: time.Now(), sites: map[string]*migrateSiteStatus{}}
	for _, d := range domains {
		j.sites[d] = &migrateSiteStatus{Domain: d, State: "pending"}
		j.order = append(j.order, d)
	}
	return j
}

func (j *migrateJob) appendLog(line string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.log = append(j.log, line)
	if len(j.log) > maxJobLogLines {
		j.log = j.log[len(j.log)-maxJobLogLines:]
	}
}

func (j *migrateJob) logf(format string, args ...any) { j.appendLog(fmt.Sprintf(format, args...)) }

func (j *migrateJob) setState(domain, state string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if s, ok := j.sites[domain]; ok {
		s.State = state
	}
}

func (j *migrateJob) setStep(domain, step string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if s, ok := j.sites[domain]; ok {
		s.Step = step
		s.Percent = 0
	}
	j.log = append(j.log, fmt.Sprintf("[%s] %s", domain, step))
	if len(j.log) > maxJobLogLines {
		j.log = j.log[len(j.log)-maxJobLogLines:]
	}
}

func (j *migrateJob) setPercent(domain string, pct int) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if s, ok := j.sites[domain]; ok {
		s.Percent = pct
	}
}

func (j *migrateJob) fail(domain string, err error) {
	j.mu.Lock()
	if s, ok := j.sites[domain]; ok {
		s.State = "failed"
		s.Error = err.Error()
	}
	j.log = append(j.log, fmt.Sprintf("[%s] FAILED: %v", domain, err))
	j.mu.Unlock()
}

func (j *migrateJob) succeed(domain string) {
	j.mu.Lock()
	if s, ok := j.sites[domain]; ok {
		s.State = "success"
		s.Percent = 100
		s.Step = "done"
	}
	j.log = append(j.log, fmt.Sprintf("[%s] migration complete", domain))
	j.mu.Unlock()
}

func (j *migrateJob) finish() {
	j.mu.Lock()
	j.running = false
	j.mu.Unlock()
}

func (j *migrateJob) isRunning() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.running
}

func (j *migrateJob) snapshot() map[string]any {
	j.mu.Lock()
	defer j.mu.Unlock()
	sites := make([]migrateSiteStatus, 0, len(j.order))
	for _, d := range j.order {
		sites = append(sites, *j.sites[d])
	}
	log := make([]string, len(j.log))
	copy(log, j.log)
	return map[string]any{
		"running": j.running,
		"sites":   sites,
		"log":     log,
	}
}

// MigrateManager drives the whole remote-site-migration wizard: connecting,
// discovering vhosts, and running a (single, at a time — this whole tool's
// "no login, one operator" design does not need more) migration job.
//
// It holds the SSH private key on disk for exactly as long as a discovery
// or migration needs it — a fresh temp directory per discovery, removed the
// moment the job that used it finishes, so a leaked key never outlives the
// migration it was for. The key's content itself never appears in a log
// line or a status response.
type MigrateManager struct {
	mu         sync.Mutex
	workDir    string
	remote     migrate.RemoteConfig
	discovered map[string]discoveredSite
	job        *migrateJob
}

// DiscoverRequest is what the web UI's connection form submits.
type DiscoverRequest struct {
	Host       string
	Port       int
	User       string
	PrivateKey string
}

// Discover connects to the remote host, lists its nginx vhosts, and reads
// each one's wp-config.php — one long-lived connection setup (the key
// material, the known_hosts pinning) reused for every site rather than
// reconnecting per site.
func (m *MigrateManager) Discover(ctx context.Context, req DiscoverRequest) ([]map[string]any, error) {
	m.mu.Lock()
	if m.job != nil && m.job.isRunning() {
		m.mu.Unlock()
		return nil, fmt.Errorf("a migration is already running; wait for it to finish before discovering another host")
	}
	previousWorkDir := m.workDir
	m.mu.Unlock()

	if req.Host == "" || req.User == "" || strings.TrimSpace(req.PrivateKey) == "" {
		return nil, fmt.Errorf("host, user and a private key are required")
	}

	workDir, err := os.MkdirTemp("", "ngxsetup-migrate-*")
	if err != nil {
		return nil, fmt.Errorf("preparing a working directory: %w", err)
	}
	keyPath := filepath.Join(workDir, "id")
	key := req.PrivateKey
	if !strings.HasSuffix(key, "\n") {
		key += "\n"
	}
	if err := os.WriteFile(keyPath, []byte(key), 0o600); err != nil {
		os.RemoveAll(workDir)
		return nil, fmt.Errorf("writing the private key: %w", err)
	}
	knownHosts := filepath.Join(workDir, "known_hosts")
	if err := os.WriteFile(knownHosts, nil, 0o600); err != nil {
		os.RemoveAll(workDir)
		return nil, err
	}

	cfg := migrate.RemoteConfig{
		Host: req.Host, Port: req.Port, User: req.User,
		KeyPath: keyPath, KnownHostsPath: knownHosts,
		Sudo: req.User != "root",
	}
	client := migrate.Client{Cfg: cfg}

	if err := client.TestConnection(ctx); err != nil {
		os.RemoveAll(workDir)
		return nil, fmt.Errorf("connecting to %s@%s: %w", req.User, req.Host, err)
	}
	if err := client.EnsureRemoteTool(ctx, "rsync", "rsync"); err != nil {
		os.RemoveAll(workDir)
		return nil, err
	}

	vhosts, err := client.DiscoverVHosts(ctx)
	if err != nil {
		os.RemoveAll(workDir)
		return nil, err
	}

	// Local site registry, to flag a domain already served here — a fresh
	// Ctx purely to read state, discarded immediately after.
	localCtx, ctxErr := newCtx(ctx, false)

	discovered := map[string]discoveredSite{}
	pub := make([]map[string]any, 0, len(vhosts))
	for _, v := range vhosts {
		d := discoveredSite{NginxVHost: v}
		if v.Root != "" {
			if info, ok := client.ReadWPConfig(ctx, v.Root); ok {
				d.WPConfigInfo = info
				d.WordPress = true
			}
		}
		if ctxErr == nil {
			d.AlreadyLocal = localCtx.State.DomainTaken(v.Domain, "")
		}
		discovered[v.Domain] = d
		pub = append(pub, d.public())
	}

	m.mu.Lock()
	m.workDir = workDir
	m.remote = cfg
	m.discovered = discovered
	m.mu.Unlock()

	// The previous session's key material, if any, is no longer needed —
	// this discovery superseded it.
	if previousWorkDir != "" {
		os.RemoveAll(previousWorkDir)
	}

	return pub, nil
}

// MigrateOptions are the per-run choices that apply to every selected site
// — the same TLS choice AddSite offers, since a migrated site is a normal
// site from this point forward.
type MigrateOptions struct {
	TLS        bool
	SelfSigned bool
}

// StartMigration kicks off a background job migrating every given domain,
// sequentially — "do one by one" from the feature request, so one site's
// database is never competing with another's for this machine's I/O and a
// failure is always attributable to exactly one site, not an ambiguous mix.
func (m *MigrateManager) StartMigration(domains []string, opts MigrateOptions) error {
	m.mu.Lock()
	if m.job != nil && m.job.isRunning() {
		m.mu.Unlock()
		return fmt.Errorf("a migration is already running")
	}
	if m.discovered == nil {
		m.mu.Unlock()
		return fmt.Errorf("run discovery first")
	}
	var sites []discoveredSite
	for _, d := range domains {
		ds, ok := m.discovered[d]
		if !ok {
			m.mu.Unlock()
			return fmt.Errorf("%s was not part of the last discovery — discover again and reselect", d)
		}
		if !ds.WordPress {
			m.mu.Unlock()
			return fmt.Errorf("%s has no readable wp-config.php and cannot be migrated by this tool", d)
		}
		if ds.AlreadyLocal {
			m.mu.Unlock()
			return fmt.Errorf("%s already exists on this machine", d)
		}
		sites = append(sites, ds)
	}
	if len(sites) == 0 {
		m.mu.Unlock()
		return fmt.Errorf("select at least one site to migrate")
	}
	remote := m.remote
	workDir := m.workDir
	job := newMigrateJob(domains)
	m.job = job
	m.mu.Unlock()

	// A context independent of the HTTP request that started this — the
	// request returns as soon as the job is scheduled; the migration keeps
	// running against the operator's own cancel button, not whatever
	// timeout the browser's fetch() call happened to have.
	jobCtx, cancel := context.WithCancel(context.Background())
	job.cancel = cancel

	go func() {
		defer func() {
			job.finish()
			os.RemoveAll(workDir)
		}()
		m.run(jobCtx, remote, sites, opts, job)
	}()
	return nil
}

// Cancel stops the running job, if any. Whatever site is mid-transfer when
// this is called is left exactly as MigrateAbortSite would leave any other
// failed site — cleaned up, not half-registered.
func (m *MigrateManager) Cancel() {
	m.mu.Lock()
	job := m.job
	m.mu.Unlock()
	if job != nil && job.cancel != nil {
		job.cancel()
	}
}

// Status returns the current (or, once finished, the last) job's progress
// for the frontend to poll — nil when nothing has ever run.
func (m *MigrateManager) Status() map[string]any {
	m.mu.Lock()
	job := m.job
	m.mu.Unlock()
	if job == nil {
		return nil
	}
	return job.snapshot()
}

// run drives every selected site's migration in turn, using one
// provision.Ctx for the whole job — the same "one Ctx per run" shape every
// CLI command already uses, just held across a background goroutine
// instead of a single function call.
func (m *MigrateManager) run(ctx context.Context, remote migrate.RemoteConfig, sites []discoveredSite, opts MigrateOptions, job *migrateJob) {
	pc, err := provision.New(ctx, provision.Options{})
	if err != nil {
		job.logf("could not start: %v", err)
		for _, ds := range sites {
			job.fail(ds.Domain, err)
		}
		return
	}

	stagingDir, err := os.MkdirTemp("", "ngxsetup-migrate-staging-*")
	if err != nil {
		job.logf("could not prepare a staging directory: %v", err)
		for _, ds := range sites {
			job.fail(ds.Domain, err)
		}
		return
	}
	defer os.RemoveAll(stagingDir)

	for _, ds := range sites {
		if ctx.Err() != nil {
			job.fail(ds.Domain, fmt.Errorf("migration cancelled"))
			continue
		}
		job.setState(ds.Domain, "running")
		job.logf("[%s] starting migration", ds.Domain)

		remoteSite := provision.DiscoveredSite{Domain: ds.Domain, Aliases: ds.Aliases, Root: ds.Root, DBInfo: ds.WPConfigInfo}
		siteReq := provision.MigrateSiteRequest{Domain: ds.Domain, Aliases: ds.Aliases, TLS: opts.TLS, SelfSigned: opts.SelfSigned}
		if err := pc.MigrateRunOne(ctx, remote, remoteSite, siteReq, stagingDir, jobProgress{domain: ds.Domain, job: job}); err != nil {
			job.fail(ds.Domain, err)
			logx.Warn("migrating %s failed: %v", ds.Domain, err)
			continue
		}
		job.succeed(ds.Domain)
	}
}

// jobProgress adapts one site's slice of a migrateJob to
// provision.MigrateProgress, so MigrateRunOne's single pipeline can drive
// either this job-polling status or the CLI's direct terminal output.
type jobProgress struct {
	domain string
	job    *migrateJob
}

func (p jobProgress) Step(step string) { p.job.setStep(p.domain, step) }
func (p jobProgress) Percent(pct int)  { p.job.setPercent(p.domain, pct) }
func (p jobProgress) Log(line string)  { p.job.logf("[%s] %s", p.domain, line) }
