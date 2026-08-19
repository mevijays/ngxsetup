package site

import (
	"fmt"
	"regexp"
	"strings"
)

// Naming turns a domain name into the several different identifiers a site
// needs, each with its own constraints:
//
//   - a slug, used for filenames and the nginx server block
//   - a Linux account name, which the kernel limits to 32 characters
//   - a database name and user, which must be a valid unquoted SQL identifier
//
// The previous implementation used one transformation for all of them —
// stripping dots from the domain — which silently collided for
// example.com and exam.ple.com, and produced database names that were invalid
// whenever a domain started with a digit or exceeded the length limit.

const (
	// maxUserName is the kernel's limit on a login name.
	maxUserName = 32
	userPrefix  = "web-"
	// maxSlug leaves room for the prefix within maxUserName.
	maxSlug = maxUserName - len(userPrefix)
	// maxDBBase leaves room for the separator and uniqueness suffix within
	// MySQL's 64-character identifier limit.
	maxDBBase = 32
)

var (
	nonSlugChar   = regexp.MustCompile(`[^a-z0-9]+`)
	validDomain   = regexp.MustCompile(`^(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)
	leadingDigits = regexp.MustCompile(`^[0-9]+`)
)

// NormalizeDomain lowercases a domain and strips a leading "www." and any
// trailing dot, so www.Example.com. and example.com describe one site.
func NormalizeDomain(d string) string {
	d = strings.ToLower(strings.TrimSpace(d))
	d = strings.TrimSuffix(d, ".")
	d = strings.TrimPrefix(d, "www.")
	return d
}

// ValidateDomain rejects input that is not a plausible hostname.
//
// This runs before the name reaches a config file, a filesystem path or a
// shell-adjacent context, so it is also the boundary that keeps a crafted
// "domain" from becoming an injection.
func ValidateDomain(d string) error {
	if d == "" {
		return fmt.Errorf("domain is required")
	}
	if len(d) > 253 {
		return fmt.Errorf("domain %q is longer than the 253-character limit", d)
	}
	if !validDomain.MatchString(d) {
		return fmt.Errorf("%q is not a valid domain name (expected something like example.com; internationalised domains must be given in punycode, e.g. xn--80ak6aa92e.com)", d)
	}
	return nil
}

// Slug derives the short identifier used for filenames, the nginx server block
// and the PHP-FPM pool.
func Slug(domain string) string {
	s := nonSlugChar.ReplaceAllString(NormalizeDomain(domain), "-")
	s = strings.Trim(s, "-")

	// A name starting with a digit is a poor Linux account name and an invalid
	// unquoted SQL identifier, so give it a letter to start from.
	if s == "" {
		s = "site"
	} else if s[0] >= '0' && s[0] <= '9' {
		s = "s" + s
	}

	if len(s) > maxSlug {
		s = strings.Trim(s[:maxSlug], "-")
	}
	return s
}

// UniqueSlug appends a numeric discriminator until the slug is free. Two
// different domains that reduce to the same slug — example.com and
// example.net — must not share a pool, a user or a document root.
func UniqueSlug(domain string, taken func(string) bool) string {
	base := Slug(domain)
	if !taken(base) {
		return base
	}
	for i := 2; i < 1000; i++ {
		suffix := fmt.Sprintf("-%d", i)
		trimmed := base
		if len(trimmed)+len(suffix) > maxSlug {
			trimmed = strings.Trim(base[:maxSlug-len(suffix)], "-")
		}
		candidate := trimmed + suffix
		if !taken(candidate) {
			return candidate
		}
	}
	return base
}

// UserName returns the Linux account that owns a site and runs its PHP pool.
func UserName(slug string) string {
	name := userPrefix + slug
	if len(name) > maxUserName {
		name = name[:maxUserName]
	}
	return strings.Trim(name, "-")
}

// DBName builds a database identifier from a slug plus a random suffix.
//
// The suffix is not decoration: it stops a newly created site from silently
// adopting the tables of a previously removed one with the same name, which
// would present the old site's content under the new domain.
func DBName(slug, suffix string) string {
	base := strings.ReplaceAll(slug, "-", "_")
	base = leadingDigits.ReplaceAllString(base, "")
	base = strings.Trim(base, "_")
	if base == "" {
		base = "site"
	}
	if len(base) > maxDBBase {
		base = strings.Trim(base[:maxDBBase], "_")
	}
	if suffix == "" {
		return base
	}
	return base + "_" + suffix
}

// TablePrefix returns the WordPress table prefix for a site.
//
// Randomising it is mild obscurity rather than a security control, but it does
// defeat the large class of automated attacks whose payloads hard-code wp_.
func TablePrefix(suffix string) string {
	if suffix == "" {
		return "wp_"
	}
	return "wp" + suffix + "_"
}
