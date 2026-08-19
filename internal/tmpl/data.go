package tmpl

import "ngxsetup/internal/tuning"

// The types below are the contracts between the tuning engine and the config
// files. Templates render with missingkey=error, so a field renamed here fails
// the render rather than writing "<no value>" into a live configuration.

// Global is the context for server-wide nginx configuration.
type Global struct {
	Plan tuning.Plan

	// ACMERoot is the shared webroot certificate challenges are served from,
	// so a domain can be validated before its own vhost exists.
	ACMERoot string
	// Resolvers is the space-separated resolver list used for OCSP stapling.
	Resolvers string
	// TrustedNetworks are exempt from rate limiting: monitoring, office ranges.
	TrustedNetworks []string
	// RejectHandshake enables `ssl_reject_handshake`, available from nginx
	// 1.19.4. On older builds the catch-all TLS server is omitted instead.
	RejectHandshake bool
	// CacheVaryDevice splits the cache by device class. Off unless the site
	// genuinely serves different markup to mobile clients.
	CacheVaryDevice bool
}

// Headers is the context for the security header snippet, rendered once per
// variant because HSTS must only appear on sites with a real certificate.
type Headers struct {
	HSTS                  bool
	ContentSecurityPolicy string
}

// Site is the context for one virtual host.
type Site struct {
	Plan tuning.Plan

	Slug        string
	Domain      string
	PrimaryName string // the host redirects and canonical URLs point at
	ServerNames string // space-separated server_name list
	Root        string
	SocketPath  string

	TLS       bool
	CertPath  string
	KeyPath   string
	ChainPath string
	// OCSPStapling enables ssl_stapling for this site. Only meaningful for a
	// CA-issued certificate — see 20-ssl.conf.tmpl and setLetsEncryptCertPaths
	// for why a self-signed certificate must never set this.
	OCSPStapling bool

	// HTTP2Style is "listen" for nginx below 1.25.1, where http2 is a listen
	// parameter, and "directive" for newer builds where it is its own
	// directive. Emitting the wrong one is a config-test failure.
	HTTP2Style string

	HeadersSnippet string // "security-headers.conf" or the HSTS variant
	OverridePath   string
	ACMERoot       string

	CacheEnabled      bool
	BlockXMLRPC       bool
	BlockBadAgents    bool
	BlockBadReferrers bool
	BlockScraperBots  bool
	AdminAllowList    []string
}

// Pool is the context for a per-site PHP-FPM pool.
type Pool struct {
	Plan tuning.Plan

	Slug            string
	Domain          string
	User            string
	Group           string
	SocketPath      string
	Root            string
	TmpDir          string
	SessionDir      string
	TLS             bool
	StrictFunctions bool
}

// PHPIni is the context for the php.ini drop-in, rendered once per SAPI.
type PHPIni struct {
	Plan tuning.Plan

	SAPI             string // "fpm" or "cli"
	Timezone         string
	OpcacheFileCache string
}

// DB is the context for the database drop-in.
type DB struct {
	Plan tuning.Plan
}

// System is the context for sysctl and limits files.
type System struct {
	Plan tuning.Plan
}

// Unit is the context for a systemd drop-in.
type Unit struct {
	Unit      string
	Nofile    int
	Restart   bool
	OOMAdjust string
}

// FPMService is the context for the per-site systemd template unit. One
// rendering serves every site — %i expands to the slug at instantiation —
// so these are host-wide values, not per-site ones.
type FPMService struct {
	PHPVersion   string
	FPMBinary    string
	FPMConfigDir string
	WebRoot      string
	PHPLogDir    string
	PHPSocketDir string
	RunDir       string
	DBUnit       string
}

// BorgBackup is the context for the scheduled borg backup service and timer.
type BorgBackup struct {
	DBUnit     string
	Prune      bool
	OnCalendar string
	// ExecPath is the ngxsetup binary the scheduled service calls back
	// into. Resolved from the running process (os.Executable, symlinks
	// followed) rather than assumed to be /usr/local/bin/ngxsetup — that
	// path is only current if `setup` was the last thing to update it,
	// and a binary replaced by hand afterward (exactly what happens
	// while developing this tool, and just as plausibly during a manual
	// upgrade) would otherwise leave the timer silently running a stale
	// copy indefinitely. Confirmed live: after redeploying a newer build
	// to a different path, /usr/local/bin/ngxsetup was hours stale and
	// had no `borg` subcommand at all.
	ExecPath string
}

// FPMGlobal is the context for one site's PHP-FPM master configuration.
type FPMGlobal struct {
	Domain   string
	PidFile  string
	ErrorLog string
	PoolFile string
}

// FPMLimits is the context for a per-instance resource-limit drop-in.
type FPMLimits struct {
	Domain      string
	MemoryMaxMB int
	// CPUQuotaPercent is 0 to omit the cap entirely. 100 means one full core.
	CPUQuotaPercent int
}

// Fail2ban is the context for the jail configuration.
type Fail2ban struct {
	Backend   string
	IgnoreIPs string
}

// Logrotate is the context for log rotation.
type Logrotate struct {
	KeepDays   int
	PHPVersion string
}

// Salt is one WordPress authentication key.
type Salt struct {
	Name  string
	Value string
}

// WPConfig is the context for a generated wp-config.php.
type WPConfig struct {
	Domain      string
	DBName      string
	DBUser      string
	DBPassword  string
	TablePrefix string
	Salts       []Salt

	SiteURL          string
	MemoryLimit      string
	AdminMemoryLimit string
	DisableCron      bool
	// AllowFileMods permits WordPress to write to its own code directories.
	// Off by default: it is the difference between an upload bug and a
	// persistent backdoor.
	AllowFileMods bool
}

// PhpMyAdmin is the context for the dedicated phpMyAdmin server block.
type PhpMyAdmin struct {
	Port         int
	TLS          bool
	CertPath     string
	KeyPath      string
	AllowList    []string
	HtpasswdPath string
	SocketPath   string
}
