package tmpl

import (
	"regexp"
	"strings"
	"testing"

	"ngxsetup/internal/facts"
	"ngxsetup/internal/tuning"
)

func samplePlan() tuning.Plan {
	return tuning.Compute(facts.Facts{
		OS:         facts.OS{ID: "ubuntu", VersionID: "24.04"},
		CPUCores:   4,
		MemTotalMB: 8192,
		SwapMB:     2048,
		DBFlavor:   facts.DBMariaDB,
		DBVersion:  "10.11.8",
		PHPVersion: "8.3",
		Storage:    facts.Storage{Known: true, TotalMB: 50000, FreeMB: 40000},
	}, tuning.Options{})
}

// Every template must render. A template that fails to parse is a server that
// cannot be provisioned, and the failure would otherwise surface only on a
// customer's machine.
func TestAllTemplatesRender(t *testing.T) {
	plan := samplePlan()

	cases := []struct {
		name string
		data any
	}{
		{"nginx/nginx.conf.tmpl", Global{Plan: plan, ACMERoot: "/var/www/_acme", Resolvers: "127.0.0.53"}},
		{"nginx/conf.d/00-core.conf.tmpl", Global{Plan: plan, ACMERoot: "/var/www/_acme", RejectHandshake: true}},
		{"nginx/conf.d/10-compression.conf.tmpl", Global{Plan: plan}},
		{"nginx/conf.d/20-ssl.conf.tmpl", Global{Plan: plan, Resolvers: "127.0.0.53 1.1.1.1"}},
		{"nginx/conf.d/30-cache.conf.tmpl", Global{Plan: plan}},
		{"nginx/conf.d/40-limits.conf.tmpl", Global{Plan: plan, TrustedNetworks: []string{"10.0.0.0/8"}}},
		{"nginx/snippets/security-headers.conf.tmpl", Headers{HSTS: true}},
		{"nginx/snippets/hardening.conf.tmpl", Headers{}},
		{"nginx/snippets/fastcgi-php.conf.tmpl", Headers{}},
		{"nginx/sites/site.conf.tmpl", sampleSite(plan)},
		{"nginx/sites/phpmyadmin.conf.tmpl", PhpMyAdmin{
			Port: 8443, AllowList: []string{"203.0.113.4"},
			HtpasswdPath: "/etc/nginx/ngxsetup.htpasswd", SocketPath: "/run/php/tools.sock",
		}},
		{"php/pool.conf.tmpl", samplePool(plan)},
		{"php/php.ini.tmpl", PHPIni{Plan: plan, SAPI: "fpm", Timezone: "UTC", OpcacheFileCache: "/var/cache/php/opcache"}},
		{"db/server.cnf.tmpl", DB{Plan: plan}},
		{"system/sysctl.conf.tmpl", System{Plan: plan}},
		{"system/limits.conf.tmpl", System{Plan: plan}},
		{"system/unit-override.conf.tmpl", Unit{Unit: "nginx.service", Nofile: 65535, Restart: true}},
		{"system/logrotate.tmpl", Logrotate{KeepDays: 14, PHPVersion: "8.3"}},
		{"fail2ban/jail.local.tmpl", Fail2ban{Backend: "auto", IgnoreIPs: "127.0.0.1/8"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, err := Render(c.name, c.data)
			if err != nil {
				t.Fatalf("render failed: %v", err)
			}
			body := string(out)
			// A missing field renders as this string and produces a config
			// file that looks fine and breaks at service start.
			if strings.Contains(body, "<no value>") {
				t.Error("template produced <no value>; a field is missing from the data struct")
			}
			if !strings.Contains(body, Ident) {
				t.Error("rendered file is missing the managed marker, so it will never be updated again")
			}
			if strings.Contains(body, "{{") || strings.Contains(body, "}}") {
				t.Error("unexpanded template delimiters remain in the output")
			}
		})
	}
}

