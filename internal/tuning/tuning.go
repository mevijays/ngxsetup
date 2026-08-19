// Package tuning turns a machine profile into a complete configuration
// decision for nginx, PHP-FPM, opcache and MariaDB/MySQL.
//
// The whole package is a pure function of facts.Facts plus Options. Nothing
// here touches the filesystem, runs a command, or reads the environment, which
// means every sizing decision can be tested against synthetic hardware — a
// 1 GB single-core VPS and a 64 GB 32-core box exercise the same code path.
//
// The governing idea is a memory budget. Total RAM is split explicitly between
// the database, the PHP worker pool, and deliberately unallocated memory for
// the kernel page cache. Every other number is derived from that split, so the
// configuration cannot over-commit memory the way an ad-hoc collection of
// "recommended" values does.
package tuning

import (
	"fmt"

	"ngxsetup/internal/facts"
)

// defaultPHPWorkerMB is the assumed steady-state RSS of one PHP-FPM child
// running WordPress with a typical plugin set, excluding the shared opcache
// segment. Page builders and WooCommerce run heavier; override via Options.
const defaultPHPWorkerMB = 80

// perConnectionDBOverheadMB approximates the peak per-thread buffers a MySQL
// connection can allocate with the buffer sizes this plan emits.
const perConnectionDBOverheadMB = 4

// fpmMasterMB is the resident cost of one PHP-FPM master process. Each site
// runs its own isolated instance rather than sharing one, which is what makes
// the per-site mount namespace possible — but N sites means N masters, and
// that memory has to come out of the budget rather than being discovered as a
// shortfall later. Much of a master's footprint is shared pages across
// instances, so this is deliberately a conservative per-instance figure.
const fpmMasterMB = 12

// Compute derives the full plan. It never returns an error: unknown facts fall
// back to conservative defaults and are recorded in Plan.Warnings instead, so
// a missing /proc entry degrades the tuning rather than blocking provisioning.
func Compute(f facts.Facts, o Options) Plan {
	p := Plan{Profile: o.Profile}
	if p.Profile == "" {
		p.Profile = ProfileBalanced
	}

	p.Budget = computeBudget(f, o, p.Profile, &p)
	p.PHP, p.OPcache = computePHP(f, o, p.Budget, p.Profile, &p)
	p.DB = computeDB(f, o, p.Budget, p.PHP, &p)
	p.Nginx = computeNginx(f, o, p.Budget, p.PHP, &p)
	p.Sysctl = computeSysctl(f, p.Nginx, &p)
	p.Limits = LimitsPlan{
		NginxNofile: p.Nginx.WorkerRlimitNofile,
		PHPNofile:   p.PHP.RlimitFiles,
		DBNofile:    max(65535, p.DB.MaxConnections*8),
	}

	p.validate(f)
	return p
}

// ---- budget ----------------------------------------------------------------

func computeBudget(f facts.Facts, o Options, prof Profile, p *Plan) Budget {
	total := f.MemTotalMB
	if total <= 0 {
		// Never invent a large number: sizing a 512 MB box as if it had 4 GB
		// produces a configuration that cannot start.
		total = 1024
		p.Warnings = append(p.Warnings,
			"could not read total memory; assuming 1024 MB. Pass --memory-mb to override.")
	}

	reserve := o.ReserveMB
	if reserve <= 0 {
		reserve = defaultReserve(total)
	}
	if reserve >= total {
		reserve = total / 4
	}
	usable := total - reserve

	// Each site's isolated PHP-FPM master is charged before anything is
	// split, because it is spent whether the site serves a request or not.
	// Folding it into the PHP slice instead would let the worker count be
	// sized against memory the masters have already consumed.
	masters := o.Sites * fpmMasterMB
	if masters > usable/3 {
		// A pathological site count on a small box: cap the charge so the
		// budget stays workable, and say so rather than silently producing
		// a plan that cannot hold.
		masters = usable / 3
		p.Warnings = append(p.Warnings, fmt.Sprintf(
			"%d sites × %d MB of per-site PHP-FPM masters exceeds a third of usable memory; "+
				"consider fewer sites per host or more RAM", o.Sites, fpmMasterMB))
	}
	if masters > 0 {
		usable -= masters
		p.Notes = append(p.Notes, fmt.Sprintf(
			"reserved %d MB for %d isolated PHP-FPM master processes (%d MB each) — the cost of running "+
				"each site in its own mount namespace rather than sharing one service",
			masters, o.Sites, fpmMasterMB))
	}

	dbW, phpW := weights(prof, total)
	b := Budget{
		TotalMB:   total,
		ReserveMB: reserve,
		UsableMB:  usable,
		DBMB:      int(float64(usable) * dbW),
		PHPMB:     int(float64(usable) * phpW),
	}
	b.FreeMB = usable - b.DBMB - b.PHPMB

	p.Notes = append(p.Notes, fmt.Sprintf(
		"reserved %d MB for the kernel, sshd, systemd and monitoring; %d MB left to allocate",
		reserve, usable))
	p.Notes = append(p.Notes, fmt.Sprintf(
		"left %d MB unallocated so the kernel page cache can hold the FastCGI cache and static files in RAM",
		b.FreeMB))
	return b
}

