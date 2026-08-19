package site

import (
	"strings"
	"testing"

	"ngxsetup/internal/db"
)

func TestNormalizeDomain(t *testing.T) {
	cases := map[string]string{
		"Example.COM":      "example.com",
		"www.example.com":  "example.com",
		"example.com.":     "example.com",
		"  example.com  ":  "example.com",
		"WWW.Example.Com.": "example.com",
		"blog.example.com": "blog.example.com",
	}
	for in, want := range cases {
		if got := NormalizeDomain(in); got != want {
			t.Errorf("NormalizeDomain(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidateDomainRejectsInjection(t *testing.T) {
	// These reach nginx config files and filesystem paths. Rejecting them at
	// the boundary is what keeps them from becoming something worse.
	bad := []string{
		"", "localhost", "example", "-example.com", "example-.com",
		"exa mple.com", "example.com/../../etc/passwd", "example.com;rm -rf /",
		"exam$ple.com", "example.com\nserver{}", "../etc", "example..com",
		"http://example.com", "example.com:8080", "*.example.com",
		strings.Repeat("a", 250) + ".com",
	}
	for _, d := range bad {
		if err := ValidateDomain(d); err == nil {
			t.Errorf("ValidateDomain(%q) accepted an invalid domain", d)
		}
	}

	good := []string{
		"example.com", "blog.example.com", "a.b.c.example.co.uk",
		"xn--80ak6aa92e.com", "my-site.example.com", "123.example.com",
	}
	for _, d := range good {
		if err := ValidateDomain(NormalizeDomain(d)); err != nil {
			t.Errorf("ValidateDomain(%q) rejected a valid domain: %v", d, err)
		}
	}
}

func TestSlug(t *testing.T) {
	cases := map[string]string{
		"example.com":        "example-com",
		"www.example.com":    "example-com",
		"blog.example.co.uk": "blog-example-co-uk",
		"123abc.com":         "s123abc-com",
		"my-site.com":        "my-site-com",
	}
	for in, want := range cases {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
}

// The account name derived from a slug has to fit in the kernel's 32-character
// limit, or useradd fails partway through provisioning.
func TestSlugFitsSystemUserLimit(t *testing.T) {
	long := "a-very-long-subdomain-name-indeed.example-company-limited.co.uk"
	s := Slug(long)
	u := UserName(s)
	if len(u) > 32 {
		t.Errorf("user name %q is %d characters, over the 32-character limit", u, len(u))
	}
	if strings.HasSuffix(s, "-") || strings.HasPrefix(s, "-") {
		t.Errorf("slug %q has a stray separator after truncation", s)
	}
}

// Two distinct domains must never share a slug: they would share a document
// root, a system user and a PHP pool.
func TestUniqueSlugAvoidsCollisions(t *testing.T) {
	used := map[string]bool{"example-com": true}
	got := UniqueSlug("example.com", func(s string) bool { return used[s] })
	if got == "example-com" {
		t.Fatal("returned a slug that is already taken")
	}
	used[got] = true

	third := UniqueSlug("example.com", func(s string) bool { return used[s] })
	if used[third] {
		t.Fatalf("third allocation collided: %q", third)
	}
	if len(UserName(third)) > 32 {
		t.Errorf("disambiguated slug produced an over-long user name: %q", UserName(third))
	}
}

// Database names must be valid unquoted SQL identifiers, which is stricter
// than the slug rules: no hyphens, no leading digit.
func TestDBNameIsAValidIdentifier(t *testing.T) {
	for _, domain := range []string{
		"example.com", "123abc.com", "my-site.example.co.uk",
		"a-very-long-subdomain-name-indeed.example-company-limited.co.uk",
	} {
		name := DBName(Slug(domain), "a1b2")
		if err := db.ValidateIdentifier(name); err != nil {
			t.Errorf("DBName for %q produced %q which the database layer rejects: %v", domain, name, err)
		}
		if strings.Contains(name, "-") {
			t.Errorf("DBName %q contains a hyphen", name)
		}
	}
}

func TestDBNameLengthBounded(t *testing.T) {
	name := DBName(Slug(strings.Repeat("verylongname", 10)+".com"), "abcd")
	if len(name) > 64 {
		t.Errorf("database name is %d characters, over the 64-character limit", len(name))
	}
}

func TestTablePrefix(t *testing.T) {
	if got := TablePrefix("a1b2"); got != "wpa1b2_" {
		t.Errorf("TablePrefix = %q", got)
	}
	if got := TablePrefix(""); got != "wp_" {
		t.Errorf("empty suffix should give the WordPress default, got %q", got)
	}
}