func sampleSite(plan tuning.Plan) Site {
	return Site{
		Plan: plan, Slug: "example-com", Domain: "example.com",
		PrimaryName: "example.com", ServerNames: "example.com www.example.com",
		Root: "/var/www/example-com/public", SocketPath: "/run/php/example-com.sock",
		TLS: true, CertPath: "/etc/letsencrypt/live/example.com/fullchain.pem",
		KeyPath:    "/etc/letsencrypt/live/example.com/privkey.pem",
		HTTP2Style: "listen", HeadersSnippet: "security-headers-hsts.conf",
		OverridePath: "/etc/nginx/sites-available/example-com.custom.conf",
		ACMERoot:     "/var/www/_acme",
		CacheEnabled: true, BlockXMLRPC: true, BlockBadAgents: true,
	}
}

func samplePool(plan tuning.Plan) Pool {
	return Pool{
		Plan: plan, Slug: "example-com", Domain: "example.com",
		User: "web-example-com", Group: "web-example-com",
		SocketPath: "/run/php/example-com.sock", Root: "/var/www/example-com/public",
		TmpDir: "/var/www/example-com/tmp", SessionDir: "/var/www/example-com/sessions",
		TLS: true, StrictFunctions: true,
	}
}

// nginx will not start with unbalanced braces, and a template conditional that
// opens a block it does not close is easy to write and hard to spot.
func TestNginxTemplatesHaveBalancedBraces(t *testing.T) {
	plan := samplePlan()
	cases := []struct {
		name string
		data any
	}{
		{"nginx/nginx.conf.tmpl", Global{Plan: plan, ACMERoot: "/var/www/_acme", Resolvers: "127.0.0.53"}},
		{"nginx/conf.d/00-core.conf.tmpl", Global{Plan: plan, ACMERoot: "/a", RejectHandshake: true}},
		{"nginx/conf.d/00-core.conf.tmpl", Global{Plan: plan, ACMERoot: "/a", RejectHandshake: false}},
		{"nginx/conf.d/30-cache.conf.tmpl", Global{Plan: plan, CacheVaryDevice: true}},
		{"nginx/conf.d/30-cache.conf.tmpl", Global{Plan: plan, CacheVaryDevice: false}},
		{"nginx/snippets/hardening.conf.tmpl", Headers{}},
	}
	for _, c := range cases {
		out, err := Render(c.name, c.data)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if d := braceBalance(string(out)); d != 0 {
			t.Errorf("%s: brace balance %+d", c.name, d)
		}
	}
}

// The site template has many conditional branches; every combination has to
// produce a syntactically valid server block.
func TestSiteTemplateBranchesAreValid(t *testing.T) {
	plan := samplePlan()
	for _, tls := range []bool{true, false} {
		for _, xmlrpc := range []bool{true, false} {
			for _, admin := range [][]string{nil, {"203.0.113.9"}} {
				for _, style := range []string{"listen", "directive"} {
					s := sampleSite(plan)
					s.TLS = tls
					s.BlockXMLRPC = xmlrpc
					s.AdminAllowList = admin
					s.HTTP2Style = style
					// Both flags round-trip through the same true/false space
					// as BlockXMLRPC above; one representative combination
					// alongside it is enough to catch an unbalanced brace
					// without squaring the size of this loop nest.
					s.BlockBadReferrers = xmlrpc
					s.BlockScraperBots = !xmlrpc
					out, err := Render("nginx/sites/site.conf.tmpl", s)
					if err != nil {
						t.Fatalf("tls=%v xmlrpc=%v admin=%v: %v", tls, xmlrpc, admin, err)
					}
					body := string(out)
					if d := braceBalance(body); d != 0 {
						t.Errorf("tls=%v xmlrpc=%v admin=%v style=%s: brace balance %+d\n%s",
							tls, xmlrpc, admin, style, d, body)
					}
					// Every server block needs a PHP handler, or the site
					// serves the source of index.php as a download.
					if !strings.Contains(body, "fastcgi_pass") {
						t.Error("site config has no fastcgi_pass")
					}
					if tls && !strings.Contains(body, "return 301 https://") {
						t.Error("TLS site is missing the plain-HTTP redirect")
					}
					if !tls && strings.Contains(body, "ssl_certificate ") {
						t.Error("non-TLS site should not reference a certificate")
					}
				}
			}
		}
	}
}

