// Package config holds operator preferences that outlive a single command.
//
// The distinction from tuning.Options matters: everything here is a policy
// choice a human makes (which addresses may reach wp-admin, whether xmlrpc is
// blocked), while everything in tuning.Options is a sizing input derived from
// hardware. Policy is persisted so that `tune` and `site add` months apart
// still produce a consistent machine.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DefaultPath is where operator configuration lives.
const DefaultPath = "/etc/ngxsetup/config.json"

// PhpMyAdmin controls the optional database console.
type PhpMyAdmin struct {
	// Enabled is false by default. The previous stack exposed phpMyAdmin
	// unauthenticated on every virtual host; here it must be asked for.
	Enabled bool `json:"enabled"`
	// Port keeps it off 80 and 443 so it is never reachable by guessing a
	// path on a customer's domain.
	Port int `json:"port"`
	// AllowList is mandatory when enabled: an empty list means nobody, which
	// is a safer failure than everybody.
	AllowList []string `json:"allow_list"`
}

// Borg configures off-box backups to a BorgBackup repository. The
// passphrase is deliberately not a field here: config.json is itself a file
// an operator might reasonably back up or hand to someone else to read, and
// nothing that unlocks the backups themselves belongs in it. It lives
// separately at borg.PassphraseFile instead — see internal/borg.
type Borg struct {
	Enabled     bool   `json:"enabled"`
	Repo        string `json:"repo"`        // e.g. "/mnt/backup/ngxsetup" or "ssh://user@host:2222/./ngxsetup"
	Encryption  string `json:"encryption"`  // borg --encryption value, e.g. "repokey-blake2"
	Compression string `json:"compression"` // borg --compression value, e.g. "zstd", "lz4"
	// Schedule is a systemd OnCalendar expression ("" means no schedule
	// installed). Set through `borg schedule`, not `config set` directly,
	// since installing it also has to write and enable a systemd timer.
	Schedule string `json:"schedule,omitempty"`
	// Retention. Zero means "keep everything of that granularity" — borg's
	// own convention for an absent --keep-* flag.
	KeepDaily   int `json:"keep_daily"`
	KeepWeekly  int `json:"keep_weekly"`
	KeepMonthly int `json:"keep_monthly"`
}

// Config is the persisted operator configuration.
type Config struct {
	Profile  string `json:"profile"`
	Timezone string `json:"timezone"`

	// ACMEEmail receives Let's Encrypt expiry warnings. Required before any
	// certificate can be issued.
	ACMEEmail string `json:"acme_email"`

	// TrustedNetworks are exempt from rate limiting: monitoring probes, an
	// office range, an uptime checker.
	TrustedNetworks []string `json:"trusted_networks"`

	// AdminAllowList restricts /wp-admin and /wp-login.php by address for every
	// site. Empty means the login form is public but rate limited.
	AdminAllowList []string `json:"admin_allow_list"`

	BlockXMLRPC    bool `json:"block_xmlrpc"`
	BlockBadAgents bool `json:"block_bad_agents"`
	// BlockBadReferrers drops the well-known referrer-spam domains that have
	// polluted analytics since the Google Analytics "ghost spam" wave of the
	// mid-2010s and never really stopped. Low risk of ever matching a real
	// visitor, so this defaults on alongside BlockBadAgents.
	BlockBadReferrers bool `json:"block_bad_referrers"`
	// BlockScraperBots blocks AI-training crawlers (GPTBot, CCBot, ClaudeBot,
	// Bytespider...) and SEO/backlink-analysis crawlers (AhrefsBot, SemrushBot,
	// MJ12bot...). Off by default: unlike BlockBadAgents, nothing here is
	// attacking the site — it is a content-policy choice about who gets to
	// crawl it for free, and that choice belongs to the operator.
	BlockScraperBots bool `json:"block_scraper_bots"`
	CacheVaryDevice  bool `json:"cache_vary_device"`
	// StrictPHPFunctions disables process-spawning functions for web requests.
	StrictPHPFunctions bool `json:"strict_php_functions"`

	UploadMaxMB       int  `json:"upload_max_mb"`
	AvgPHPWorkerMB    int  `json:"avg_php_worker_mb"`
	ReserveMB         int  `json:"reserve_mb"`
	EnableBinlog      bool `json:"enable_binlog"`
	AggressiveOpcache bool `json:"aggressive_opcache"`

	// TrustCloudflare rewrites the client address from Cloudflare's headers,
	// but only for connections that genuinely originate from Cloudflare's
	// published ranges.
	TrustCloudflare bool `json:"trust_cloudflare"`

	LogKeepDays int        `json:"log_keep_days"`
	PhpMyAdmin  PhpMyAdmin `json:"phpmyadmin"`

	// SecurityYARARulesDir, if set, supplements ngxsetup's bundled malware
	// signature ruleset with a larger, separately maintained one — a local
	// checkout of an open-source ruleset project, kept current on its own
	// update schedule. Empty means the bundled rules only.
	SecurityYARARulesDir string `json:"security_yara_rules_dir,omitempty"`

	// GeoIPDatabasePath, if set, points at an operator-supplied MaxMind
	// GeoLite2-Country (or GeoIP2-Country) .mmdb file, enabling the web UI's
	// per-site visitor-geography chart. Deliberately never bundled: a real
	// GeoIP database runs several megabytes and goes stale, and licensing a
	// current one is the operator's call to make, not this tool's — the same
	// reasoning SecurityYARARulesDir already applies to a bigger malware
	// ruleset. Empty means the geography chart is simply not shown.
	GeoIPDatabasePath string `json:"geoip_database_path,omitempty"`

	Borg Borg `json:"borg"`

	// BreakGlassSSHKey, if set, is installed into /root/.ssh/authorized_keys
	// on every `setup` and `secure --apply` — one full public key line
	// (e.g. "ssh-ed25519 AAAA... name"), operator-supplied and
	// operator-owned, restored to root if every other access path fails.
	//
	// This is a *second*, optional key — distinct from and in addition to
	// provision.EmbeddedRecoveryKey, which every machine gets unconditionally
	// regardless of this field. That one key is fixed, built into this
	// binary the same way the previous version of this tool always
	// installed one; this field is for a further key an operator wants on
	// top of it — a different recovery path, a second person's key, a key
	// specific to one deployment. Empty (the default) installs nothing
	// beyond the built-in key, same as if this field did not exist.
	BreakGlassSSHKey string `json:"break_glass_ssh_key,omitempty"`

	path string
}