// defaultReserve withholds memory for everything that is not part of the web
// stack. The proportion shrinks as machines grow because the base system cost
// is largely fixed.
func defaultReserve(total int) int {
	switch {
	case total <= 1024:
		return 192
	case total <= 2048:
		return 288
	case total <= 4096:
		return 416
	case total <= 8192:
		return 640
	case total <= 16384:
		return 1024
	default:
		r := 1024 + (total-16384)/20
		return min(r, 4096)
	}
}

// weights returns the database and PHP shares of usable memory. The remainder
// is intentionally left free.
func weights(prof Profile, total int) (db, php float64) {
	switch prof {
	case ProfileCache:
		db, php = 0.32, 0.33
	case ProfileDensity:
		db, php = 0.34, 0.46
	case ProfileDatabase:
		db, php = 0.52, 0.32
	default: // balanced
		db, php = 0.40, 0.42
	}
	// On very small machines the free-memory slice matters more than either
	// service: with 1 GB, page cache is the only thing standing between a
	// traffic spike and swap death.
	if total <= 1536 {
		db, php = db*0.85, php*0.85
	}
	return db, php
}

// ---- PHP -------------------------------------------------------------------

func computePHP(f facts.Facts, o Options, b Budget, prof Profile, p *Plan) (PHPPlan, OPcachePlan) {
	workerMB := o.AvgPHPWorkerMB
	if workerMB <= 0 {
		workerMB = defaultPHPWorkerMB
	}

	// Two independent ceilings. Memory decides how many workers can coexist;
	// CPU decides how many can make progress. Exceeding either just converts
	// throughput into queueing and, in the memory case, into swapping.
	byMem := b.PHPMB / workerMB
	perCore := 8
	switch prof {
	case ProfileDensity:
		perCore = 10
	case ProfileCache:
		perCore = 6
	}
	byCPU := f.CPUCores * perCore

	maxChildren := clamp(min(byMem, byCPU), 2, 512)
	limiting := "memory"
	if byCPU < byMem {
		limiting = "CPU"
	}
	p.Notes = append(p.Notes, fmt.Sprintf(
		"php-fpm max_children=%d (%d MB budget ÷ %d MB per worker = %d; %d cores × %d = %d; %s is the binding constraint)",
		maxChildren, b.PHPMB, workerMB, byMem, f.CPUCores, perCore, byCPU, limiting))

	php := PHPPlan{
		MaxChildren:        maxChildren,
		AvgWorkerMB:        workerMB,
		ProcessIdleTimeout: "10s",
		// Recycling children bounds the damage from leaky plugins without
		// paying fork costs on every request the way a low value would.
		MaxRequests:   1000,
		ListenBacklog: 1024,
		RlimitFiles:   65535,
		// The legacy configuration allowed 18000 seconds. A single stuck
		// request could then hold a worker for five hours; with a small pool
		// that is a self-inflicted outage.
		MaxExecutionTime:        300,
		RequestTerminateTimeout: 310,
		MaxInputVars:            5000,
		SlowlogTimeout:          10,
		RealpathCacheKB:         4096,
		RealpathCacheTTL:        600,
	}

	// Process manager. On-demand costs nothing while idle, which is what a
	// dense or memory-tight box needs; dynamic keeps warm workers ready,
	// which is what a busy box needs.
	switch {
	case prof == ProfileDensity, b.TotalMB < 2048, o.Sites > 8:
		php.PM = "ondemand"
		p.Notes = append(p.Notes,
			"php-fpm pm=ondemand: idle sites cost no memory, at the price of a fork on the first request after idle")
	default:
		php.PM = "dynamic"
		php.StartServers = max(2, maxChildren/4)
		php.MinSpareServers = max(1, maxChildren/8)
		php.MaxSpareServers = max(php.StartServers, maxChildren/2)
		if php.MaxSpareServers > maxChildren {
			php.MaxSpareServers = maxChildren
		}
		if php.StartServers > php.MaxSpareServers {
			php.StartServers = php.MaxSpareServers
		}
		if php.MinSpareServers > php.StartServers {
			php.MinSpareServers = php.StartServers
		}
	}

	// memory_limit bounds a single request, not the pool. It must be well
	// under the pool budget or one runaway request evicts everything else.
	switch {
	case b.TotalMB < 2048:
		php.MemoryLimitMB = 128
	case b.TotalMB < 4096:
		php.MemoryLimitMB = 192
	case b.TotalMB < 16384:
		php.MemoryLimitMB = 256
	default:
		php.MemoryLimitMB = 512
	}
	// wp-cli imports, migrations and backups legitimately need more than a
	// web request does, and they run one at a time.
	php.CLIMemoryLimitMB = max(512, php.MemoryLimitMB*2)

	php.UploadMaxMB = o.UploadMaxMB
	if php.UploadMaxMB <= 0 {
		php.UploadMaxMB = 128
	}

	// opcache is a single shared segment per FPM master, used by every pool.
	op := OPcachePlan{
		MemoryMB:            clamp(b.UsableMB*6/100, 96, 512),
		MaxAcceleratedFiles: opcacheFiles(o.Sites),
		ValidateTimestamps:  !o.AggressiveOpcache,
		RevalidateFreq:      2,
		APCuMB:              clamp(b.UsableMB*2/100, 32, 256),
		HugeCodePages:       f.TransparentHugepages,
	}
	op.InternedStringsMB = clamp(op.MemoryMB/16, 8, 64)
	if o.AggressiveOpcache {
		p.Warnings = append(p.Warnings,
			"opcache.validate_timestamps=0: plugin, theme and core updates will not take effect until php-fpm is reloaded")
	}
	p.Notes = append(p.Notes, fmt.Sprintf(
		"opcache %s shared across all pools, %d file slots; APCu %s for the WordPress object cache",
		MemString(op.MemoryMB), op.MaxAcceleratedFiles, MemString(op.APCuMB)))

	return php, op
}

