package tuning

import (
	"fmt"
	"strings"
	"testing"

	"ngxsetup/internal/facts"
)

// machine builds synthetic hardware. Every sizing test runs against these
// rather than a real server, which is the point of keeping Compute pure.
func machine(memMB, cores int, opts ...func(*facts.Facts)) facts.Facts {
	f := facts.Facts{
		OS:           facts.OS{ID: "ubuntu", VersionID: "24.04", PrettyName: "Ubuntu 24.04 LTS"},
		CPUCores:     cores,
		CPUHostCores: cores,
		MemTotalMB:   memMB,
		MemHostMB:    memMB,
		SwapMB:       memMB / 2,
		DBFlavor:     facts.DBMariaDB,
		DBVersion:    "10.11.8",
		PHPVersion:   "8.3",
		NginxVersion: "1.24.0",
		Storage: facts.Storage{
			Known: true, Rotational: false,
			TotalMB: 50000, FreeMB: 40000,
		},
		CongestionControls: []string{"reno", "cubic", "bbr"},
	}
	for _, o := range opts {
		o(&f)
	}
	return f
}

func rotational(f *facts.Facts)  { f.Storage.Rotational = true }
func unknownDisk(f *facts.Facts) { f.Storage.Known = false }
func mysql8(f *facts.Facts)      { f.DBFlavor = facts.DBMySQL; f.DBVersion = "8.4.0" }
func noSwap(f *facts.Facts)      { f.SwapMB = 0 }

// The fleet every invariant test sweeps: from a 1 GB/1 core budget VPS to a
// 64 GB/32 core dedicated box.
var fleet = []struct {
	name  string
	mem   int
	cores int
}{
	{"1GB/1core", 1024, 1},
	{"2GB/1core", 2048, 1},
	{"2GB/2core", 2048, 2},
	{"4GB/2core", 4096, 2},
	{"8GB/4core", 8192, 4},
	{"16GB/8core", 16384, 8},
	{"32GB/16core", 32768, 16},
	{"64GB/32core", 65536, 32},
}

// The central safety property: the plan must never commit more memory than the
// machine has. An autotuner that violates this does not produce a slow server,
// it produces one that OOM-kills MySQL under load.
func TestNeverOvercommitsMemory(t *testing.T) {
	for _, m := range fleet {
		for _, prof := range Profiles {
			t.Run(fmt.Sprintf("%s/%s", m.name, prof), func(t *testing.T) {
				p := Compute(machine(m.mem, m.cores), Options{Profile: prof})

				php := p.PHP.MaxChildren * p.PHP.AvgWorkerMB
				db := p.DB.BufferPoolMB + p.DB.MaxConnections*perConnectionDBOverheadMB
				total := php + db + p.OPcache.MemoryMB + p.OPcache.APCuMB

				if total > p.Budget.UsableMB {
					t.Errorf("committed %d MB > usable %d MB (php=%d db=%d opcache=%d apcu=%d)",
						total, p.Budget.UsableMB, php, db, p.OPcache.MemoryMB, p.OPcache.APCuMB)
				}
				// And leave genuine headroom below total RAM for the page cache.
				if total > p.Budget.TotalMB*85/100 {
					t.Errorf("committed %d MB leaves too little page cache on a %d MB machine",
						total, p.Budget.TotalMB)
				}
			})
		}
	}
}

