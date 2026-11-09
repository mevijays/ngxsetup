package migrate

import (
	"regexp"
	"strings"
)

// NginxVHost is one server block discovered on a remote host's
// /etc/nginx/sites-enabled, before anything about WordPress has been
// checked.
type NginxVHost struct {
	Domain     string   // first server_name token
	Aliases    []string // remaining server_name tokens
	Root       string   // this server block's document root, if any
	SourceFile string   // which sites-enabled file this came from, for diagnostics
}

var serverBlockStart = regexp.MustCompile(`(?m)(?:^|\s)server\s*\{`)

// extractServerBlocks pulls out every `server { ... }` block in a config
// file's content, matching nested braces (location blocks, if blocks) so a
// block is never cut short. Unbalanced input (a truncated file, a config
// this parser does not understand) is skipped rather than guessed at.
func extractServerBlocks(content string) []string {
	var blocks []string
	locs := serverBlockStart.FindAllStringIndex(content, -1)
	for _, loc := range locs {
		start := loc[1] - 1 // index of the opening '{'
		depth := 0
		end := -1
		for i := start; i < len(content); i++ {
			switch content[i] {
			case '{':
				depth++
			case '}':
				depth--
				if depth == 0 {
					end = i
				}
			}
			if end != -1 {
				break
			}
		}
		if end == -1 {
			continue
		}
		blocks = append(blocks, content[start:end+1])
	}
	return blocks
}

var (
	serverNameLine = regexp.MustCompile(`(?m)^\s*server_name\s+([^;]+);`)
	rootLine       = regexp.MustCompile(`(?m)^\s*root\s+([^;]+);`)
)

// serverLevelOnly strips every nested {...} block (location, if, map...)
// out of a server{} block's text, leaving only the directives that belong
// directly to the server block itself.
//
// This matters specifically for root: every ngxsetup-managed vhost (and
// plenty of others) has a `location ^~ /.well-known/acme-challenge/ { root
// /var/www/_acme; ... }` block for Let's Encrypt's HTTP-01 challenge —
// confirmed live against a real generated vhost, where that nested root
// was found and used instead of the site's actual document root, because a
// plain "first root in the block" search cannot tell a location's root
// from the server's own. server_name cannot legally appear inside a
// location in valid nginx syntax, but it is searched the same way here for
// consistency and because it costs nothing extra to be safe about it too.
func serverLevelOnly(block string) string {
	var out strings.Builder
	depth := 0
	for i := 0; i < len(block); i++ {
		c := block[i]
		beforeDepth := depth
		switch c {
		case '{':
			depth++
		case '}':
			depth--
		}
		if c != '{' && c != '}' && beforeDepth == 1 {
			out.WriteByte(c)
		}
	}
	return out.String()
}

// parseServerBlock reads server_name and root out of one server{} block's
// text. A block with no usable server_name (missing, or only the "_"
// catch-all default) is not a real vhost and is reported as such.
func parseServerBlock(block string) (domain string, aliases []string, root string, ok bool) {
	top := serverLevelOnly(block)
	m := serverNameLine.FindStringSubmatch(top)
	if m == nil {
		return "", nil, "", false
	}
	var names []string
	for _, n := range strings.Fields(m[1]) {
		n = strings.TrimSpace(n)
		if n == "" || n == "_" {
			continue
		}
		names = append(names, n)
	}
	if len(names) == 0 {
		return "", nil, "", false
	}
	root = ""
	if rm := rootLine.FindStringSubmatch(top); rm != nil {
		root = strings.TrimSpace(rm[1])
	}
	return names[0], names[1:], root, true
}

// nginxFileMarker matches the delimiters the remote listing command
// (see Client.discoverySiteEnabledCommand) wraps every file's content in —
// see that command for the exact format this must stay in sync with.
var nginxFileMarker = regexp.MustCompile(`(?s)===NGXFILE:(.*?)===\n(.*?)===NGXFILE_END===`)

// ParseSitesEnabled turns the raw output of listing every file under
// /etc/nginx/sites-enabled (concatenated with the ===NGXFILE:.../===NGXFILE_END===
// markers the remote command emits) into one VHost per distinct domain.
//
// A domain appearing in more than one server block (a plain-HTTP block and
// its HTTPS counterpart is the common case) keeps the first occurrence that
// actually has a root directive, since that is the one worth reading
// wp-config.php from.
func ParseSitesEnabled(raw string) []NginxVHost {
	byDomain := map[string]*NginxVHost{}
	var order []string

	for _, fm := range nginxFileMarker.FindAllStringSubmatch(raw, -1) {
		path := strings.TrimSpace(fm[1])
		content := fm[2]
		for _, block := range extractServerBlocks(content) {
			domain, aliases, root, ok := parseServerBlock(block)
			if !ok {
				continue
			}
			if existing, seen := byDomain[domain]; seen {
				if existing.Root == "" && root != "" {
					existing.Root = root
					existing.Aliases = aliases
					existing.SourceFile = path
				}
				continue
			}
			byDomain[domain] = &NginxVHost{Domain: domain, Aliases: aliases, Root: root, SourceFile: path}
			order = append(order, domain)
		}
	}

	out := make([]NginxVHost, 0, len(order))
	for _, d := range order {
		out = append(out, *byDomain[d])
	}
	return out
}