// opcacheFiles sizes the hash table for the number of PHP files on disk. A
// WordPress install with plugins is roughly 4-8k files.
func opcacheFiles(sites int) int {
	if sites <= 0 {
		sites = 4
	}
	return clamp(8000*sites, 16000, 130987)
}

// ---- database --------------------------------------------------------------

func computeDB(f facts.Facts, o Options, b Budget, php PHPPlan, p *Plan) DBPlan {
	db := DBPlan{
		Flavor:      string(f.DBFlavor),
		FlushMethod: "O_DIRECT",
		// WordPress does not need a synchronous flush per transaction; losing
		// at most one second of writes on an unclean shutdown is an acceptable
		// trade for removing an fsync from every request that writes.
		FlushLogAtTrxCommit: 2,
		MaxAllowedPacketMB:  64,
		SortBufferKB:        2048,
		JoinBufferKB:        256,
		ReadBufferKB:        128,
		ReadRndBufferKB:     256,
		KeyBufferMB:         32,
		SlowQueryLog:        true,
		SlowQueryTime:       2,
		CharacterSet:        "utf8mb4",
		Binlog:              o.EnableBinlog,
		BinlogExpireDays:    7,
	}
	if db.Flavor == "" {
		db.Flavor = string(facts.DBMariaDB)
	}

	// Only PHP workers, wp-cli and the odd admin session can open connections.
	// Sizing max_connections from RAM — as the legacy tuner did, at 100 per
	// gigabyte — produces thousands of connection slots that nothing will ever
	// use while charging real memory for the buffers behind them.
	db.MaxConnections = clamp(php.MaxChildren+25, 40, 1000)
	p.Notes = append(p.Notes, fmt.Sprintf(
		"max_connections=%d = php max_children (%d) + 25 for wp-cron, wp-cli and admin sessions",
		db.MaxConnections, php.MaxChildren))

	// The buffer pool gets whatever remains of the database budget after
	// per-connection buffers and fixed overheads are paid for.
	db.PerformanceSchema = b.UsableMB >= 4096
	overhead := 128
	if db.PerformanceSchema {
		overhead += 200
	}
	pool := b.DBMB - db.MaxConnections*perConnectionDBOverheadMB - overhead
	if pool < 96 {
		pool = 96
		p.Warnings = append(p.Warnings, fmt.Sprintf(
			"database budget (%d MB) barely covers connection buffers; buffer pool floored at 96 MB. Consider --profile=database or more RAM.",
			b.DBMB))
	}

	db.BufferPoolMB, db.BufferPoolChunkMB, db.BufferPoolInstances = alignBufferPool(pool)
	// MariaDB removed innodb_buffer_pool_instances in 10.5 and fails to start
	// if it is set.
	if facts.DBFlavor(db.Flavor) == facts.DBMariaDB {
		db.BufferPoolInstances = 0
	}
	p.Notes = append(p.Notes, fmt.Sprintf(
		"innodb_buffer_pool_size=%s = %d MB database budget − %d connections × %d MB buffers − %d MB fixed overhead, aligned to the %s chunk size",
		MemString(db.BufferPoolMB), b.DBMB, db.MaxConnections, perConnectionDBOverheadMB, overhead,
		MemString(db.BufferPoolChunkMB)))

	// Redo log sized against the buffer pool: too small and InnoDB stalls on
	// checkpoint flushes under write load; too large and crash recovery drags.
	db.LogFileSizeMB = roundTo(clamp(db.BufferPoolMB/4, 64, 2048), 64)
	db.LogBufferMB = clamp(db.LogFileSizeMB/16, 8, 64)
	db.UseRedoLogCapacity = facts.DBFlavor(db.Flavor) == facts.DBMySQL && f.DBVersionAtLeast(8, 1)

	// Storage class changes the right I/O settings by an order of magnitude.
	rotational := f.Storage.Known && f.Storage.Rotational
	if rotational {
		db.IOCapacity, db.IOCapacityMax = 200, 400
		db.FlushNeighbors = 1
		p.Notes = append(p.Notes, "rotational storage detected: low InnoDB I/O capacity and neighbour flushing enabled")
	} else {
		db.IOCapacity, db.IOCapacityMax = 2000, 8000
		db.FlushNeighbors = 0
		if !f.Storage.Known {
			p.Notes = append(p.Notes, "storage type could not be determined; assuming SSD/NVMe I/O capacity")
		}
	}

	db.ReadIOThreads = clamp(f.CPUCores, 4, 16)
	db.WriteIOThreads = clamp(f.CPUCores, 4, 16)
	db.PurgeThreads = clamp(f.CPUCores/2, 1, 8)
	db.ThreadCacheSize = clamp(db.MaxConnections/2, 8, 100)
	db.TableOpenCache = clamp(db.MaxConnections*10, 400, 8000)
	db.TableDefCache = clamp(db.TableOpenCache/2, 400, 4000)
	// Temporary tables live in memory until they exceed this, then spill to
	// disk. WordPress meta queries create them constantly, but the setting is
	// per-connection: a large value multiplied by max_connections is how
	// databases get OOM-killed.
	db.TmpTableMB = roundTo(clamp(b.DBMB*3/100, 32, 256), 16)

	switch facts.DBFlavor(db.Flavor) {
	case facts.DBMySQL:
		db.Variant = "MySQL " + f.DBVersion
		db.Collation = "utf8mb4_0900_ai_ci"
		db.ConfigPath = "/etc/mysql/mysql.conf.d/99-ngxsetup.cnf"
	default:
		db.Variant = "MariaDB " + f.DBVersion
		// General is available on every MariaDB release; the newer uca1400
		// collations are not, and an unknown collation prevents startup.
		db.Collation = "utf8mb4_general_ci"
		db.ConfigPath = "/etc/mysql/mariadb.conf.d/99-ngxsetup.cnf"
	}

	if db.Binlog {
		p.Warnings = append(p.Warnings,
			"binary logging enabled: expect additional write I/O and disk growth. Ensure binlogs are expired or backed up.")
	} else {
		p.Notes = append(p.Notes,
			"binary logging disabled: a single-server WordPress host gains nothing from it and pays in write amplification")
	}
	return db
}