// Every emitted value must be usable by the service that consumes it. A zero
// or negative buffer pool, worker count or connection limit is a service that
// refuses to start.
func TestAllValuesArePositiveAndSane(t *testing.T) {
	for _, m := range fleet {
		t.Run(m.name, func(t *testing.T) {
			p := Compute(machine(m.mem, m.cores), Options{})

			checks := map[string]int{
				"php max_children":     p.PHP.MaxChildren,
				"php memory_limit":     p.PHP.MemoryLimitMB,
				"opcache memory":       p.OPcache.MemoryMB,
				"opcache files":        p.OPcache.MaxAcceleratedFiles,
				"db buffer pool":       p.DB.BufferPoolMB,
				"db max_connections":   p.DB.MaxConnections,
				"db log file size":     p.DB.LogFileSizeMB,
				"db tmp table":         p.DB.TmpTableMB,
				"nginx connections":    p.Nginx.WorkerConnections,
				"nginx rlimit_nofile":  p.Nginx.WorkerRlimitNofile,
				"nginx cache keys":     p.Nginx.CacheKeysZoneMB,
				"nginx cache max size": p.Nginx.CacheMaxSizeMB,
			}
			for name, v := range checks {
				if v <= 0 {
					t.Errorf("%s = %d, must be positive", name, v)
				}
			}
			// nginx refuses to start when the descriptor limit cannot cover
			// the connections it is told to accept. The legacy config asked
			// for 200000 connections against 80000 descriptors.
			if p.Nginx.WorkerRlimitNofile < p.Nginx.WorkerConnections*2 {
				t.Errorf("worker_rlimit_nofile=%d cannot cover %d connections",
					p.Nginx.WorkerRlimitNofile, p.Nginx.WorkerConnections)
			}
		})
	}
}

// max_connections must follow from the number of things that can connect, not
// from RAM. The legacy tuner used memGB*100, giving a 32 GB box 3200 slots
// against a pool that could open at most a few hundred.
func TestMaxConnectionsDerivesFromPHPWorkers(t *testing.T) {
	for _, m := range fleet {
		p := Compute(machine(m.mem, m.cores), Options{})
		want := p.PHP.MaxChildren + 25
		if want < 40 {
			want = 40
		}
		if p.DB.MaxConnections != want {
			t.Errorf("%s: max_connections=%d, want %d (max_children=%d)",
				m.name, p.DB.MaxConnections, want, p.PHP.MaxChildren)
		}
		if p.DB.MaxConnections > 1000 {
			t.Errorf("%s: max_connections=%d is absurd for a WordPress host", m.name, p.DB.MaxConnections)
		}
	}
}

// PHP workers are bounded by CPU as well as memory; a 64 GB single-core box
// must not be given hundreds of workers it cannot schedule.
func TestPHPWorkersBoundedByCPU(t *testing.T) {
	p := Compute(machine(65536, 1), Options{})
	if p.PHP.MaxChildren > 8 {
		t.Errorf("1 core with 64 GB got %d workers; CPU should bind at 8", p.PHP.MaxChildren)
	}
	if !containsSubstr(p.Notes, "CPU is the binding constraint") {
		t.Error("plan should explain that CPU was the binding constraint")
	}
}

// ...and bounded by memory when memory is the scarce resource.
func TestPHPWorkersBoundedByMemory(t *testing.T) {
	p := Compute(machine(1024, 16), Options{})
	if p.PHP.MaxChildren > 6 {
		t.Errorf("1 GB with 16 cores got %d workers; memory should bind", p.PHP.MaxChildren)
	}
	if !containsSubstr(p.Notes, "memory is the binding constraint") {
		t.Error("plan should explain that memory was the binding constraint")
	}
}

// FPM validates these relationships at startup and exits if they are violated.
func TestDynamicPoolInvariants(t *testing.T) {
	for _, m := range fleet {
		p := Compute(machine(m.mem, m.cores), Options{})
		if p.PHP.PM != "dynamic" {
			continue
		}
		if p.PHP.MinSpareServers > p.PHP.MaxSpareServers {
			t.Errorf("%s: min_spare(%d) > max_spare(%d)", m.name, p.PHP.MinSpareServers, p.PHP.MaxSpareServers)
		}
		if p.PHP.MaxSpareServers > p.PHP.MaxChildren {
			t.Errorf("%s: max_spare(%d) > max_children(%d)", m.name, p.PHP.MaxSpareServers, p.PHP.MaxChildren)
		}
		if p.PHP.StartServers < p.PHP.MinSpareServers || p.PHP.StartServers > p.PHP.MaxSpareServers {
			t.Errorf("%s: start_servers(%d) outside [%d,%d]", m.name,
				p.PHP.StartServers, p.PHP.MinSpareServers, p.PHP.MaxSpareServers)
		}
		if p.PHP.MinSpareServers < 1 {
			t.Errorf("%s: min_spare must be at least 1", m.name)
		}
	}
}

