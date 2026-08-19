package migrate

import (
	"strings"
	"testing"
)

const sampleSitesEnabled = `===NGXFILE:/etc/nginx/sites-enabled/example.com.conf===
server {
    listen 80;
    server_name example.com www.example.com;
    root /var/www/example.com/public;
    location / {
        try_files $uri $uri/ /index.php?$args;
    }
}
===NGXFILE_END===
===NGXFILE:/etc/nginx/sites-enabled/second.example.conf===
server {
    listen 80 default_server;
    server_name _;
    return 444;
}
server {
    listen 443 ssl;
    server_name second.example;
    root /srv/www/second;
}
===NGXFILE_END===
`

func TestParseSitesEnabled(t *testing.T) {
	vhosts := ParseSitesEnabled(sampleSitesEnabled)
	if len(vhosts) != 2 {
		t.Fatalf("got %d vhosts, want 2: %+v", len(vhosts), vhosts)
	}
	if vhosts[0].Domain != "example.com" {
		t.Errorf("vhosts[0].Domain = %q, want example.com", vhosts[0].Domain)
	}
	if len(vhosts[0].Aliases) != 1 || vhosts[0].Aliases[0] != "www.example.com" {
		t.Errorf("vhosts[0].Aliases = %v, want [www.example.com]", vhosts[0].Aliases)
	}
	if vhosts[0].Root != "/var/www/example.com/public" {
		t.Errorf("vhosts[0].Root = %q", vhosts[0].Root)
	}
	// The default_server catch-all (server_name "_") must not appear as its
	// own vhost.
	if vhosts[1].Domain != "second.example" {
		t.Errorf("vhosts[1].Domain = %q, want second.example", vhosts[1].Domain)
	}
	if vhosts[1].Root != "/srv/www/second" {
		t.Errorf("vhosts[1].Root = %q", vhosts[1].Root)
	}
}

func TestParseSitesEnabledPrefersBlockWithRoot(t *testing.T) {
	raw := `===NGXFILE:/etc/nginx/sites-enabled/a.conf===
server {
    listen 80;
    server_name a.example;
    return 301 https://$host$request_uri;
}
server {
    listen 443 ssl;
    server_name a.example;
    root /var/www/a.example/public;
}
===NGXFILE_END===
`
	vhosts := ParseSitesEnabled(raw)
	if len(vhosts) != 1 {
		t.Fatalf("got %d vhosts, want 1: %+v", len(vhosts), vhosts)
	}
	if vhosts[0].Root != "/var/www/a.example/public" {
		t.Errorf("Root = %q, want the HTTPS block's root, not the redirect block's empty one", vhosts[0].Root)
	}
}

// TestParseSitesEnabledIgnoresNestedLocationRoot is a regression test for a
// real bug found live: an ngxsetup-managed vhost's HTTP block (the
// redirect-to-HTTPS one) has an ACME-challenge location with its own root,
// and no server-level root of its own at all — before serverLevelOnly
// existed, that nested root got recorded as the site's document root, and
// because it was non-empty, the later HTTPS block's real, correct root was
// never allowed to overwrite it.
func TestParseSitesEnabledIgnoresNestedLocationRoot(t *testing.T) {
	raw := `===NGXFILE:/etc/nginx/sites-enabled/real.conf===
server {
    listen 80;
    server_name real.example;
    location ^~ /.well-known/acme-challenge/ {
        root /var/www/_acme;
        default_type "text/plain";
    }
    location / {
        return 301 https://real.example$request_uri;
    }
}
server {
    listen 443 ssl;
    server_name real.example;
    root /var/www/real-example/public;
    location ^~ /.well-known/acme-challenge/ {
        root /var/www/_acme;
    }
    location ~ \.php$ {
        fastcgi_pass unix:/run/php/real-example.sock;
    }
}
===NGXFILE_END===
`
	vhosts := ParseSitesEnabled(raw)
	if len(vhosts) != 1 {
		t.Fatalf("got %d vhosts, want 1: %+v", len(vhosts), vhosts)
	}
	if vhosts[0].Root != "/var/www/real-example/public" {
		t.Errorf("Root = %q, want the site's real document root, not the ACME-challenge location's /var/www/_acme", vhosts[0].Root)
	}
}

func TestServerLevelOnlyStripsNestedBlocksButKeepsTopLevel(t *testing.T) {
	block := `{
    server_name example.com;
    root /var/www/example/public;
    location /x/ {
        root /var/www/_wrong;
        if ($a) {
            return 404;
        }
    }
    index index.php;
}`
	top := serverLevelOnly(block)
	if !strings.Contains(top, "server_name example.com;") {
		t.Errorf("server-level server_name missing from stripped output: %q", top)
	}
	if !strings.Contains(top, "root /var/www/example/public;") {
		t.Errorf("server-level root missing from stripped output: %q", top)
	}
	if strings.Contains(top, "_wrong") {
		t.Errorf("nested location's root leaked into server-level output: %q", top)
	}
	if strings.Contains(top, "return 404") {
		t.Errorf("doubly-nested if block content leaked into server-level output: %q", top)
	}
	if !strings.Contains(top, "index index.php;") {
		t.Errorf("server-level directive after the nested block missing: %q", top)
	}
}