// alignBufferPool rounds the pool down to a multiple of instances × chunk size.
// InnoDB silently rounds up otherwise, which would push it past its budget.
func alignBufferPool(poolMB int) (size, chunk, instances int) {
	switch {
	case poolMB < 256:
		chunk = 32
	case poolMB < 1024:
		chunk = 64
	default:
		chunk = 128
	}
	instances = 1
	if poolMB >= 1024 {
		instances = clamp(poolMB/1024, 1, 8)
	}
	unit := chunk * instances
	size = (poolMB / unit) * unit
	if size < unit {
		size = unit
	}
	return size, chunk, instances
}

// ---- nginx -----------------------------------------------------------------

func computeNginx(f facts.Facts, o Options, b Budget, php PHPPlan, p *Plan) NginxPlan {
	n := NginxPlan{
		WorkerProcesses:     "auto",
		MultiAccept:         true,
		KeepaliveTimeout:    65,
		KeepaliveRequests:   1000,
		ClientBodyBufferKB:  128,
		LargeHeaderBuffers:  "4 16k",
		FastCGIBuffers:      "16 32k",
		FastCGIBufferSizeKB: 32,
		CacheInactive:       "24h",
		CacheValid:          "12h",
		ReqRateGeneral:      15,
		ReqBurstGeneral:     30,
		ReqRateLogin:        20,
		ReqBurstLogin:       5,
		ConnPerIP:           100,
	}

	// The legacy config asked for 200000 worker_connections while allowing
	// only 80000 file descriptors — a limit nginx could never reach, on a box
	// that could not have supported it anyway. Size from memory instead, then
	// derive the descriptor limit from the connection count so the two agree.
	switch {
	case b.UsableMB < 512:
		n.WorkerConnections = 1024
	case b.UsableMB < 1024:
		n.WorkerConnections = 2048
	case b.UsableMB < 3072:
		n.WorkerConnections = 4096
	case b.UsableMB < 8192:
		n.WorkerConnections = 8192
	default:
		n.WorkerConnections = 16384
	}
	// Two descriptors per connection (client plus upstream), plus headroom for
	// open files, logs and the cache.
	n.WorkerRlimitNofile = n.WorkerConnections*2 + 2048
	p.Notes = append(p.Notes, fmt.Sprintf(
		"nginx: %d worker_connections per worker × %d cores ≈ %d concurrent connections; worker_rlimit_nofile=%d covers two descriptors each",
		n.WorkerConnections, f.CPUCores, n.WorkerConnections*f.CPUCores, n.WorkerRlimitNofile))

	n.ClientMaxBodyMB = php.UploadMaxMB
	n.OpenFileCacheMax = clamp(b.UsableMB*8, 2000, 100000)
	n.SSLSessionCacheMB = clamp(b.UsableMB/200, 4, 64)

	// Compression is CPU work on the request path; a single-core box cannot
	// afford the top levels.
	if f.CPUCores <= 1 {
		n.GzipCompLevel, n.BrotliCompLevel = 4, 4
	} else {
		n.GzipCompLevel, n.BrotliCompLevel = 5, 5
	}

	// The FastCGI micro-cache is the single most important performance lever
	// here: a cached response is served without touching PHP or MySQL. The
	// keys zone holds roughly 8000 keys per megabyte.
	n.CacheKeysZoneMB = clamp(b.UsableMB/100, 8, 256)
	if f.Storage.FreeMB > 0 {
		n.CacheMaxSizeMB = clamp(f.Storage.FreeMB*15/100, 256, 20480)
	} else {
		n.CacheMaxSizeMB = 1024
		p.Warnings = append(p.Warnings, "free disk space unknown; FastCGI cache capped at 1 GB")
	}
	p.Notes = append(p.Notes, fmt.Sprintf(
		"fastcgi micro-cache: %s keys zone (~%d cached URLs) backed by %s on disk, stale-while-revalidate enabled",
		MemString(n.CacheKeysZoneMB), n.CacheKeysZoneMB*8000, MemString(n.CacheMaxSizeMB)))

	return n
}