// WPConfigInfo is what a site's wp-config.php reveals about its database.
type WPConfigInfo struct {
	DBName      string
	DBUser      string
	DBPassword  string
	DBHost      string
	TablePrefix string
	// ExtraDefines are the source's own define(...) statements for
	// constants the generated wp-config.php does not set itself —
	// multisite settings, WP_CACHE, plugin licence keys, Redis settings —
	// carried over verbatim so the migrated site behaves the same.
	ExtraDefines []string
}

// defineValue returns a string constant's value, honouring the quote style
// it was written with: a password may well contain the other kind of quote,
// or an escaped one of its own.
func defineValue(raw, constant string) string {
	re := regexp.MustCompile(`define\s*\(\s*['"]` + regexp.QuoteMeta(constant) +
		`['"]\s*,\s*(?:'((?:[^'\\]|\\.)*)'|"((?:[^"\\]|\\.)*)")`)
	if m := re.FindStringSubmatchIndex(raw); m != nil {
		if m[2] >= 0 {
			return unescapePHPSingle(raw[m[2]:m[3]])
		}
		if m[4] >= 0 {
			return unescapePHPDouble(raw[m[4]:m[5]])
		}
	}
	return ""
}

var tablePrefixLine = regexp.MustCompile(`(?m)^\s*\$table_prefix\s*=\s*['"]([^'"]*)['"]`)

// unescapePHPSingle undoes the two escapes a single-quoted PHP string has.
func unescapePHPSingle(s string) string {
	return strings.NewReplacer(`\\`, `\`, `\'`, `'`).Replace(s)
}

// unescapePHPDouble undoes the escapes that plausibly appear in a
// double-quoted credential.
func unescapePHPDouble(s string) string {
	return strings.NewReplacer(`\\`, `\`, `\"`, `"`, `\$`, `$`).Replace(s)
}

// generatedDefines are the constants the generated wp-config.php sets
// itself; a source copy of any of them would be a duplicate define.
var generatedDefines = map[string]bool{
	"DB_NAME": true, "DB_USER": true, "DB_PASSWORD": true, "DB_HOST": true,
	"DB_CHARSET": true, "DB_COLLATE": true,
	"AUTH_KEY": true, "SECURE_AUTH_KEY": true, "LOGGED_IN_KEY": true, "NONCE_KEY": true,
	"AUTH_SALT": true, "SECURE_AUTH_SALT": true, "LOGGED_IN_SALT": true, "NONCE_SALT": true,
	"DISALLOW_FILE_EDIT": true, "DISALLOW_FILE_MODS": true, "DISABLE_WP_CRON": true,
	"WP_MEMORY_LIMIT": true, "WP_MAX_MEMORY_LIMIT": true,
	"WP_POST_REVISIONS": true, "EMPTY_TRASH_DAYS": true, "AUTOSAVE_INTERVAL": true,
	"WP_DEBUG": true, "WP_DEBUG_DISPLAY": true, "WP_DEBUG_LOG": true,
	"WP_HOME": true, "WP_SITEURL": true, "ABSPATH": true,
}

var defineStatement = regexp.MustCompile(`^\s*(define\s*\(\s*['"]([A-Za-z_][A-Za-z0-9_]*)['"]\s*,.*\)\s*;)`)

// extraDefines collects the single-line define(...) statements a
// wp-config.php makes outside comments, other than the ones the generated
// config sets itself. A define split over several lines, or wrapped in a
// conditional, is not carried over.
func extraDefines(raw string) []string {
	var out []string
	seen := map[string]bool{}
	inBlock := false
	for _, line := range strings.Split(raw, "\n") {
		trimmed := strings.TrimSpace(line)
		if inBlock {
			if strings.Contains(trimmed, "*/") {
				inBlock = false
			}
			continue
		}
		if strings.HasPrefix(trimmed, "/*") {
			if !strings.Contains(trimmed[2:], "*/") {
				inBlock = true
			}
			continue
		}
		m := defineStatement.FindStringSubmatch(line)
		if m == nil || generatedDefines[m[2]] || seen[m[2]] {
			continue
		}
		seen[m[2]] = true
		out = append(out, m[1])
	}
	return out
}

// ParseWPConfig reads the database constants and table prefix out of a
// wp-config.php's source. Deliberately tolerant of formatting — single or
// double quotes, any amount of internal whitespace — since a wp-config.php
// this tool did not generate can be styled any way its original author's
// editor auto-formatted it.
//
// The second return value is false when DB_NAME or DB_USER could not be
// found at all, which means this either is not a WordPress wp-config.php or
// is one this parser cannot make sense of — either way, not something safe
// to attempt a database migration from.
func ParseWPConfig(raw string) (WPConfigInfo, bool) {
	name := defineValue(raw, "DB_NAME")
	user := defineValue(raw, "DB_USER")
	if name == "" || user == "" {
		return WPConfigInfo{}, false
	}
	host := defineValue(raw, "DB_HOST")
	if host == "" {
		host = "localhost"
	}
	// A DB_HOST of "localhost:3306" or "127.0.0.1:3306" is common; mysqldump
	// wants the host and port split, but for a same-machine dump this is
	// only ever informational (Client.DumpDatabase always connects to
	// whatever this value says, port included, is what MySQL's own client
	// libraries already default to when the whole string is passed as-is).
	prefix := "wp_"
	if m := tablePrefixLine.FindStringSubmatch(raw); m != nil && m[1] != "" {
		prefix = m[1]
	}
	return WPConfigInfo{
		DBName:       name,
		DBUser:       user,
		DBPassword:   defineValue(raw, "DB_PASSWORD"),
		DBHost:       host,
		TablePrefix:  prefix,
		ExtraDefines: extraDefines(raw),
	}, true
}