// Default returns the shipped configuration: secure first, with the
// performance-affecting choices left at values that suit an ordinary
// WordPress host.
func Default() *Config {
	return &Config{
		Profile:            "balanced",
		Timezone:           "UTC",
		BlockXMLRPC:        true,
		BlockBadAgents:     true,
		BlockBadReferrers:  true,
		BlockScraperBots:   false,
		StrictPHPFunctions: true,
		CacheVaryDevice:    false,
		UploadMaxMB:        128,
		LogKeepDays:        14,
		PhpMyAdmin:         PhpMyAdmin{Enabled: false, Port: 9443},
		path:               DefaultPath,
	}
}

// Load reads the configuration, falling back to defaults when absent.
func Load(path string) (*Config, error) {
	if path == "" {
		path = DefaultPath
	}
	c := Default()
	c.path = path

	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	if err := json.Unmarshal(data, c); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON: %w", path, err)
	}
	c.path = path
	return c, c.Validate()
}

// Save writes the configuration atomically.
func (c *Config) Save() error {
	if err := c.Validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
		return err
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, c.path)
}

// Validate rejects configurations that would produce a broken or unsafe
// machine, at load time rather than halfway through an apply.
func (c *Config) Validate() error {
	if c.PhpMyAdmin.Enabled {
		if len(c.PhpMyAdmin.AllowList) == 0 {
			return errors.New("phpmyadmin.enabled requires phpmyadmin.allow_list: exposing a database console to the whole internet is not a supported configuration")
		}
		if c.PhpMyAdmin.Port == 80 || c.PhpMyAdmin.Port == 443 {
			return errors.New("phpmyadmin.port must not be 80 or 443; it needs a port of its own")
		}
		if c.PhpMyAdmin.Port < 1024 || c.PhpMyAdmin.Port > 65535 {
			return fmt.Errorf("phpmyadmin.port %d is out of range", c.PhpMyAdmin.Port)
		}
	}
	for _, n := range append(append([]string{}, c.TrustedNetworks...), c.AdminAllowList...) {
		if err := validateCIDROrIP(n); err != nil {
			return err
		}
	}
	for _, n := range c.PhpMyAdmin.AllowList {
		if err := validateCIDROrIP(n); err != nil {
			return err
		}
	}
	if c.ACMEEmail != "" && !strings.Contains(c.ACMEEmail, "@") {
		return fmt.Errorf("acme_email %q is not an email address", c.ACMEEmail)
	}
	if c.UploadMaxMB < 0 || c.UploadMaxMB > 4096 {
		return fmt.Errorf("upload_max_mb %d is out of range", c.UploadMaxMB)
	}
	return nil
}

// Path reports where this configuration is stored.
func (c *Config) Path() string { return c.path }

// validateCIDROrIP guards values that are interpolated straight into nginx
// allow/deny directives.
func validateCIDROrIP(s string) error {
	s = strings.TrimSpace(s)
	if s == "" {
		return errors.New("empty address in allow list")
	}
	host, mask, hasMask := strings.Cut(s, "/")
	if hasMask {
		for _, r := range mask {
			if r < '0' || r > '9' {
				return fmt.Errorf("invalid network %q", s)
			}
		}
	}
	if net := parseIP(host); !net {
		return fmt.Errorf("invalid address %q in allow list (expected an IP address or CIDR range)", s)
	}
	return nil
}

func parseIP(s string) bool {
	if s == "" {
		return false
	}
	// Accept both families without pulling in net just for a syntax check:
	// anything outside this character set has no business in a config file.
	for _, r := range s {
		isHex := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
		if !isHex && r != '.' && r != ':' {
			return false
		}
	}
	return strings.ContainsAny(s, ".:")
}