// ---- sysctl ----------------------------------------------------------------

func computeSysctl(f facts.Facts, n NginxPlan, p *Plan) []Setting {
	cores := max(f.CPUCores, 1)
	fileMax := max(2097152, cores*n.WorkerRlimitNofile*2)

	// Socket buffers scale with memory. The legacy configuration pinned every
	// TCP buffer to 30 MB regardless of machine size, which on a 1 GB box
	// reserves more memory for sockets than the box has.
	sockMaxMB := clamp(f.MemTotalMB/512, 4, 16)
	sockMax := sockMaxMB * 1024 * 1024

	s := []Setting{
		{"fs.file-max", itoa(fileMax), "system-wide descriptor ceiling, derived from nginx worker limits"},
		{"fs.nr_open", itoa(max(1048576, n.WorkerRlimitNofile*2)), "per-process descriptor ceiling"},

		{"net.core.somaxconn", "65535", "accept queue depth; nginx listen backlog cannot exceed this"},
		{"net.core.netdev_max_backlog", "16384", "packets queued when the NIC outpaces the kernel"},
		{"net.core.rmem_max", itoa(sockMax), "scaled to available memory"},
		{"net.core.wmem_max", itoa(sockMax), "scaled to available memory"},
		{"net.core.default_qdisc", "fq", "fair queueing, required for BBR pacing"},

		{"net.ipv4.tcp_max_syn_backlog", "8192", ""},
		{"net.ipv4.tcp_max_tw_buckets", "262144", ""},
		{"net.ipv4.tcp_fin_timeout", "15", "release FIN-WAIT-2 sockets sooner under churn"},
		{"net.ipv4.tcp_tw_reuse", "1", "reuse TIME-WAIT sockets for outbound connections"},
		{"net.ipv4.ip_local_port_range", "1024 65535", ""},
		{"net.ipv4.tcp_slow_start_after_idle", "0", "keep the congestion window across keepalive idle gaps"},
		{"net.ipv4.tcp_mtu_probing", "1", "survive paths with broken PMTU discovery"},
		{"net.ipv4.tcp_rmem", fmt.Sprintf("4096 87380 %d", sockMax), ""},
		{"net.ipv4.tcp_wmem", fmt.Sprintf("4096 65536 %d", sockMax), ""},

		// The legacy sysctl set disabled SYN cookies and enabled
		// tcp_tw_recycle. The first removes the kernel's SYN-flood defence;
		// the second was removed in Linux 4.12 because it silently breaks
		// clients behind NAT. Both are corrected here.
		{"net.ipv4.tcp_syncookies", "1", "SYN flood protection (the previous configuration disabled this)"},

		{"net.ipv4.conf.all.rp_filter", "1", "reverse-path filtering: drop spoofed source addresses"},
		{"net.ipv4.conf.default.rp_filter", "1", ""},
		{"net.ipv4.conf.all.accept_redirects", "0", "ignore ICMP redirects"},
		{"net.ipv4.conf.all.send_redirects", "0", ""},
		{"net.ipv4.conf.all.accept_source_route", "0", ""},
		{"net.ipv4.conf.all.log_martians", "1", ""},
		{"net.ipv6.conf.all.accept_redirects", "0", ""},

		{"vm.swappiness", "10", "prefer reclaiming page cache over swapping the working set"},
		{"vm.dirty_ratio", "20", ""},
		{"vm.dirty_background_ratio", "5", "start writeback early to avoid stalling bursts"},
		{"vm.overcommit_memory", "0", "kernel heuristic; MySQL dislikes strict overcommit accounting"},
	}

	// BBR needs both the algorithm and the fq queueing discipline. Selecting an
	// unavailable algorithm is a silent no-op, so check first.
	switch {
	case f.HasCongestionControl("bbr"):
		s = append(s, Setting{"net.ipv4.tcp_congestion_control", "bbr",
			"BBR sustains throughput on lossy and long-haul paths far better than CUBIC"})
		p.Notes = append(p.Notes, "TCP congestion control set to BBR with fq queueing")
	case len(f.CongestionControls) > 0:
		s = append(s, Setting{"net.ipv4.tcp_congestion_control", "cubic", "BBR unavailable on this kernel"})
		p.Notes = append(p.Notes, "BBR not available on this kernel; using CUBIC")
	}

	if f.Container {
		p.Warnings = append(p.Warnings,
			"running in a container: most sysctl values come from the host and cannot be set here. Network tuning will be skipped where the kernel refuses it.")
	}
	return s
}