// Small and dense machines must not keep idle workers resident.
func TestOndemandOnConstrainedMachines(t *testing.T) {
	if p := Compute(machine(1024, 1), Options{}); p.PHP.PM != "ondemand" {
		t.Errorf("1 GB machine got pm=%s, want ondemand", p.PHP.PM)
	}
	if p := Compute(machine(8192, 4), Options{Profile: ProfileDensity}); p.PHP.PM != "ondemand" {
		t.Errorf("density profile got pm=%s, want ondemand", p.PHP.PM)
	}
	if p := Compute(machine(8192, 4), Options{Sites: 20}); p.PHP.PM != "ondemand" {
		t.Errorf("20 sites got pm=%s, want ondemand", p.PHP.PM)
	}
	if p := Compute(machine(8192, 4), Options{}); p.PHP.PM != "dynamic" {
		t.Errorf("ordinary 8 GB box got pm=%s, want dynamic", p.PHP.PM)
	}
}

// MariaDB 10.5 removed innodb_buffer_pool_instances and refuses to start when
// it is present. Emitting it was a latent startup failure in the old tuner.
func TestMariaDBOmitsBufferPoolInstances(t *testing.T) {
	p := Compute(machine(16384, 8), Options{})
	if p.DB.BufferPoolInstances != 0 {
		t.Errorf("MariaDB plan set buffer_pool_instances=%d; the directive must be omitted",
			p.DB.BufferPoolInstances)
	}
	if !strings.Contains(p.DB.ConfigPath, "mariadb.conf.d") {
		t.Errorf("MariaDB config path = %q, want a mariadb.conf.d drop-in", p.DB.ConfigPath)
	}
	if p.DB.Collation != "utf8mb4_general_ci" {
		t.Errorf("MariaDB collation = %q; utf8mb4_0900_ai_ci does not exist there", p.DB.Collation)
	}
}

func TestMySQLGetsMySQLSpecificSettings(t *testing.T) {
	p := Compute(machine(16384, 8, mysql8), Options{})
	if p.DB.BufferPoolInstances < 1 {
		t.Error("MySQL supports buffer_pool_instances and should set it")
	}
	if !strings.Contains(p.DB.ConfigPath, "mysql.conf.d") {
		t.Errorf("MySQL config path = %q, want a mysql.conf.d drop-in", p.DB.ConfigPath)
	}
	if p.DB.Collation != "utf8mb4_0900_ai_ci" {
		t.Errorf("MySQL 8 collation = %q", p.DB.Collation)
	}
	if !p.DB.UseRedoLogCapacity {
		t.Error("MySQL 8.4 should use innodb_redo_log_capacity, not the deprecated log_file_size")
	}
}

// InnoDB rounds the buffer pool up to a multiple of instances × chunk size.
// Rounding down ourselves is what keeps it inside its budget.
func TestBufferPoolAlignment(t *testing.T) {
	for _, m := range fleet {
		p := Compute(machine(m.mem, m.cores), Options{})
		inst := max(p.DB.BufferPoolInstances, 1)
		unit := p.DB.BufferPoolChunkMB * inst
		if unit <= 0 {
			t.Fatalf("%s: bad chunk/instance combination", m.name)
		}
		if p.DB.BufferPoolMB%unit != 0 {
			t.Errorf("%s: buffer pool %d MB is not a multiple of %d MB (chunk %d × %d instances)",
				m.name, p.DB.BufferPoolMB, unit, p.DB.BufferPoolChunkMB, inst)
		}
	}
}

