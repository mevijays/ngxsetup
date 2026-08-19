package provision

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"ngxsetup/internal/logx"
	"ngxsetup/internal/system"
)

// cacheKeyPrefixes enumerates the scheme and method combinations that can
// appear at the start of a cache key, given the key format configured in
// 30-cache.conf: "$scheme$request_method$host$uri$args".
//
// Enumerating them lets a purge match a host exactly. A substring search would
// purge notexample.com when asked to purge example.com.
var cacheKeyPrefixes = []string{
	"httpGET", "httpsGET", "httpHEAD", "httpsHEAD",
	"httpPOST", "httpsPOST",
}

// PurgeCache removes cached responses.
//
// Purging is done by reading the key nginx stores in each cache file's header
// rather than by requiring the third-party cache-purge module. That keeps the
// feature working on a stock nginx, which the module-based approach in the old
// configuration did not — it referenced fastcgi_cache_purge without ever
// installing a module that provides it.
func (c *Ctx) PurgeCache(nameOrSlug string) error {
	cacheRoot := c.Path(CacheDir)
	if _, err := os.Stat(cacheRoot); err != nil {
		logx.Info("no cache directory at %s; nothing to purge", CacheDir)
		return nil
	}

	if nameOrSlug == "" {
		return c.purgeAll(cacheRoot)
	}

	rec, err := c.State.Find(nameOrSlug)
	if err != nil {
		return err
	}
	hosts := append([]string{rec.Domain}, rec.Aliases...)
	return c.purgeHosts(cacheRoot, hosts, rec.Domain)
}

func (c *Ctx) purgeAll(cacheRoot string) error {
	if c.Writer.DryRun {
		logx.Change("[dry-run] would empty the entire FastCGI cache")
		return nil
	}
	removed := 0
	entries, err := os.ReadDir(cacheRoot)
	if err != nil {
		return err
	}
	for _, e := range entries {
		// Remove the shard directories rather than the cache root itself, so
		// the directory's ownership and mode survive.
		if err := os.RemoveAll(filepath.Join(cacheRoot, e.Name())); err != nil {
			return err
		}
		removed++
	}
	logx.Change("emptied the FastCGI cache (%d shards)", removed)
	return c.reloadNginxAfterPurge()
}

func (c *Ctx) purgeHosts(cacheRoot string, hosts []string, label string) error {
	// Precompute the exact key prefixes that identify this site's entries.
	var wanted [][]byte
	for _, h := range hosts {
		for _, p := range cacheKeyPrefixes {
			wanted = append(wanted, []byte(p+h+"/"))
		}
	}

	matched, removed := 0, 0
	err := filepath.WalkDir(cacheRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		key, err := readCacheKey(path)
		if err != nil {
			return nil // an entry being written right now; skip it
		}
		for _, w := range wanted {
			if bytes.HasPrefix(key, w) {
				matched++
				if c.Writer.DryRun {
					return nil
				}
				if err := os.Remove(path); err == nil {
					removed++
				}
				return nil
			}
		}
		return nil
	})
	if err != nil {
		return err
	}

	if c.Writer.DryRun {
		logx.Change("[dry-run] would purge %d cached responses for %s", matched, label)
		return nil
	}
	logx.Change("purged %d cached responses for %s", removed, label)
	return c.reloadNginxAfterPurge()
}

// readCacheKey extracts the request key from an nginx cache file.
//
// The file starts with a binary header, then a line of the form "KEY: <key>".
// Only the first kilobyte is read: the rest is the cached response body.
func readCacheKey(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	buf := make([]byte, 1024)
	n, err := f.Read(buf)
	if n == 0 {
		return nil, err
	}
	buf = buf[:n]

	i := bytes.Index(buf, []byte("\nKEY: "))
	if i < 0 {
		return nil, fmt.Errorf("no key header")
	}
	rest := buf[i+len("\nKEY: "):]
	if j := bytes.IndexByte(rest, '\n'); j >= 0 {
		return rest[:j], nil
	}
	return nil, fmt.Errorf("truncated key header")
}

func (c *Ctx) reloadNginxAfterPurge() error {
	if !system.IsActive(c.Context, c.Runner, "nginx.service") {
		return nil
	}
	// nginx keeps the cache index in shared memory. Removing files from under
	// it leaves stale index entries that resolve to nothing until the cache
	// manager notices; a reload rebuilds the index immediately.
	return system.Reload(c.Context, c.Runner, "nginx.service")
}

// CacheStats reports the on-disk size and entry count of the cache.
func (c *Ctx) CacheStats() (entries int, bytesUsed int64, err error) {
	root := c.Path(CacheDir)
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		entries++
		bytesUsed += info.Size()
		return nil
	})
	if err != nil && strings.Contains(err.Error(), "no such file") {
		return 0, 0, nil
	}
	return entries, bytesUsed, err
}