// ---- validation ------------------------------------------------------------

// validate asserts the invariants that make this plan safe to apply. These
// checks exist because the failure mode of a bad autotuner is not a wrong
// number, it is a server that will not boot.
func (p *Plan) validate(f facts.Facts) {
	// Committed memory must fit. Worker count times average RSS is the honest
	// steady-state figure; memory_limit is a per-request ceiling that workers
	// do not all reach at once.
	phpCommitted := p.PHP.MaxChildren * p.PHP.AvgWorkerMB
	dbCommitted := p.DB.BufferPoolMB + p.DB.MaxConnections*perConnectionDBOverheadMB + 128
	committed := phpCommitted + dbCommitted + p.OPcache.MemoryMB + p.OPcache.APCuMB + 64

	if committed > p.Budget.UsableMB {
		p.Warnings = append(p.Warnings, fmt.Sprintf(
			"projected steady-state usage (%d MB) exceeds the usable budget (%d MB); consider more RAM or --profile=cache",
			committed, p.Budget.UsableMB))
	}
	if committed > p.Budget.TotalMB {
		p.Warnings = append(p.Warnings, fmt.Sprintf(
			"projected usage (%d MB) exceeds total memory (%d MB) — this configuration will swap or OOM",
			committed, p.Budget.TotalMB))
	}

	if p.PHP.MaxChildren < 4 {
		p.Warnings = append(p.Warnings, fmt.Sprintf(
			"only %d PHP workers fit in %d MB; this machine can serve cached traffic well but will queue uncached requests",
			p.PHP.MaxChildren, p.Budget.PHPMB))
	}
	if p.PHP.PM == "dynamic" {
		// FPM refuses to start if these are inconsistent.
		if p.PHP.MinSpareServers > p.PHP.MaxSpareServers ||
			p.PHP.MaxSpareServers > p.PHP.MaxChildren ||
			p.PHP.StartServers < p.PHP.MinSpareServers ||
			p.PHP.StartServers > p.PHP.MaxSpareServers {
			p.Warnings = append(p.Warnings, "internal: inconsistent php-fpm dynamic pool sizing")
		}
	}
	if f.SwapMB == 0 && p.Budget.TotalMB <= 2048 {
		p.Warnings = append(p.Warnings,
			"no swap configured on a small machine: a traffic spike will OOM-kill MySQL rather than slow down. Consider a 1 GB swapfile.")
	}
}

// ---- helpers ---------------------------------------------------------------

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func roundTo(v, unit int) int {
	if unit <= 0 {
		return v
	}
	r := (v / unit) * unit
	if r < unit {
		r = unit
	}
	return r
}

func itoa(v int) string { return fmt.Sprintf("%d", v) }
