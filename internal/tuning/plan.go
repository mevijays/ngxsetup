package tuning

import (
	"fmt"
	"strings"
)

// Profile selects how the memory budget is split between the three consumers.
type Profile string

const (
	// ProfileBalanced suits a handful of ordinary WordPress sites.
	ProfileBalanced Profile = "balanced"
	// ProfileCache maximises headroom for the page cache and FastCGI micro-cache.
	// This is the right answer for "huge load on small hardware": cached
	// responses never reach PHP or MySQL at all.
	ProfileCache Profile = "cache"
	// ProfileDensity favours many low-traffic sites: on-demand pools, small
	// per-site footprints.
	ProfileDensity Profile = "density"
	// ProfileDatabase favours large or query-heavy datasets (WooCommerce).
	ProfileDatabase Profile = "database"
)

// Profiles lists every valid profile, for CLI validation and help text.
var Profiles = []Profile{ProfileBalanced, ProfileCache, ProfileDensity, ProfileDatabase}

func ParseProfile(s string) (Profile, error) {
	p := Profile(strings.ToLower(strings.TrimSpace(s)))
	if s == "" {
		return ProfileBalanced, nil
	}
	for _, valid := range Profiles {
		if p == valid {
			return p, nil
		}
	}
	return "", fmt.Errorf("unknown profile %q (valid: %s)", s, joinProfiles())
}

func joinProfiles() string {
	out := make([]string, len(Profiles))
	for i, p := range Profiles {
		out[i] = string(p)
	}
	return strings.Join(out, ", ")
}

// Options are the operator-supplied inputs to Compute. Zero values mean
// "derive it", so callers only override what they actually care about.
type Options struct {
	Profile Profile

	// Sites is the number of WordPress sites the box will host. It affects
	// opcache file counts and pool sizing, not the memory split.
	Sites int

	// AvgPHPWorkerMB overrides the assumed steady-state RSS of one PHP-FPM
	// child. The default suits WordPress with a normal plugin set; sites
	// running page builders or WooCommerce should raise it.
	AvgPHPWorkerMB int

	// ReserveMB overrides the memory withheld for the kernel and base system.
	ReserveMB int

	// EnableBinlog turns on binary logging. Off by default: a single-server
	// WordPress host gets no benefit from it and pays in write amplification
	// and disk consumption.
	EnableBinlog bool

	// UploadMaxMB bounds PHP uploads and nginx client_max_body_size together.
	UploadMaxMB int

	// AggressiveOpcache disables opcache timestamp validation. Faster, but
	// plugin and theme updates then require an explicit PHP-FPM reload.
	AggressiveOpcache bool
}

// Budget is the memory allocation decision, in megabytes.
type Budget struct {
	TotalMB   int
	ReserveMB int
	UsableMB  int

	DBMB  int
	PHPMB int
	// FreeMB is deliberately unallocated memory. It is not waste: the kernel
	// page cache serves the FastCGI cache and every static asset out of it,
	// which is what lets a small box absorb large traffic.
	FreeMB int
}

// NginxPlan holds nginx-level tunables.
type NginxPlan struct {
	WorkerProcesses    string // "auto" or a number
	WorkerConnections  int
	WorkerRlimitNofile int
	MultiAccept        bool
	KeepaliveTimeout   int
	KeepaliveRequests  int

	ClientMaxBodyMB    int
	ClientBodyBufferKB int
	LargeHeaderBuffers string
	OpenFileCacheMax   int
	SSLSessionCacheMB  int
	GzipCompLevel      int
	BrotliCompLevel    int

	// FastCGI micro-cache.
	CacheKeysZoneMB     int
	CacheMaxSizeMB      int
	CacheInactive       string
	CacheValid          string
	FastCGIBuffers      string
	FastCGIBufferSizeKB int

	// Rate limiting. Sized generously enough not to trip real users while
	// still bounding brute-force and scraper traffic.
	ReqRateGeneral  int // requests/second per IP for normal pages
	ReqBurstGeneral int
	ReqRateLogin    int // requests/minute per IP for wp-login/xmlrpc
	ReqBurstLogin   int
	ConnPerIP       int
}

// PHPPlan holds PHP-FPM pool and php.ini values.
type PHPPlan struct {
	PM                 string // static | dynamic | ondemand
	MaxChildren        int
	StartServers       int
	MinSpareServers    int
	MaxSpareServers    int
	ProcessIdleTimeout string
	MaxRequests        int
	ListenBacklog      int
	RlimitFiles        int
	// AvgWorkerMB is the per-worker memory assumption the pool was sized
	// against. Recorded so validation and `tune --explain` can show the
	// arithmetic rather than a bare number.
	AvgWorkerMB int

	MemoryLimitMB           int
	CLIMemoryLimitMB        int
	UploadMaxMB             int
	MaxExecutionTime        int
	RequestTerminateTimeout int
	MaxInputVars            int
	SlowlogTimeout          int

	RealpathCacheKB  int
	RealpathCacheTTL int
}