// Storage class changes the right InnoDB I/O settings by an order of magnitude.
func TestStorageClassDrivesIOSettings(t *testing.T) {
	ssd := Compute(machine(8192, 4), Options{})
	hdd := Compute(machine(8192, 4, rotational), Options{})

	if ssd.DB.IOCapacity <= hdd.DB.IOCapacity {
		t.Errorf("SSD io_capacity (%d) should exceed rotational (%d)", ssd.DB.IOCapacity, hdd.DB.IOCapacity)
	}
	if ssd.DB.FlushNeighbors != 0 || hdd.DB.FlushNeighbors != 1 {
		t.Errorf("flush_neighbors: ssd=%d hdd=%d, want 0 and 1", ssd.DB.FlushNeighbors, hdd.DB.FlushNeighbors)
	}
	// An unknown device must not be guessed as rotational; that would cripple
	// fast storage.
	unk := Compute(machine(8192, 4, unknownDisk), Options{})
	if unk.DB.IOCapacity != ssd.DB.IOCapacity {
		t.Error("unknown storage should default to the SSD profile, not the rotational one")
	}
}

// Profiles must actually move the budget in the direction they promise.
func TestProfilesShiftTheBudget(t *testing.T) {
	m := machine(8192, 4)
	balanced := Compute(m, Options{Profile: ProfileBalanced})
	cache := Compute(m, Options{Profile: ProfileCache})
	dbHeavy := Compute(m, Options{Profile: ProfileDatabase})

	if cache.Budget.FreeMB <= balanced.Budget.FreeMB {
		t.Errorf("cache profile free memory %d should exceed balanced %d",
			cache.Budget.FreeMB, balanced.Budget.FreeMB)
	}
	if dbHeavy.DB.BufferPoolMB <= balanced.DB.BufferPoolMB {
		t.Errorf("database profile buffer pool %d should exceed balanced %d",
			dbHeavy.DB.BufferPoolMB, balanced.DB.BufferPoolMB)
	}
	if dbHeavy.PHP.MaxChildren > balanced.PHP.MaxChildren {
		t.Error("database profile should not also increase PHP workers")
	}
}

// Regression guards against the specific dangerous values the old stack shipped.
func TestSysctlFixesLegacyMistakes(t *testing.T) {
	p := Compute(machine(8192, 4), Options{})
	got := map[string]string{}
	for _, s := range p.Sysctl {
		got[s.Key] = s.Value
	}

	if got["net.ipv4.tcp_syncookies"] != "1" {
		t.Error("SYN cookies must be enabled; the legacy sysctl set them to 0")
	}
	if _, present := got["net.ipv4.tcp_tw_recycle"]; present {
		t.Error("tcp_tw_recycle was removed from Linux 4.12 and breaks NAT clients; it must not be emitted")
	}
	if got["net.ipv4.tcp_congestion_control"] != "bbr" {
		t.Errorf("congestion control = %q, want bbr when available", got["net.ipv4.tcp_congestion_control"])
	}
	if got["net.core.default_qdisc"] != "fq" {
		t.Error("BBR requires the fq queueing discipline")
	}
	if got["vm.swappiness"] == "1" {
		t.Error("swappiness=1 risks OOM instead of graceful degradation on small machines")
	}
}

// Socket buffers must scale with the machine. The legacy set pinned 30 MB
// buffers on every box, which exceeds the total RAM of a 1 GB VPS.
func TestSocketBuffersScaleWithMemory(t *testing.T) {
	small := sysctlValue(Compute(machine(1024, 1), Options{}), "net.core.rmem_max")
	large := sysctlValue(Compute(machine(65536, 32), Options{}), "net.core.rmem_max")
	if small == large {
		t.Error("socket buffers should differ between a 1 GB and a 64 GB machine")
	}
	if small > 1024*1024*8 {
		t.Errorf("1 GB machine got %d byte socket buffers", small)
	}
}