// Regression guards on the specific defects found in the legacy configuration.
func TestLegacyDefectsAreFixed(t *testing.T) {
	plan := samplePlan()

	fcgi, err := Render("nginx/snippets/fastcgi-php.conf.tmpl", Headers{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(directives(string(fcgi)), "$fastcgi_script_name$request_filename") {
		t.Error("SCRIPT_FILENAME still concatenates two incompatible path variables")
	}
	if !strings.Contains(string(fcgi), "try_files $fastcgi_script_name =404") {
		t.Error("missing the try_files guard that blocks path-info code execution")
	}

	// fastcgi_cache_use_stale has a narrower accepted token set than
	// proxy_cache_use_stale: nginx 1.24 rejects http_502 and http_504 outright
	// (confirmed against a real nginx -t, not from documentation), which fails
	// config validation on every machine rather than just some. This is a
	// standing regression guard, not a one-off fix.
	cache, err := Render("nginx/conf.d/30-cache.conf.tmpl", Global{Plan: plan})
	if err != nil {
		t.Fatal(err)
	}
	cacheBody := directives(string(cache))
	for _, forbidden := range []string{"http_502", "http_504"} {
		if strings.Contains(cacheBody, forbidden) {
			t.Errorf("fastcgi_cache_use_stale contains %q, which nginx rejects as an invalid value", forbidden)
		}
	}
	if !strings.Contains(cacheBody, "fastcgi_cache_use_stale") {
		t.Error("fastcgi_cache_use_stale directive is missing entirely")
	}

	ssl, _ := Render("nginx/conf.d/20-ssl.conf.tmpl", Global{Plan: plan, Resolvers: "127.0.0.53"})
	if strings.Contains(string(ssl), "TLSv1 ") || strings.Contains(string(ssl), "TLSv1.1") {
		t.Error("deprecated TLS versions are still enabled")
	}
	if !strings.Contains(string(ssl), "TLSv1.3") {
		t.Error("TLS 1.3 is not enabled")
	}
	// ssl_stapling must not be global: confirmed live that setting it here
	// makes nginx log "issuer certificate not found" on every single reload
	// for any self-signed site, since OCSP stapling has no CA to ask for a
	// self-signed certificate. It belongs per-site — see site.conf.tmpl.
	if strings.Contains(directives(string(ssl)), "ssl_stapling") {
		t.Error("ssl_stapling must not be set globally; it only applies to CA-issued certificates")
	}

	main, _ := Render("nginx/nginx.conf.tmpl", Global{Plan: plan, ACMERoot: "/a", Resolvers: "x"})
	if strings.Contains(string(main), "access_log off;") {
		t.Error("access logging is globally disabled again; fail2ban jails would silently never match")
	}

	site, _ := Render("nginx/sites/site.conf.tmpl", sampleSite(plan))
	if strings.Contains(string(site), "/usr/share/phpmyadmin") {
		t.Error("phpMyAdmin must not be mounted inside a site's server block")
	}

	hdr, _ := Render("nginx/snippets/security-headers.conf.tmpl", Headers{HSTS: true})
	for _, want := range []string{"X-Content-Type-Options", "X-Frame-Options", "Referrer-Policy", "Strict-Transport-Security"} {
		if !strings.Contains(string(hdr), want) {
			t.Errorf("missing security header %s", want)
		}
	}
	if strings.Contains(directives(string(hdr)), "X-Powered-By") {
		t.Error("the stack still advertises itself in a response header")
	}

	noHSTS, _ := Render("nginx/snippets/security-headers.conf.tmpl", Headers{HSTS: false})
	if strings.Contains(directives(string(noHSTS)), "Strict-Transport-Security") {
		t.Error("HSTS must never be sent by a site without a real certificate")
	}
}

// OCSP stapling must appear only for a real, CA-issued certificate, and must
// come with the issuer chain nginx needs to verify the stapled response —
// confirmed live: the ChainPath field existed but was never actually
// populated, so even gating ssl_stapling to Let's Encrypt sites alone would
// still have produced "issuer certificate not found" on every reload.
func TestOCSPStaplingOnlyForRealCertificates(t *testing.T) {
	plan := samplePlan()

	selfSigned := sampleSite(plan)
	selfSigned.OCSPStapling = false
	selfSigned.ChainPath = ""
	out, err := Render("nginx/sites/site.conf.tmpl", selfSigned)
	if err != nil {
		t.Fatal(err)
	}
	body := directives(string(out))
	if strings.Contains(body, "ssl_stapling") {
		t.Error("a self-signed site must never enable ssl_stapling")
	}

	real := sampleSite(plan)
	real.OCSPStapling = true
	real.ChainPath = "/etc/letsencrypt/live/example.com/chain.pem"
	out, err = Render("nginx/sites/site.conf.tmpl", real)
	if err != nil {
		t.Fatal(err)
	}
	body = directives(string(out))
	// The directives are column-aligned with padding spaces, so this checks
	// for the directive name followed eventually by "on;" rather than an
	// exact one-space substring.
	if !regexp.MustCompile(`ssl_stapling\s+on;`).MatchString(body) ||
		!regexp.MustCompile(`ssl_stapling_verify\s+on;`).MatchString(body) {
		t.Errorf("a real certificate should enable OCSP stapling:\n%s", body)
	}
	if !strings.Contains(body, "ssl_trusted_certificate "+real.ChainPath) {
		t.Error("ssl_stapling_verify needs ssl_trusted_certificate pointed at the issuer chain, or nginx cannot verify the staple")
	}
}

// MariaDB refuses to start when given MySQL-only directives. This was a latent
// failure in the previous tuner, which always wrote MySQL 8 syntax.
func TestDatabaseTemplateRespectsFlavor(t *testing.T) {
	mariaPlan := samplePlan()
	maria, err := Render("db/server.cnf.tmpl", DB{Plan: mariaPlan})
	if err != nil {
		t.Fatal(err)
	}
	body := directives(string(maria))
	for _, forbidden := range []string{
		"innodb_buffer_pool_instances",
		"default_authentication_plugin",
		"utf8mb4_0900_ai_ci",
		"innodb_dedicated_server",
		"binlog_expire_logs_seconds",
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("MariaDB config contains MySQL-only directive %q", forbidden)
		}
	}
	if !strings.Contains(body, "query_cache_size = 0") {
		t.Error("MariaDB query cache should be explicitly disabled")
	}

	mysqlPlan := tuning.Compute(facts.Facts{
		CPUCores: 8, MemTotalMB: 16384, DBFlavor: facts.DBMySQL, DBVersion: "8.4.0",
		Storage: facts.Storage{Known: true, FreeMB: 40000, TotalMB: 50000},
	}, tuning.Options{})
	mysql, err := Render("db/server.cnf.tmpl", DB{Plan: mysqlPlan})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(mysql), "innodb_buffer_pool_instances") {
		t.Error("MySQL config should set buffer pool instances")
	}
	if strings.Contains(string(mysql), "query_cache_size") {
		t.Error("MySQL 8 removed the query cache; setting it prevents startup")
	}
}

