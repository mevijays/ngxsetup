// Package state persists what the tool has provisioned.
//
// Without a registry, a provisioning tool can only ever add things: it cannot
// list what it made, cannot cleanly remove a site, and cannot tell whether a
// file on disk is one of its own. The previous implementation had no registry
// at all, which is why it had no `remove` and no `list`.
//
// The file is JSON with an explicit schema version, and it never holds a
// password — credentials live in a mode-0600 file next to the site and in
// wp-config.php, so a leaked state file is not a leaked database.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"
)

// SchemaVersion is bumped when the on-disk format changes incompatibly.
const SchemaVersion = 1

// DefaultPath is where the registry lives.
const DefaultPath = "/var/lib/ngxsetup/state.json"

// Site records one provisioned virtual host.
type Site struct {
	Slug    string   `json:"slug"`
	Domain  string   `json:"domain"`
	Aliases []string `json:"aliases,omitempty"`

	Root       string `json:"root"`
	User       string `json:"user"`
	SocketPath string `json:"socket_path"`
	PHPVersion string `json:"php_version"`

	TLS      bool   `json:"tls"`
	CertPath string `json:"cert_path,omitempty"`
	// ChainPath is the issuer-only certificate chain (certbot's chain.pem),
	// distinct from CertPath's leaf+chain fullchain.pem. nginx needs it
	// separately for ssl_trusted_certificate, which OCSP stapling
	// verification requires — without it nginx logs "issuer certificate not
	// found" and silently skips stapling on every reload. Only ever set for
	// a CA-issued certificate; a self-signed one has no issuer to chain to.
	ChainPath string `json:"chain_path,omitempty"`
	// CertSource is "letsencrypt", "self-signed" or "custom", which determines
	// whether renewal is this tool's responsibility.
	CertSource string `json:"cert_source,omitempty"`

	// Database identifiers only. The password is deliberately absent.
	DBName string `json:"db_name,omitempty"`
	DBUser string `json:"db_user,omitempty"`

	WordPress    bool   `json:"wordpress"`
	CacheEnabled bool   `json:"cache_enabled"`
	Enabled      bool   `json:"enabled"`
	CreatedAt    string `json:"created_at"`
}

// ServerNames returns the domain and its aliases, for nginx's server_name.
func (s Site) ServerNames() string {
	return strings.Join(append([]string{s.Domain}, s.Aliases...), " ")
}

// State is the whole registry.
type State struct {
	SchemaVersion int    `json:"schema_version"`
	Profile       string `json:"profile,omitempty"`
	PHPVersion    string `json:"php_version,omitempty"`
	DBFlavor      string `json:"db_flavor,omitempty"`
	// SetupCompleted records that `setup` has run to completion, so other
	// commands can give a useful error instead of a confusing one.
	SetupCompleted bool   `json:"setup_completed"`
	LastAppliedAt  string `json:"last_applied_at,omitempty"`
	Sites          []Site `json:"sites"`

	path string
	// base is what this State looked like when it was loaded (or last
	// saved). Save merges against it — see Save.
	base *State
}

// ErrNotFound is returned when a named site is not in the registry.
var ErrNotFound = errors.New("site not found")

// Load reads the registry, returning an empty one when the file is absent.
func Load(path string) (*State, error) {
	if path == "" {
		path = DefaultPath
	}
	s := &State{SchemaVersion: SchemaVersion, path: path}

	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	if err := json.Unmarshal(data, s); err != nil {
		// A corrupt registry must not be silently replaced with an empty one:
		// that would orphan every site it described.
		return nil, fmt.Errorf("%s is corrupt (%w); move it aside to start fresh", path, err)
	}
	if s.SchemaVersion > SchemaVersion {
		return nil, fmt.Errorf("%s was written by a newer ngxsetup (schema %d, this build understands %d)",
			path, s.SchemaVersion, SchemaVersion)
	}
	s.path = path
	s.base = s.snapshot()
	return s, nil
}

// Save writes the registry atomically with restrictive permissions.
//
// More than one process can hold a State at once — a CLI command run while
// `ngxsetup web` is up, or a migration job that runs for an hour while the
// web UI keeps adding and removing sites — and a plain "write what I have"
// would let whichever saved last silently erase every change the others
// made in the meantime. So Save is a three-way merge, under an exclusive
// lock: it re-reads the file, and for every site (and every top-level
// field) this State changed since it was loaded, its version wins;
// everything else keeps whatever is on disk now.
func (s *State) Save() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	unlock, err := lockFile(s.path + ".lock")
	if err != nil {
		return err
	}
	defer unlock()

	disk, err := Load(s.path)
	if err != nil {
		return err
	}
	merged := s.mergeOnto(disk)
	merged.SchemaVersion = SchemaVersion
	sort.Slice(merged.Sites, func(i, j int) bool { return merged.Sites[i].Slug < merged.Sites[j].Slug })

	data, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}

	// This State now reflects the file, including other writers' changes.
	s.SchemaVersion = merged.SchemaVersion
	s.Profile, s.PHPVersion, s.DBFlavor = merged.Profile, merged.PHPVersion, merged.DBFlavor
	s.SetupCompleted, s.LastAppliedAt = merged.SetupCompleted, merged.LastAppliedAt
	s.Sites = merged.Sites
	s.base = s.snapshot()
	return nil
}