func TestNoCongestionControlWhenKernelListUnknown(t *testing.T) {
	f := machine(8192, 4)
	f.CongestionControls = nil
	p := Compute(f, Options{})
	for _, s := range p.Sysctl {
		if s.Key == "net.ipv4.tcp_congestion_control" {
			t.Error("must not set a congestion algorithm without knowing which are available")
		}
	}
}

// A missing /proc/meminfo must not produce a plan sized for an imaginary
// machine; it must produce a small, safe plan plus a warning.
func TestUnknownMemoryIsConservative(t *testing.T) {
	f := machine(8192, 4)
	f.MemTotalMB = 0
	p := Compute(f, Options{})
	if p.Budget.TotalMB != 1024 {
		t.Errorf("unknown memory assumed %d MB, want a conservative 1024", p.Budget.TotalMB)
	}
	if !containsSubstr(p.Warnings, "could not read total memory") {
		t.Error("unknown memory must produce a warning")
	}
}

func TestSmallMachineWithoutSwapIsWarned(t *testing.T) {
	p := Compute(machine(1024, 1, noSwap), Options{})
	if !containsSubstr(p.Warnings, "no swap configured") {
		t.Error("a 1 GB machine with no swap should be warned about OOM risk")
	}
}

func TestOptionsOverrideDerivedValues(t *testing.T) {
	p := Compute(machine(8192, 4), Options{AvgPHPWorkerMB: 256, ReserveMB: 2048, UploadMaxMB: 64})
	if p.Budget.ReserveMB != 2048 {
		t.Errorf("reserve override ignored: %d", p.Budget.ReserveMB)
	}
	if p.PHP.AvgWorkerMB != 256 {
		t.Errorf("worker size override ignored: %d", p.PHP.AvgWorkerMB)
	}
	if p.PHP.UploadMaxMB != 64 || p.Nginx.ClientMaxBodyMB != 64 {
		t.Errorf("upload limit must apply to both php and nginx: php=%d nginx=%d",
			p.PHP.UploadMaxMB, p.Nginx.ClientMaxBodyMB)
	}
	heavy := Compute(machine(8192, 4), Options{AvgPHPWorkerMB: 256})
	light := Compute(machine(8192, 4), Options{AvgPHPWorkerMB: 40})
	if heavy.PHP.MaxChildren >= light.PHP.MaxChildren {
		t.Error("heavier workers must yield fewer of them")
	}
}

// nginx client_max_body_size below PHP's post_max_size produces a confusing
// 413 that looks like a PHP problem; they must always agree.
func TestUploadLimitsAgreeAcrossServices(t *testing.T) {
	for _, m := range fleet {
		p := Compute(machine(m.mem, m.cores), Options{})
		if p.Nginx.ClientMaxBodyMB != p.PHP.UploadMaxMB {
			t.Errorf("%s: nginx %d MB vs php %d MB", m.name, p.Nginx.ClientMaxBodyMB, p.PHP.UploadMaxMB)
		}
	}
}

// A five-hour execution limit turns one stuck request into an outage on a
// small pool.
func TestExecutionTimeIsBounded(t *testing.T) {
	p := Compute(machine(8192, 4), Options{})
	if p.PHP.MaxExecutionTime > 600 {
		t.Errorf("max_execution_time=%d is long enough to exhaust the pool", p.PHP.MaxExecutionTime)
	}
	if p.PHP.RequestTerminateTimeout <= p.PHP.MaxExecutionTime {
		t.Error("FPM's hard timeout must sit above PHP's own limit so PHP reports the error first")
	}
}

func TestOpcacheValidationDefaultsOn(t *testing.T) {
	p := Compute(machine(8192, 4), Options{})
	if !p.OPcache.ValidateTimestamps {
		t.Error("timestamp validation must default on, or WordPress updates silently do nothing")
	}
	agg := Compute(machine(8192, 4), Options{AggressiveOpcache: true})
	if agg.OPcache.ValidateTimestamps {
		t.Error("--aggressive-opcache should disable validation")
	}
	if !containsSubstr(agg.Warnings, "will not take effect until php-fpm is reloaded") {
		t.Error("disabling validation must warn about the operational consequence")
	}
}