// Ubuntu's packaged mariadb.conf.d/50-server.cnf sets expire_logs_days
// unconditionally, which is meaningless without log_bin and produces
// "You need to use --log-bin to make --expire-logs-days work" on every
// single startup when binlog is off — confirmed live. skip-log-bin alone
// does not silence it; the value must be explicitly overridden to 0.
func TestBinlogOffSilencesDistroExpireLogsWarning(t *testing.T) {
	plan := samplePlan() // binlog off by default
	out, err := Render("db/server.cnf.tmpl", DB{Plan: plan})
	if err != nil {
		t.Fatal(err)
	}
	body := directives(string(out))
	if !strings.Contains(body, "skip-log-bin") {
		t.Fatal("expected skip-log-bin when binlog is off")
	}
	if !strings.Contains(body, "expire_logs_days = 0") {
		t.Error("expire_logs_days must be explicitly zeroed to override the distribution's own default and silence its startup warning")
	}
}

// [client] is read by every MySQL/MariaDB client-family program — mysql,
// mysqldump, mysqladmin, mysqlcheck, mysqlslap. Confirmed live: putting
// default-character-set there made mysqlslap refuse to start at all with
// "unknown variable 'default-character-set'", a benchmarking tool an
// operator running this stack would reasonably use. The setting belongs in
// [mysql], read only by the interactive CLI it is actually meant for.
func TestClientCharsetDoesNotBreakOtherClientTools(t *testing.T) {
	out, err := Render("db/server.cnf.tmpl", DB{Plan: samplePlan()})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(out), "\n")
	section := ""
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			section = trimmed
			continue
		}
		if section == "[client]" && strings.Contains(trimmed, "default-character-set") {
			t.Errorf("default-character-set is in [client], which mysqlslap cannot parse; move it to [mysql]")
		}
	}
	if !strings.Contains(string(out), "[mysql]") {
		t.Error("expected a [mysql] section carrying default-character-set")
	}
}