func TestParseSitesEnabledNoServerBlocks(t *testing.T) {
	if vhosts := ParseSitesEnabled(""); len(vhosts) != 0 {
		t.Errorf("empty input produced %d vhosts, want 0", len(vhosts))
	}
	if vhosts := ParseSitesEnabled("not an nginx config at all"); len(vhosts) != 0 {
		t.Errorf("garbage input produced %d vhosts, want 0", len(vhosts))
	}
}

func TestParseSitesEnabledUnbalancedBracesSkipped(t *testing.T) {
	raw := `===NGXFILE:/etc/nginx/sites-enabled/broken.conf===
server {
    server_name broken.example;
    root /var/www/broken;
===NGXFILE_END===
`
	// The opening brace is never closed; this must not panic or hang, and
	// produces no vhost since there is nothing well-formed to extract.
	if vhosts := ParseSitesEnabled(raw); len(vhosts) != 0 {
		t.Errorf("unbalanced input produced %d vhosts, want 0: %+v", len(vhosts), vhosts)
	}
}

func TestParseWPConfigStandardFormatting(t *testing.T) {
	raw := `<?php
define( 'DB_NAME', 'wp_prod' );
define( 'DB_USER', 'wp_prod_user' );
define( 'DB_PASSWORD', 'hunter2!' );
define( 'DB_HOST', 'localhost' );
$table_prefix = 'wp_';
`
	info, ok := ParseWPConfig(raw)
	if !ok {
		t.Fatal("ParseWPConfig reported not-a-WordPress-config for a well-formed file")
	}
	want := WPConfigInfo{DBName: "wp_prod", DBUser: "wp_prod_user", DBPassword: "hunter2!", DBHost: "localhost", TablePrefix: "wp_"}
	if info != want {
		t.Errorf("ParseWPConfig = %+v, want %+v", info, want)
	}
}

func TestParseWPConfigAlternateQuotingAndSpacing(t *testing.T) {
	raw := `<?php
define("DB_NAME","altdb");
define(  "DB_USER" , "altuser"  );
define('DB_PASSWORD','p@ss');
$table_prefix  =  "wp7x9_";
`
	info, ok := ParseWPConfig(raw)
	if !ok {
		t.Fatal("ParseWPConfig failed on differently-formatted but valid input")
	}
	if info.DBName != "altdb" || info.DBUser != "altuser" || info.DBPassword != "p@ss" {
		t.Errorf("info = %+v", info)
	}
	if info.TablePrefix != "wp7x9_" {
		t.Errorf("TablePrefix = %q, want wp7x9_", info.TablePrefix)
	}
	if info.DBHost != "localhost" {
		t.Errorf("DBHost = %q, want localhost (default when DB_HOST is absent)", info.DBHost)
	}
}

func TestParseWPConfigNotWordPress(t *testing.T) {
	if _, ok := ParseWPConfig("<?php\necho 'hello';\n"); ok {
		t.Error("ParseWPConfig accepted a file with no DB_NAME/DB_USER as a WordPress config")
	}
}

func TestParseWPConfigMissingTablePrefixDefaultsToWP(t *testing.T) {
	raw := `<?php
define('DB_NAME','x');
define('DB_USER','y');
`
	info, ok := ParseWPConfig(raw)
	if !ok {
		t.Fatal("expected a match")
	}
	if info.TablePrefix != "wp_" {
		t.Errorf("TablePrefix = %q, want the wp_ default when $table_prefix is absent", info.TablePrefix)
	}
}

func TestShellQuoteHandlesEmbeddedSingleQuotes(t *testing.T) {
	cases := map[string]string{
		"plain": `'plain'`,
		"it's":  `'it'\''s'`,
		"":      `''`,
		"a'b'c": `'a'\''b'\''c'`,
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestExtractServerBlocksHandlesNestedBraces(t *testing.T) {
	raw := `server {
    server_name nested.example;
    root /var/www/nested;
    location ~ \.php$ {
        if ($request_method = POST) {
            return 405;
        }
    }
}`
	blocks := extractServerBlocks(raw)
	if len(blocks) != 1 {
		t.Fatalf("got %d blocks, want 1", len(blocks))
	}
	if !strings.HasPrefix(strings.TrimSpace(blocks[0]), "{") {
		t.Errorf("block does not start at the opening brace: %q", blocks[0][:20])
	}
	if !strings.HasSuffix(strings.TrimSpace(blocks[0]), "}") {
		t.Errorf("block does not end at the matching closing brace")
	}
}