// OPcachePlan holds opcache and APCu sizing.
type OPcachePlan struct {
	MemoryMB            int
	InternedStringsMB   int
	MaxAcceleratedFiles int
	ValidateTimestamps  bool
	RevalidateFreq      int
	APCuMB              int
	HugeCodePages       bool
}

// DBPlan holds database server settings. Rendering is flavour-aware because
// several of these directives exist on only one of MariaDB / MySQL.
type DBPlan struct {
	Flavor  string
	Variant string // human label, e.g. "MariaDB 10.11"

	BufferPoolMB      int
	BufferPoolChunkMB int
	// BufferPoolInstances is 0 when the directive must be omitted: MariaDB
	// removed it in 10.5 and refuses to start if it is present.
	BufferPoolInstances int
	// UseRedoLogCapacity selects innodb_redo_log_capacity (MySQL 8.0.30+)
	// over the deprecated innodb_log_file_size.
	UseRedoLogCapacity  bool
	LogFileSizeMB       int
	LogBufferMB         int
	FlushMethod         string
	FlushLogAtTrxCommit int
	FlushNeighbors      int
	IOCapacity          int
	IOCapacityMax       int
	ReadIOThreads       int
	WriteIOThreads      int
	PurgeThreads        int

	MaxConnections     int
	ThreadCacheSize    int
	TableOpenCache     int
	TableDefCache      int
	MaxAllowedPacketMB int
	TmpTableMB         int
	SortBufferKB       int
	JoinBufferKB       int
	ReadBufferKB       int
	ReadRndBufferKB    int
	KeyBufferMB        int

	PerformanceSchema bool
	Binlog            bool
	BinlogExpireDays  int
	SlowQueryLog      bool
	SlowQueryTime     float64
	CharacterSet      string
	Collation         string

	// ConfigPath is the drop-in this plan renders to. Writing a high-numbered
	// drop-in rather than replacing the distro config keeps the change
	// reversible and survives package upgrades.
	ConfigPath string
}

// Setting is one ordered key/value with an explanatory comment.
type Setting struct {
	Key     string
	Value   string
	Comment string
}

// LimitsPlan holds file-descriptor limits applied via systemd drop-ins.
type LimitsPlan struct {
	NginxNofile int
	PHPNofile   int
	DBNofile    int
}

// Plan is the complete, machine-specific configuration decision. It is a pure
// function of Facts plus Options, which is what makes it unit-testable without
// a server and reviewable before it is applied.
type Plan struct {
	Profile Profile
	Budget  Budget
	Nginx   NginxPlan
	PHP     PHPPlan
	OPcache OPcachePlan
	DB      DBPlan
	Sysctl  []Setting
	Limits  LimitsPlan

	// Notes explain why each major number was chosen. `ngxsetup tune --explain`
	// prints these; they are the difference between a tool an engineer trusts
	// and a black box.
	Notes []string
	// Warnings flag configurations that are legal but risky.
	Warnings []string
}

// MemString formats megabytes the way nginx, PHP and MySQL all accept.
func MemString(mb int) string {
	if mb >= 1024 && mb%1024 == 0 {
		return fmt.Sprintf("%dG", mb/1024)
	}
	return fmt.Sprintf("%dM", mb)
}

// Explain renders the derivation of the plan as human-readable lines.
func (p Plan) Explain() []string {
	out := []string{
		fmt.Sprintf("profile: %s", p.Profile),
		fmt.Sprintf("memory budget: %d MB total − %d MB reserved for the OS = %d MB usable",
			p.Budget.TotalMB, p.Budget.ReserveMB, p.Budget.UsableMB),
		fmt.Sprintf("  database   %5d MB", p.Budget.DBMB),
		fmt.Sprintf("  php-fpm    %5d MB", p.Budget.PHPMB),
		fmt.Sprintf("  page cache %5d MB (left free on purpose)", p.Budget.FreeMB),
	}
	return append(out, p.Notes...)
}

// Summary renders the headline numbers for status output.
func (p Plan) Summary() [][2]string {
	return [][2]string{
		{"profile", string(p.Profile)},
		{"nginx workers", fmt.Sprintf("%s × %d connections", p.Nginx.WorkerProcesses, p.Nginx.WorkerConnections)},
		{"fastcgi cache", fmt.Sprintf("%s keys zone, %s on disk", MemString(p.Nginx.CacheKeysZoneMB), MemString(p.Nginx.CacheMaxSizeMB))},
		{"php-fpm", fmt.Sprintf("pm=%s, max_children=%d, memory_limit=%s", p.PHP.PM, p.PHP.MaxChildren, MemString(p.PHP.MemoryLimitMB))},
		{"opcache", fmt.Sprintf("%s, %d files", MemString(p.OPcache.MemoryMB), p.OPcache.MaxAcceleratedFiles)},
		{"database", fmt.Sprintf("%s buffer pool, max_connections=%d", MemString(p.DB.BufferPoolMB), p.DB.MaxConnections)},
	}
}
