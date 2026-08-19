# Tuning

## Profiles

| Profile    | Use for                                                |
|------------|--------------------------------------------------------|
| `balanced` | a handful of ordinary WordPress sites (default)        |
| `cache`    | maximum traffic per gigabyte; the most aggressive cache |
| `density`  | many low-traffic sites on one machine                  |
| `database` | WooCommerce, large catalogues, query-heavy workloads   |

```bash
ngxsetup tune --profile=cache --explain     # preview and reasoning
ngxsetup tune --profile=cache --apply --save
```

`--save` persists the choice so later commands use it too.

If your sites run heavy page builders, tell the tuner what a worker
really costs, and it will size the pool accordingly:

```bash
ngxsetup tune --php-worker-mb 160 --explain
```

Plan for a machine you don't have yet:

```bash
ngxsetup tune --memory-mb 16384 --explain
```

## The memory budget

`tuning.Compute(facts.Facts, Options) Plan` is a pure function — no I/O,
no commands run — which is what makes it testable against synthetic
hardware from a 1 GB single-core VPS to a 64 GB 32-core box.
Everything starts here:

```
total RAM
  − reserve for the kernel, sshd, systemd, monitoring
  = usable

usable × db_weight   → database
usable × php_weight  → PHP worker pool
remainder            → left free, on purpose
```

The remainder is not slack. The kernel page cache serves the FastCGI
cache and every static asset out of it, so free memory is what turns a
cache hit into a memory read. A tuner that allocates 100% of RAM to
services produces a slower server than one that does not.

Weights come from the profile. The reserve shrinks proportionally as
machines grow, because the base system cost is largely fixed.

## Deriving the rest

Each number follows from the budget and from what actually constrains
it:

- **PHP `max_children`** is the smaller of `php_budget ÷ per_worker_MB`
  and `cores × 8`. Memory decides how many workers can coexist; CPU
  decides how many can make progress. `tune --explain` names which one
  bound the result.
- **`max_connections`** is `max_children + 25`. Only PHP workers,
  wp-cli and admin sessions can open connections, so sizing it from RAM
  (100 per gigabyte, the previous tuner's approach) creates thousands
  of slots nothing will use while charging real memory for the buffers
  behind them.
- **`innodb_buffer_pool_size`** is the database budget minus
  per-connection buffers and fixed overheads, then rounded *down* to a
  multiple of `instances × chunk_size`. InnoDB rounds up otherwise,
  which would push it past its budget.
- **InnoDB I/O settings** come from the detected storage class.
  Rotational and solid-state differ by an order of magnitude in the
  right `io_capacity`; an unknown device defaults to the SSD profile
  rather than being guessed as rotational, which would cripple fast
  storage.
- **`worker_rlimit_nofile`** is derived from `worker_connections`, so
  the two always agree.
- **FastCGI cache max size** is 15% of free disk space, rounded to the
  nearest 256 MB. That rounding matters: free disk drifts on its own
  between runs (logs, temp files, the cache itself growing) even with
  zero real configuration change, and an un-rounded value would
  rewrite the cache config — and reload nginx — on every re-apply for
  no reason. Found live; see the [changelog](https://github.com/mevijays/ngxsetup/commits/main)
  for the fix.

## Flavour awareness

MariaDB removed `innodb_buffer_pool_instances` in 10.5 and refuses to
start if it is present. MySQL 8 removed the query cache.
`utf8mb4_0900_ai_ci` exists only on MySQL. `default_authentication_plugin`
was removed in MySQL 8.4. The plan records which flavour is installed
and the template branches accordingly, which is why the same code
produces a working configuration on both.

## Caching

The FastCGI micro-cache is the main performance mechanism. Three parts
matter:

**What may be cached** is decided by `map` directives matching the
cookies WordPress and WooCommerce actually set, the request method, and
a list of personal or administrative paths. Notably *not* excluded:
feeds, sitemaps, paginated archives and URLs with query strings — some
older configurations exclude all of those, which means almost no real
traffic is ever cached.

**The cache key** normalises away campaign and click-tracking
parameters, so a URL whose query string is nothing but `utm_*` and
`fbclid` folds onto its clean form. Every shared social link becomes a
hit instead of a miss.

**The serving policy** combines `fastcgi_cache_lock` (a stampede of
concurrent misses for one key collapses into a single upstream request)
with `fastcgi_cache_background_update` and `use_stale` (an expired
entry is served immediately while it refreshes behind the scenes).
Together they mean a cache expiry is not a latency spike.

Purging reads the key nginx stores in each cache file's header, so
per-site purge works on a stock nginx with no third-party module:

```bash
sudo ngxsetup cache purge example.com
sudo ngxsetup cache purge         # everything
ngxsetup cache stats
```

Check whether a request was served from cache via the
`X-Cache-Status` response header. `BYPASS` means one of the skip rules
matched — a login cookie, a POST, or an administrative path. `MISS`
followed by `HIT` on a second request is correct.

## Applying twice changes nothing

Rendering is deterministic — no timestamps, no hostnames, no random
values — and every MB-sized value is derived from either total RAM
(essentially constant between two nearby runs) or free disk space,
rounded to absorb ordinary drift. A re-apply with nothing actually
changed reports nothing changed, and reloads nothing — verified in CI
on every push by running `tune --apply` twice back-to-back against a
real stack. See [Architecture → The apply pipeline](architecture.md#the-apply-pipeline).
