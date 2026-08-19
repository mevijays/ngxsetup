package provision

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"ngxsetup/internal/config"
	"ngxsetup/internal/facts"
	"ngxsetup/internal/logx"
	"ngxsetup/internal/render"
	"ngxsetup/internal/state"
	"ngxsetup/internal/system"
	"ngxsetup/internal/tmpl"
	"ngxsetup/internal/tuning"
)

// tmplIdent is the marker every managed file carries.
const tmplIdent = tmpl.Ident

// testCtx builds a context rooted in a temporary directory, so the full apply
// pipeline can run without touching the machine running the tests.
func testCtx(t *testing.T) *Ctx {
	t.Helper()
	logx.SetOutput(&strings.Builder{}, &strings.Builder{})

	root := t.TempDir()
	f := facts.Facts{
		OS:           facts.OS{ID: "ubuntu", VersionID: "24.04", PrettyName: "Ubuntu 24.04 LTS"},
		CPUCores:     4,
		MemTotalMB:   8192,
		SwapMB:       2048,
		PHPVersion:   "8.3",
		NginxVersion: "1.24.0",
		DBFlavor:     facts.DBMariaDB,
		DBVersion:    "10.11.8",
		Storage:      facts.Storage{Known: true, TotalMB: 50000, FreeMB: 40000},
	}
	st, err := state.Load(filepath.Join(root, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()

	return &Ctx{
		Context:    context.Background(),
		Facts:      f,
		Plan:       tuning.Compute(f, tuning.Options{}),
		Config:     cfg,
		State:      st,
		Runner:     system.Runner{DryRun: true},
		PHPVersion: "8.3",
		FPMUnit:    "php8.3-fpm.service",
		DBUnit:     "mariadb.service",
		Writer: &render.Writer{
			Root:      root,
			BackupDir: filepath.Join(root, "backups"),
		},
	}
}

func exists(t *testing.T, c *Ctx, p string) bool {
	t.Helper()
	_, err := os.Stat(c.Path(p))
	return err == nil
}

func read(t *testing.T, c *Ctx, p string) string {
	t.Helper()
	b, err := os.ReadFile(c.Path(p))
	if err != nil {
		t.Fatalf("reading %s: %v", p, err)
	}
	return string(b)
}

func TestApplyWritesTheWholeStack(t *testing.T) {
	c := testCtx(t)

	if err := c.ApplyNginx(); err != nil {
		t.Fatalf("ApplyNginx: %v", err)
	}
	if err := c.ApplyPHP(); err != nil {
		t.Fatalf("ApplyPHP: %v", err)
	}
	if err := c.ApplyDB(); err != nil {
		t.Fatalf("ApplyDB: %v", err)
	}
	if err := c.ApplySystem(); err != nil {
		t.Fatalf("ApplySystem: %v", err)
	}

	want := []string{
		"/etc/nginx/nginx.conf",
		"/etc/nginx/conf.d/00-ngxsetup-core.conf",
		"/etc/nginx/conf.d/10-ngxsetup-compression.conf",
		"/etc/nginx/conf.d/20-ngxsetup-ssl.conf",
		"/etc/nginx/conf.d/30-ngxsetup-cache.conf",
		"/etc/nginx/conf.d/40-ngxsetup-limits.conf",
		"/etc/nginx/snippets/ngxsetup/hardening.conf",
		"/etc/nginx/snippets/ngxsetup/fastcgi-php.conf",
		"/etc/nginx/snippets/ngxsetup/security-headers.conf",
		"/etc/nginx/snippets/ngxsetup/security-headers-hsts.conf",
		"/etc/php/8.3/fpm/conf.d/99-ngxsetup.ini",
		"/etc/php/8.3/cli/conf.d/99-ngxsetup.ini",
		"/etc/mysql/mariadb.conf.d/99-ngxsetup.cnf",
		"/etc/sysctl.d/60-ngxsetup.conf",
		"/etc/security/limits.d/60-ngxsetup.conf",
		"/etc/logrotate.d/ngxsetup",
	}
	for _, p := range want {
		if !exists(t, c, p) {
			t.Errorf("missing %s", p)
		}
	}
}

// The whole point of a managed configuration: running it again changes nothing.
func TestApplyIsIdempotent(t *testing.T) {
	c := testCtx(t)
	for _, fn := range []func() error{c.ApplyNginx, c.ApplyPHP, c.ApplyDB, c.ApplySystem} {
		if err := fn(); err != nil {
			t.Fatal(err)
		}
	}
	first := c.Writer.Changed()
	if first == 0 {
		t.Fatal("first apply changed nothing")
	}
	c.Writer.Commit()

	for _, fn := range []func() error{c.ApplyNginx, c.ApplyPHP, c.ApplyDB, c.ApplySystem} {
		if err := fn(); err != nil {
			t.Fatal(err)
		}
	}
	if second := c.Writer.Changed(); second != 0 {
		t.Errorf("second apply rewrote %d files; it should have rewritten none", second)
	}
}

// The values the tuner computed must be the values that reach the files.
func TestPlanValuesReachTheConfigFiles(t *testing.T) {
	c := testCtx(t)
	if err := c.ApplyNginx(); err != nil {
		t.Fatal(err)
	}
	if err := c.ApplyPHP(); err != nil {
		t.Fatal(err)
	}
	if err := c.ApplyDB(); err != nil {
		t.Fatal(err)
	}

	nginxConf := read(t, c, "/etc/nginx/nginx.conf")
	if !strings.Contains(nginxConf, "worker_connections "+itoa(c.Plan.Nginx.WorkerConnections)) {
		t.Error("worker_connections from the plan did not reach nginx.conf")
	}
	if !strings.Contains(nginxConf, "worker_rlimit_nofile "+itoa(c.Plan.Nginx.WorkerRlimitNofile)) {
		t.Error("worker_rlimit_nofile did not reach nginx.conf")
	}

	phpIni := read(t, c, "/etc/php/8.3/fpm/conf.d/99-ngxsetup.ini")
	if !strings.Contains(phpIni, "opcache.memory_consumption = "+itoa(c.Plan.OPcache.MemoryMB)) {
		t.Error("opcache size did not reach the php ini")
	}

	dbCnf := read(t, c, "/etc/mysql/mariadb.conf.d/99-ngxsetup.cnf")
	if !strings.Contains(dbCnf, "max_connections   = "+itoa(c.Plan.DB.MaxConnections)) &&
		!strings.Contains(dbCnf, "max_connections = "+itoa(c.Plan.DB.MaxConnections)) {
		t.Errorf("max_connections did not reach the database config:\n%s", dbCnf)
	}
	if !strings.Contains(dbCnf, tuning.MemString(c.Plan.DB.BufferPoolMB)) {
		t.Error("buffer pool size did not reach the database config")
	}
}

// The CLI and the cache purger both depend on the nginx cache key format; a
// change to one without the other silently breaks per-site purging.
func TestCacheKeyFormatMatchesPurger(t *testing.T) {
	c := testCtx(t)
	if err := c.ApplyNginx(); err != nil {
		t.Fatal(err)
	}
	cacheConf := read(t, c, "/etc/nginx/conf.d/30-ngxsetup-cache.conf")

	const wantKey = `fastcgi_cache_key "$scheme$request_method$host$uri$ngx_cache_args"`
	if !strings.Contains(cacheConf, wantKey) {
		t.Fatalf("cache key format changed; update cacheKeyPrefixes in cache.go to match\nlooking for: %s", wantKey)
	}
	// The purger builds prefixes from scheme+method; confirm they are still the
	// leading components of the key.
	for _, p := range cacheKeyPrefixes {
		if !strings.HasPrefix(p, "http") {
			t.Errorf("prefix %q does not start with a scheme", p)
		}
	}
}

func TestSiteConfigsAreWritten(t *testing.T) {
	c := testCtx(t)
	if err := c.ApplyNginx(); err != nil {
		t.Fatal(err)
	}
	rec := state.Site{
		Slug: "example-com", Domain: "example.com",
		Aliases: []string{"www.example.com"},
		Root:    c.DocumentRoot("example-com"), User: "web-example-com",
		SocketPath: c.SocketPath("example-com"), PHPVersion: "8.3",
		Enabled: true, CacheEnabled: true,
	}
	if err := c.writeSiteConfigs(rec); err != nil {
		t.Fatal(err)
	}

	conf := read(t, c, "/etc/nginx/sites-available/example-com.conf")
	if !strings.Contains(conf, "server_name example.com www.example.com") {
		t.Error("server_name does not include the alias")
	}
	if !strings.Contains(conf, "fastcgi_pass unix:"+rec.SocketPath) {
		t.Error("site is not pointed at its own PHP-FPM socket")
	}
	// Without TLS the site must not send HSTS: a browser that sees it would
	// refuse plain HTTP to the domain afterwards.
	if !strings.Contains(conf, "security-headers.conf") || strings.Contains(conf, "security-headers-hsts.conf") {
		t.Error("a non-TLS site must use the non-HSTS header snippet")
	}

	// writeSiteConfigs must never write into the distribution's own pool.d —
	// that is exactly the file a re-enabled shared php-fpm service would pick
	// up and run outside the site's jail. The pool is WriteFPMService's job,
	// tested separately below, into FPMPoolDir instead.
	if exists(t, c, "/etc/php/8.3/fpm/pool.d/example-com.conf") {
		t.Error("writeSiteConfigs must not write a pool into the distribution's pool.d")
	}

	if !exists(t, c, "/etc/nginx/sites-enabled/example-com.conf") {
		t.Error("site was not enabled")
	}
	if !exists(t, c, "/etc/nginx/sites-available/example-com.custom.conf") {
		t.Error("per-site override file was not created")
	}
}

// The isolated pool WriteFPMService writes — the one that actually runs a
// site — lives in FPMPoolDir, confined and running as the site's own user.
func TestWriteFPMServiceWritesAnIsolatedPool(t *testing.T) {
	c := testCtx(t)
	rec := state.Site{
		Slug: "example-com", Domain: "example.com",
		Root: c.DocumentRoot("example-com"), User: "web-example-com",
		SocketPath: c.SocketPath("example-com"), PHPVersion: "8.3",
	}
	if err := c.WriteFPMService(rec); err != nil {
		t.Fatal(err)
	}

	pool := read(t, c, "/etc/ngxsetup/fpm/pools/example-com.conf")
	if !strings.Contains(pool, "user  = web-example-com") {
		t.Errorf("pool does not run as the site user:\n%s", pool)
	}
	if !strings.Contains(pool, "open_basedir") {
		t.Error("pool has no open_basedir confinement")
	}
	if !strings.Contains(pool, "disable_functions") {
		t.Error("strict function list missing from the pool")
	}

	if exists(t, c, "/etc/php/8.3/fpm/pool.d/example-com.conf") {
		t.Error("WriteFPMService must not write into the distribution's pool.d either")
	}
}

// The override file is where an operator's own directives live; re-applying
// must never touch it.
func TestSiteOverrideFileSurvivesReapply(t *testing.T) {
	c := testCtx(t)
	if err := c.ApplyNginx(); err != nil {
		t.Fatal(err)
	}
	rec := state.Site{
		Slug: "example-com", Domain: "example.com",
		Root: c.DocumentRoot("example-com"), User: "web-example-com",
		SocketPath: c.SocketPath("example-com"), PHPVersion: "8.3", Enabled: true,
	}
	if err := c.writeSiteConfigs(rec); err != nil {
		t.Fatal(err)
	}

	overridePath := "/etc/nginx/sites-available/example-com.custom.conf"
	custom := "location /special { return 418; }\n"
	if err := os.WriteFile(c.Path(overridePath), []byte(custom), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := c.writeSiteConfigs(rec); err != nil {
		t.Fatal(err)
	}
	if got := read(t, c, overridePath); got != custom {
		t.Errorf("operator's override file was overwritten:\n%s", got)
	}
}

// A validation failure must leave the machine exactly as it was found.
func TestTransactionRollsBackOnValidationFailure(t *testing.T) {
	c := testCtx(t)
	if err := c.ApplyNginx(); err != nil {
		t.Fatal(err)
	}
	c.Writer.Commit()
	before := read(t, c, "/etc/nginx/nginx.conf")

	// Change the plan so the next apply produces different output, then fail
	// validation.
	c.Plan.Nginx.WorkerConnections = 12345
	err := c.Transaction("test", c.ApplyNginx, func() error {
		return os.ErrInvalid
	})
	if err == nil {
		t.Fatal("expected the transaction to fail")
	}
	if after := read(t, c, "/etc/nginx/nginx.conf"); after != before {
		t.Error("nginx.conf was not restored after the validation failure")
	}
}

// The shared www pool would undo per-site isolation, so applying PHP config
// must remove it.
func TestSharedWWWPoolIsRemoved(t *testing.T) {
	c := testCtx(t)
	poolPath := "/etc/php/8.3/fpm/pool.d/www.conf"
	if err := os.MkdirAll(filepath.Dir(c.Path(poolPath)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c.Path(poolPath), []byte("[www]\nuser = www-data\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := c.ApplyPHP(); err != nil {
		t.Fatal(err)
	}
	if exists(t, c, poolPath) {
		t.Error("the shared www pool is still present; every site could run as www-data")
	}
}

// A machine that was set up by an older, buggy build has a stray per-site
// pool in the distribution's pool.d alongside the real, isolated one in
// FPMPoolDir — the exact file a re-enabled shared service would run outside
// the site's jail. Applying PHP config again (`tune --apply`) must sweep it
// up for every registered site, not just newly created ones.
func TestApplyPHPRemovesStrayLegacyPerSitePools(t *testing.T) {
	c := testCtx(t)
	c.State.Upsert(state.Site{Slug: "legacy-com", Domain: "legacy.com", PHPVersion: "8.3"})

	legacy := "/etc/php/8.3/fpm/pool.d/legacy-com.conf"
	if err := os.MkdirAll(filepath.Dir(c.Path(legacy)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c.Path(legacy), []byte("[legacy-com]\nuser = web-legacy-com\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := c.ApplyPHP(); err != nil {
		t.Fatal(err)
	}
	if exists(t, c, legacy) {
		t.Error("a stray legacy per-site pool in the distribution's pool.d survived ApplyPHP")
	}
}

// Ubuntu's mariadb-server package does not create /var/log/mysql/ itself — it
// defaults to journald logging — so a config that turns on slow_query_log
// without also creating that directory starts successfully but silently
// disables slow query logging for the life of the process. Confirmed live.
func TestApplyDBCreatesLogDirectory(t *testing.T) {
	c := testCtx(t)
	if err := c.ApplyDB(); err != nil {
		t.Fatal(err)
	}
	if !exists(t, c, DBLogDir) {
		t.Fatalf("%s was not created; slow_query_log_file would fail silently", DBLogDir)
	}
}

// ValidateDB restarts the database — a real service interruption — to
// perform its check, unlike the cheap nginx -t / php-fpm -t syntax tests. It
// must only pay that cost when ApplyDB actually produced a new file, or a
// second, idempotent run of `setup`/`tune --apply` bounces the database every
// single time it runs, confirmed live: a re-run after only the site count and
// disk-free numbers drifted — neither of which affects the database plan —
// still restarted MariaDB unconditionally under the pre-fix logic.
//
// ValidateDB's restart itself shells out to systemctl and cannot be exercised
// portably in this suite; this test instead pins down the signal the fix
// relies on — that re-applying byte-identical database configuration reports
// zero writes on the second call, which is what lets ValidateDB tell "config
// changed" apart from "nothing to do."
func TestApplyDBIsIdempotentForValidateDBToTrust(t *testing.T) {
	c := testCtx(t)
	if err := c.ApplyDB(); err != nil {
		t.Fatal(err)
	}
	if c.Writer.Changed() == 0 {
		t.Fatal("first apply should have written the database config")
	}
	c.Writer.Commit()

	if err := c.ApplyDB(); err != nil {
		t.Fatal(err)
	}
	if got := c.Writer.Changed(); got != 0 {
		t.Errorf("re-applying identical database configuration reported %d changed file(s); "+
			"ValidateDB would read this as \"config changed\" and restart the database unnecessarily", got)
	}
}

// A fresh apt install of nginx leaves a stock nginx.conf on disk that carries
// no ngxsetup marker. The very first `setup` run must be able to take
// ownership of it rather than refusing with "not created by ngxsetup" — which
// is what happened against the real target before bootstrapOwnership existed:
// every fresh provision failed on its first write.
func TestFirstSetupTakesOwnershipOfStockConfig(t *testing.T) {
	c := testCtx(t)
	stockConf := "/etc/nginx/nginx.conf"
	if err := os.MkdirAll(filepath.Dir(c.Path(stockConf)), 0o755); err != nil {
		t.Fatal(err)
	}
	// A stock package file: plausible nginx.conf content, no marker.
	if err := os.WriteFile(c.Path(stockConf), []byte("user www-data;\nworker_processes auto;\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if c.State.SetupCompleted {
		t.Fatal("test fixture assumption broken: SetupCompleted should default false")
	}
	c.bootstrapOwnership()

	if err := c.ApplyNginx(); err != nil {
		t.Fatalf("first-time setup could not take ownership of the stock nginx.conf: %v", err)
	}
	if got := read(t, c, stockConf); !strings.Contains(got, tmplIdent) {
		t.Error("nginx.conf was not actually rewritten with our managed content")
	}
}

// Once setup has completed, the protection must come back: an operator who
// hand-edits nginx.conf after that point should not have it silently
// clobbered by a later `setup` or `tune` run.
func TestSecondSetupRespectsHandEdits(t *testing.T) {
	c := testCtx(t)
	c.State.SetupCompleted = true

	stockConf := "/etc/nginx/nginx.conf"
	if err := os.MkdirAll(filepath.Dir(c.Path(stockConf)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c.Path(stockConf), []byte("# hand-tuned by an operator\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	c.bootstrapOwnership()
	if c.Writer.Force {
		t.Fatal("bootstrapOwnership must not force writes once setup has already completed")
	}
	if err := c.ApplyNginx(); err == nil {
		t.Error("a second setup run overwrote a hand-edited nginx.conf without --force")
	}
}

// bootstrapOwnership must never downgrade an operator's explicit --force.
func TestBootstrapOwnershipDoesNotClearExplicitForce(t *testing.T) {
	c := testCtx(t)
	c.Writer.Force = true
	c.bootstrapOwnership()
	if !c.Writer.Force {
		t.Error("explicit --force was cleared")
	}
}

// writeSiteConfigs must derive OCSPStapling and ChainPath from the site
// record's actual certificate source, not just from TLS being on — a
// self-signed site is TLS-enabled too, but must never get ssl_stapling.
func TestOCSPStaplingWiredFromCertSource(t *testing.T) {
	c := testCtx(t)
	if err := c.ApplyNginx(); err != nil {
		t.Fatal(err)
	}

	selfSigned := state.Site{
		Slug: "self-example-com", Domain: "self.example.com",
		Root: c.DocumentRoot("self-example-com"), User: "web-self-example-com",
		SocketPath: c.SocketPath("self-example-com"), PHPVersion: "8.3", Enabled: true,
		TLS: true, CertSource: "self-signed",
		CertPath: "/etc/ngxsetup/certs/self-example-com/fullchain.pem",
	}
	if err := c.writeSiteConfigs(selfSigned); err != nil {
		t.Fatal(err)
	}
	body := read(t, c, "/etc/nginx/sites-available/self-example-com.conf")
	if strings.Contains(body, "ssl_stapling") {
		t.Error("self-signed site must not have ssl_stapling in its rendered config")
	}

	real := state.Site{
		Slug: "real-example-com", Domain: "real.example.com",
		Root: c.DocumentRoot("real-example-com"), User: "web-real-example-com",
		SocketPath: c.SocketPath("real-example-com"), PHPVersion: "8.3", Enabled: true,
		TLS: true, CertSource: "letsencrypt",
		CertPath:  "/etc/letsencrypt/live/real.example.com/fullchain.pem",
		ChainPath: "/etc/letsencrypt/live/real.example.com/chain.pem",
	}
	if err := c.writeSiteConfigs(real); err != nil {
		t.Fatal(err)
	}
	body = read(t, c, "/etc/nginx/sites-available/real-example-com.conf")
	if !regexp.MustCompile(`ssl_stapling\s+on;`).MatchString(body) {
		t.Errorf("a Let's Encrypt site should have ssl_stapling enabled:\n%s", body)
	}
	if !strings.Contains(body, "ssl_trusted_certificate "+real.ChainPath) {
		t.Error("the issuer chain path was not wired through to the rendered config")
	}
}

// The distribution's default site competes for default_server and would answer
// for unknown hosts.
func TestDistributionDefaultSiteIsRemoved(t *testing.T) {
	c := testCtx(t)
	def := "/etc/nginx/sites-enabled/default"
	if err := os.MkdirAll(filepath.Dir(c.Path(def)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c.Path(def), []byte("server { listen 80 default_server; }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := c.ApplyNginx(); err != nil {
		t.Fatal(err)
	}
	if exists(t, c, def) {
		t.Error("the distribution default site was left enabled")
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