// snapshot deep-copies the persisted fields.
func (s *State) snapshot() *State {
	c := &State{
		SchemaVersion:  s.SchemaVersion,
		Profile:        s.Profile,
		PHPVersion:     s.PHPVersion,
		DBFlavor:       s.DBFlavor,
		SetupCompleted: s.SetupCompleted,
		LastAppliedAt:  s.LastAppliedAt,
		Sites:          make([]Site, len(s.Sites)),
	}
	for i, site := range s.Sites {
		site.Aliases = append([]string(nil), site.Aliases...)
		c.Sites[i] = site
	}
	return c
}

// mergeOnto returns disk with this State's own changes (relative to base)
// applied on top.
func (s *State) mergeOnto(disk *State) *State {
	base := s.base
	if base == nil {
		base = &State{}
	}
	out := disk.snapshot()
	out.path = s.path

	if s.Profile != base.Profile {
		out.Profile = s.Profile
	}
	if s.PHPVersion != base.PHPVersion {
		out.PHPVersion = s.PHPVersion
	}
	if s.DBFlavor != base.DBFlavor {
		out.DBFlavor = s.DBFlavor
	}
	if s.SetupCompleted != base.SetupCompleted {
		out.SetupCompleted = s.SetupCompleted
	}
	if s.LastAppliedAt != base.LastAppliedAt {
		out.LastAppliedAt = s.LastAppliedAt
	}

	baseBy := map[string]Site{}
	for _, site := range base.Sites {
		baseBy[site.Slug] = site
	}
	mine := map[string]Site{}
	for _, site := range s.Sites {
		mine[site.Slug] = site
	}
	result := map[string]Site{}
	var order []string
	for _, site := range out.Sites {
		result[site.Slug] = site
		order = append(order, site.Slug)
	}
	for slug, m := range mine {
		if b, ok := baseBy[slug]; ok && reflect.DeepEqual(normalizeSite(b), normalizeSite(m)) {
			continue // unchanged here; keep whatever is on disk
		}
		if _, ok := result[slug]; !ok {
			order = append(order, slug)
		}
		result[slug] = m
	}
	for slug := range baseBy {
		if _, ok := mine[slug]; !ok {
			delete(result, slug) // deleted here
		}
	}
	out.Sites = out.Sites[:0]
	for _, slug := range order {
		if site, ok := result[slug]; ok {
			out.Sites = append(out.Sites, site)
		}
	}
	return out
}

// normalizeSite makes nil and empty alias lists compare equal.
func normalizeSite(s Site) Site {
	if len(s.Aliases) == 0 {
		s.Aliases = nil
	}
	return s
}

// Path reports where this registry is stored.
func (s *State) Path() string { return s.path }

// Find locates a site by slug or by any of its domain names.
func (s *State) Find(nameOrSlug string) (*Site, error) {
	q := strings.ToLower(strings.TrimSpace(nameOrSlug))
	q = strings.TrimPrefix(q, "www.")
	for i := range s.Sites {
		site := &s.Sites[i]
		if site.Slug == q || strings.EqualFold(site.Domain, q) {
			return site, nil
		}
		for _, a := range site.Aliases {
			if strings.EqualFold(a, q) {
				return site, nil
			}
		}
	}
	return nil, fmt.Errorf("%w: %s", ErrNotFound, nameOrSlug)
}

// Upsert adds or replaces a site record.
func (s *State) Upsert(site Site) {
	if site.CreatedAt == "" {
		site.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	}
	for i := range s.Sites {
		if s.Sites[i].Slug == site.Slug {
			// Preserve the original creation time across updates.
			site.CreatedAt = s.Sites[i].CreatedAt
			s.Sites[i] = site
			return
		}
	}
	s.Sites = append(s.Sites, site)
}

// Delete removes a site record.
func (s *State) Delete(slug string) bool {
	for i := range s.Sites {
		if s.Sites[i].Slug == slug {
			s.Sites = append(s.Sites[:i], s.Sites[i+1:]...)
			return true
		}
	}
	return false
}

// DomainTaken reports whether any existing site already answers for a name,
// which would otherwise produce two server blocks competing for one host.
func (s *State) DomainTaken(domain string, exceptSlug string) bool {
	d := strings.ToLower(domain)
	for _, site := range s.Sites {
		if site.Slug == exceptSlug {
			continue
		}
		if strings.EqualFold(site.Domain, d) {
			return true
		}
		for _, a := range site.Aliases {
			if strings.EqualFold(a, d) {
				return true
			}
		}
	}
	return false
}

// SlugTaken reports whether a slug is already in use.
func (s *State) SlugTaken(slug string) bool {
	for _, site := range s.Sites {
		if site.Slug == slug {
			return true
		}
	}
	return false
}

// Count returns the number of registered sites, which feeds pool and opcache
// sizing.
func (s *State) Count() int { return len(s.Sites) }

// Touch records that configuration was applied.
func (s *State) Touch() { s.LastAppliedAt = time.Now().UTC().Format(time.RFC3339) }