// Rendering must be deterministic or every apply shows a spurious diff and
// rollback comparisons become meaningless.
func TestRenderIsDeterministic(t *testing.T) {
	plan := samplePlan()
	a, _ := Render("nginx/nginx.conf.tmpl", Global{Plan: plan, ACMERoot: "/a", Resolvers: "x"})
	b, _ := Render("nginx/nginx.conf.tmpl", Global{Plan: plan, ACMERoot: "/a", Resolvers: "x"})
	if string(a) != string(b) {
		t.Error("rendering is not deterministic")
	}
}

// directives strips comment lines so assertions test what the service will
// actually parse, not the explanatory prose around it.
func directives(s string) string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") || strings.HasPrefix(t, ";") {
			continue
		}
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

func TestMissingFieldIsAnError(t *testing.T) {
	// A map without the key the template needs must fail the render rather
	// than silently producing an unusable config file.
	if _, err := Render("system/unit-override.conf.tmpl", map[string]any{"Unit": "nginx"}); err == nil {
		t.Error("expected an error for a missing template field")
	}
}

// Every template under php/ is parsed by PHP's INI parser, which only
// accepts `;` as a comment leader — a `#` first line is read as a malformed
// key=value entry and php-fpm refuses to start. This walks the embedded
// filesystem rather than listing files by hand, so a PHP template added
// later is covered automatically. It was written after exactly this bug
// shipped twice: once in a hand-written pool file, once in fpm-global.conf
// because the path predicate used Contains("/php/") against a name that has
// no leading slash.
func TestEveryPHPTemplateUsesINICommentSyntax(t *testing.T) {
	entries, err := files.ReadDir("files/php")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("no templates found under files/php; this guard would silently pass")
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := "php/" + e.Name()
		t.Run(name, func(t *testing.T) {
			header := headerFor(name)
			if strings.HasPrefix(header, "#") {
				t.Errorf("headerFor(%q) returns a `#` comment; PHP's INI parser rejects it", name)
			}
			if header != "" && !strings.HasPrefix(header, ";") {
				t.Errorf("headerFor(%q) = %q, want a `;`-style comment or none at all", name, header)
			}
		})
	}
}

func TestIsManaged(t *testing.T) {
	if !IsManaged([]byte(ManagedHeader + "\nfoo\n")) {
		t.Error("header should be recognised")
	}
	if IsManaged([]byte("# handwritten\nfoo\n")) {
		t.Error("unmarked content must not be treated as managed")
	}
	// The marker only counts near the top; a site override mentioning the tool
	// in a comment further down is still the operator's file.
	long := strings.Repeat("# operator notes\n", 200) + Ident
	if IsManaged([]byte(long)) {
		t.Error("marker found outside the header region should not count")
	}
}

func braceBalance(s string) int {
	depth := 0
	for _, line := range strings.Split(s, "\n") {
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		depth += strings.Count(line, "{") - strings.Count(line, "}")
	}
	return depth
}