// Determinism: the same inputs must always produce the same plan, or diffs and
// rollbacks become meaningless.
func TestComputeIsDeterministic(t *testing.T) {
	m := machine(8192, 4)
	a := Compute(m, Options{Profile: ProfileBalanced, Sites: 3})
	b := Compute(m, Options{Profile: ProfileBalanced, Sites: 3})
	if fmt.Sprintf("%+v", a) != fmt.Sprintf("%+v", b) {
		t.Error("Compute is not deterministic")
	}
}

func TestExplainCoversEveryMajorDecision(t *testing.T) {
	p := Compute(machine(8192, 4), Options{})
	text := strings.Join(p.Explain(), "\n")
	for _, want := range []string{"max_children", "max_connections", "innodb_buffer_pool_size", "worker_connections", "micro-cache"} {
		if !strings.Contains(text, want) {
			t.Errorf("explain output does not mention %q", want)
		}
	}
}

// Each site now runs its own PHP-FPM master (the cost of per-site mount
// namespace isolation), and that memory is spent whether the site serves a
// request or not. If the budget did not charge for it, the worker count
// would be sized against memory the masters had already taken — the exact
// over-commit this engine exists to prevent.
func TestBudgetChargesForPerSiteFPMMasters(t *testing.T) {
	m := machine(8192, 4)
	none := Compute(m, Options{Sites: 0})
	ten := Compute(m, Options{Sites: 10})

	wantCharge := 10 * fpmMasterMB
	if diff := none.Budget.UsableMB - ten.Budget.UsableMB; diff != wantCharge {
		t.Errorf("usable memory dropped by %d MB for 10 sites, want %d (%d MB each)",
			diff, wantCharge, fpmMasterMB)
	}
	if ten.Budget.PHPMB >= none.Budget.PHPMB {
		t.Error("the PHP worker budget should shrink once master processes are charged for")
	}
}

// A pathological site count on a small box must not drive the budget to zero
// or negative; it should cap the charge and say so.
func TestBudgetCapsMasterChargeOnOvercrowdedHost(t *testing.T) {
	p := Compute(machine(1024, 1), Options{Sites: 200})
	if p.Budget.UsableMB <= 0 {
		t.Fatalf("usable memory collapsed to %d MB", p.Budget.UsableMB)
	}
	if p.PHP.MaxChildren < 1 {
		t.Error("worker count must stay at least 1 even on an overcrowded host")
	}
	if !containsSubstr(p.Warnings, "exceeds a third of usable memory") {
		t.Errorf("expected a warning about too many sites for this host, got %v", p.Warnings)
	}
}

func TestParseProfile(t *testing.T) {
	if p, err := ParseProfile(""); err != nil || p != ProfileBalanced {
		t.Errorf("empty profile should default to balanced, got %v %v", p, err)
	}
	if _, err := ParseProfile("turbo"); err == nil {
		t.Error("unknown profile should be rejected")
	}
	if p, err := ParseProfile("CACHE"); err != nil || p != ProfileCache {
		t.Errorf("profile parsing should be case-insensitive, got %v %v", p, err)
	}
}

func TestMemString(t *testing.T) {
	cases := map[int]string{512: "512M", 1024: "1G", 1536: "1536M", 2048: "2G", 96: "96M"}
	for in, want := range cases {
		if got := MemString(in); got != want {
			t.Errorf("MemString(%d) = %q, want %q", in, got, want)
		}
	}
}

// ---- helpers ---------------------------------------------------------------

func containsSubstr(list []string, want string) bool {
	for _, s := range list {
		if strings.Contains(s, want) {
			return true
		}
	}
	return false
}

func sysctlValue(p Plan, key string) int {
	for _, s := range p.Sysctl {
		if s.Key == key {
			var n int
			fmt.Sscanf(s.Value, "%d", &n)
			return n
		}
	}
	return 0
}
