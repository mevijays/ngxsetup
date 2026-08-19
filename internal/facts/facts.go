// Package facts discovers what the machine actually is.
//
// Every tuning decision this tool makes is derived from these numbers, so the
// package is deliberately paranoid: it reads cgroup limits as well as host
// totals (a 64 GB host may hand a container 2 GB), it detects rotational vs
// solid-state storage (a 10x difference in the right InnoDB I/O settings), and
// it never guesses silently — unknown values are reported as unknown so the
// tuner can fall back to conservative defaults.
//
// All reads go through a Source so the whole package is testable off-Linux.
package facts

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// Source abstracts the filesystem so tests can supply a synthetic /proc.
type Source interface {
	ReadFile(path string) ([]byte, error)
	Exists(path string) bool
}

// OSSource reads the real filesystem.
type OSSource struct{}

func (OSSource) ReadFile(path string) ([]byte, error) { return os.ReadFile(path) }
func (OSSource) Exists(path string) bool              { _, err := os.Stat(path); return err == nil }

// MapSource is an in-memory Source for tests.
type MapSource map[string]string

func (m MapSource) ReadFile(path string) ([]byte, error) {
	v, ok := m[path]
	if !ok {
		return nil, os.ErrNotExist
	}
	return []byte(v), nil
}
func (m MapSource) Exists(path string) bool { _, ok := m[path]; return ok }

// OS identifies the distribution.
type OS struct {
	ID         string // "ubuntu", "debian"
	IDLike     string // "debian"
	VersionID  string // "24.04"
	Codename   string // "noble"
	PrettyName string
}

// DebianFamily reports whether apt-based provisioning applies.
func (o OS) DebianFamily() bool {
	return o.ID == "debian" || o.ID == "ubuntu" ||
		strings.Contains(o.IDLike, "debian") || strings.Contains(o.IDLike, "ubuntu")
}

// VersionAtLeast compares the dotted VersionID against major.minor.
func (o OS) VersionAtLeast(major, minor int) bool {
	ma, mi := splitVersion(o.VersionID)
	if ma != major {
		return ma > major
	}
	return mi >= minor
}

// Storage describes the device backing the site data directory.
type Storage struct {
	Path       string
	Device     string
	Rotational bool // true = spinning disk; drives InnoDB I/O capacity and flush neighbours
	Known      bool // false when the device could not be classified
	TotalMB    int
	FreeMB     int
}

// Facts is the complete machine profile the tuner consumes.
type Facts struct {
	OS   OS
	Arch string

	// CPUCores is the effective core count: host cores capped by any cgroup
	// CPU quota. Tuning must use this, not the host count, or a 2-core
	// container on a 64-core host gets sized for 64 cores.
	CPUCores     int
	CPUHostCores int
	CPUQuota     float64 // effective cores from cgroup quota; 0 when unlimited

	// MemTotalMB is likewise the effective limit: min(host RAM, cgroup limit).
	MemTotalMB  int
	MemHostMB   int
	MemCgroupMB int // 0 when unlimited
	MemAvailMB  int
	SwapMB      int

	Virt      string // "kvm", "lxc", "none", "" when undetermined
	Container bool
	Storage   Storage
	FileMax   int
	SELinux   bool

	// CongestionControls lists the TCP congestion algorithms the running
	// kernel can actually use. Selecting one that is not loaded silently
	// leaves the previous algorithm in place, so this must be checked.
	CongestionControls []string
	// TransparentHugepages is true when THP is available, which is a
	// precondition for opcache.huge_code_pages.
	TransparentHugepages bool

	// Software versions, populated by DetectSoftware. Empty means not installed.
	NginxVersion string
	PHPVersion   string // "8.3"
	DBFlavor     DBFlavor
	DBVersion    string
}

// DBFlavor distinguishes MariaDB from MySQL. This matters more than it looks:
// several settings the legacy tuner emitted unconditionally (utf8mb4_0900_ai_ci,
// default_authentication_plugin, innodb_dedicated_server) exist only on MySQL 8
// and prevent MariaDB from starting at all.
type DBFlavor string

const (
	DBNone    DBFlavor = ""
	DBMariaDB DBFlavor = "mariadb"
	DBMySQL   DBFlavor = "mysql"
)

// Detect builds the hardware and OS profile.
func Detect(src Source) Facts {
	f := Facts{Arch: runtime.GOARCH}
	f.OS = detectOS(src)

	f.CPUHostCores = detectHostCPU(src)
	f.CPUQuota = detectCPUQuota(src)
	f.CPUCores = f.CPUHostCores
	if f.CPUQuota > 0 {
		if eff := int(f.CPUQuota + 0.5); eff >= 1 && eff < f.CPUCores {
			f.CPUCores = eff
		}
	}
	if f.CPUCores < 1 {
		f.CPUCores = 1
	}

	f.MemHostMB, f.MemAvailMB, f.SwapMB = detectMemory(src)
	f.MemCgroupMB = detectMemoryCgroup(src)
	f.MemTotalMB = f.MemHostMB
	if f.MemCgroupMB > 0 && f.MemCgroupMB < f.MemTotalMB {
		f.MemTotalMB = f.MemCgroupMB
	}

	f.Virt, f.Container = detectVirt(src)
	f.FileMax = readInt(src, "/proc/sys/fs/file-max")
	f.SELinux = src.Exists("/sys/fs/selinux/enforce")

	if b, err := src.ReadFile("/proc/sys/net/ipv4/tcp_available_congestion_control"); err == nil {
		f.CongestionControls = strings.Fields(string(b))
	}
	if b, err := src.ReadFile("/sys/kernel/mm/transparent_hugepage/enabled"); err == nil {
		// The file reads like "always [madvise] never"; only a non-never
		// selection makes huge code pages usable.
		f.TransparentHugepages = !strings.Contains(string(b), "[never]")
	}
	return f
}

// HasCongestionControl reports whether the kernel offers the named algorithm.
func (f Facts) HasCongestionControl(name string) bool {
	for _, c := range f.CongestionControls {
		if c == name {
			return true
		}
	}
	return false
}

// DetectStorage classifies the device backing path and measures free space.
// Split from Detect because it needs a syscall, not just file reads.
func DetectStorage(src Source, path string) Storage {
	s := Storage{Path: path}
	s.Device = deviceFor(src, path)
	if s.Device != "" {
		if base := blockBase(s.Device); base != "" {
			if b, err := src.ReadFile("/sys/block/" + base + "/queue/rotational"); err == nil {
				s.Rotational = strings.TrimSpace(string(b)) == "1"
				s.Known = true
			}
		}
	}
	// Virtual and network filesystems report rotational=1 meaninglessly; a
	// wrong "spinning disk" verdict would cripple InnoDB on fast storage, so
	// prefer "unknown" and let the tuner use its non-rotational default.
	if strings.HasPrefix(s.Device, "/dev/mapper/") && !s.Known {
		s.Known = false
	}
	if total, free, err := diskUsage(path); err == nil {
		s.TotalMB, s.FreeMB = total, free
	}
	return s
}

// Runner is the subset of command execution DetectSoftware needs.
//
// CombinedOutput — not Output — is what version detection needs throughout:
// `nginx -v` writes its version banner to stderr, not stdout (this is
// documented nginx behaviour, confirmed against a real binary — `php -r` and
// `mariadbd --version` both use stdout, so nginx is the outlier, not the
// others). A Runner that only captured stdout left NginxVersion silently
// empty on every real machine, which in turn disabled
// Ctx.SupportsRejectHandshake() (so the unknown-SNI catch-all server was
// never rendered) and made Ctx.HTTP2Style() return the pre-1.25 syntax
// regardless of the nginx actually installed — correct by coincidence on an
// old nginx, wrong on anything 1.25.1+. Combining both streams for a
// version-string parse is safe generally: extractVersion just finds the
// first dotted numeric run in whatever came back.
type Runner interface {
	Output(ctx context.Context, name string, args ...string) (string, error)
	CombinedOutput(ctx context.Context, name string, args ...string) (string, error)
	Look(name string) bool
}

// DetectSoftware fills in versions of the stack components. Absent components
// leave their fields empty rather than erroring: setup runs before they exist.
func (f *Facts) DetectSoftware(ctx context.Context, r Runner) {
	if r.Look("nginx") {
		if out, err := r.CombinedOutput(ctx, "nginx", "-v"); err == nil {
			f.NginxVersion = extractVersion(out)
		}
	}
	if r.Look("php") {
		if out, err := r.Output(ctx, "php", "-r", "echo PHP_MAJOR_VERSION.'.'.PHP_MINOR_VERSION;"); err == nil {
			f.PHPVersion = strings.TrimSpace(out)
		}
	}
	// mysql/mariadb --version reports the flavour in its banner; on Debian the
	// mysql binary is frequently a MariaDB symlink, so the banner is the only
	// reliable discriminator.
	for _, bin := range []string{"mariadbd", "mysqld"} {
		if !r.Look(bin) {
			continue
		}
		out, err := r.CombinedOutput(ctx, bin, "--version")
		if err != nil {
			continue
		}
		low := strings.ToLower(out)
		f.DBVersion = extractVersion(out)
		if strings.Contains(low, "mariadb") {
			f.DBFlavor = DBMariaDB
		} else {
			f.DBFlavor = DBMySQL
		}
		break
	}
}

// PHPVersionAtLeast compares the detected PHP version.
func (f Facts) PHPVersionAtLeast(major, minor int) bool {
	ma, mi := splitVersion(f.PHPVersion)
	if ma != major {
		return ma > major
	}
	return mi >= minor
}

// DBVersionAtLeast compares the detected database version.
func (f Facts) DBVersionAtLeast(major, minor int) bool {
	ma, mi := splitVersion(f.DBVersion)
	if ma != major {
		return ma > major
	}
	return mi >= minor
}

// Describe renders the profile for status output.
func (f Facts) Describe() [][2]string {
	mem := fmt.Sprintf("%d MB", f.MemTotalMB)
	if f.MemCgroupMB > 0 && f.MemCgroupMB < f.MemHostMB {
		mem += fmt.Sprintf(" (cgroup-limited from %d MB)", f.MemHostMB)
	}
	cpu := strconv.Itoa(f.CPUCores)
	if f.CPUCores != f.CPUHostCores {
		cpu += fmt.Sprintf(" (quota-limited from %d)", f.CPUHostCores)
	}
	disk := "unknown"
	if f.Storage.TotalMB > 0 {
		kind := "SSD/NVMe"
		if !f.Storage.Known {
			kind = "unknown type, assuming SSD"
		} else if f.Storage.Rotational {
			kind = "rotational"
		}
		disk = fmt.Sprintf("%d MB free of %d MB (%s)", f.Storage.FreeMB, f.Storage.TotalMB, kind)
	}
	rows := [][2]string{
		{"os", strings.TrimSpace(f.OS.PrettyName + " " + f.Arch)},
		{"cpu", cpu},
		{"memory", mem},
		{"swap", fmt.Sprintf("%d MB", f.SwapMB)},
		{"storage", disk},
		{"virt", orDash(f.Virt)},
	}
	if f.NginxVersion != "" {
		rows = append(rows, [2]string{"nginx", f.NginxVersion})
	}
	if f.PHPVersion != "" {
		rows = append(rows, [2]string{"php", f.PHPVersion})
	}
	if f.DBFlavor != DBNone {
		rows = append(rows, [2]string{"database", string(f.DBFlavor) + " " + f.DBVersion})
	}
	return rows
}

// ---- internals -------------------------------------------------------------

func detectOS(src Source) OS {
	var o OS
	b, err := src.ReadFile("/etc/os-release")
	if err != nil {
		return o
	}
	for _, line := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		v = strings.Trim(v, `"'`)
		switch k {
		case "ID":
			o.ID = v
		case "ID_LIKE":
			o.IDLike = v
		case "VERSION_ID":
			o.VersionID = v
		case "VERSION_CODENAME":
			o.Codename = v
		case "PRETTY_NAME":
			o.PrettyName = v
		}
	}
	return o
}

func detectHostCPU(src Source) int {
	b, err := src.ReadFile("/proc/cpuinfo")
	if err != nil {
		return runtime.NumCPU()
	}
	n := 0
	sc := bufio.NewScanner(strings.NewReader(string(b)))
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "processor") {
			n++
		}
	}
	if n == 0 {
		return runtime.NumCPU()
	}
	return n
}

// detectCPUQuota returns the effective core count permitted by a cgroup CPU
// quota, or 0 when unlimited. Both cgroup v2 and v1 layouts are handled.
func detectCPUQuota(src Source) float64 {
	if b, err := src.ReadFile("/sys/fs/cgroup/cpu.max"); err == nil { // v2
		fields := strings.Fields(string(b))
		if len(fields) == 2 && fields[0] != "max" {
			quota, err1 := strconv.ParseFloat(fields[0], 64)
			period, err2 := strconv.ParseFloat(fields[1], 64)
			if err1 == nil && err2 == nil && period > 0 && quota > 0 {
				return quota / period
			}
		}
		return 0
	}
	quota := readInt(src, "/sys/fs/cgroup/cpu/cpu.cfs_quota_us") // v1
	period := readInt(src, "/sys/fs/cgroup/cpu/cpu.cfs_period_us")
	if quota > 0 && period > 0 {
		return float64(quota) / float64(period)
	}
	return 0
}

func detectMemory(src Source) (totalMB, availMB, swapMB int) {
	b, err := src.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0, 0
	}
	get := func(key string) int {
		for _, line := range strings.Split(string(b), "\n") {
			if !strings.HasPrefix(line, key+":") {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				kb, _ := strconv.Atoi(fields[1])
				return kb / 1024
			}
		}
		return 0
	}
	return get("MemTotal"), get("MemAvailable"), get("SwapTotal")
}

// detectMemoryCgroup returns the cgroup memory ceiling in MB, 0 if unlimited.
func detectMemoryCgroup(src Source) int {
	if b, err := src.ReadFile("/sys/fs/cgroup/memory.max"); err == nil { // v2
		s := strings.TrimSpace(string(b))
		if s == "max" {
			return 0
		}
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return int(n / (1 << 20))
		}
		return 0
	}
	if b, err := src.ReadFile("/sys/fs/cgroup/memory/memory.limit_in_bytes"); err == nil { // v1
		if n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); err == nil {
			// v1 signals "unlimited" with a value near the 64-bit page-aligned
			// maximum rather than a sentinel string.
			if n < (1<<62) && n > 0 {
				return int(n / (1 << 20))
			}
		}
	}
	return 0
}

func detectVirt(src Source) (string, bool) {
	// Container detection without shelling out to systemd-detect-virt, which
	// is unavailable in minimal images.
	if src.Exists("/.dockerenv") {
		return "docker", true
	}
	if b, err := src.ReadFile("/proc/1/environ"); err == nil {
		if strings.Contains(string(b), "container=lxc") {
			return "lxc", true
		}
	}
	if b, err := src.ReadFile("/sys/class/dmi/id/product_name"); err == nil {
		name := strings.TrimSpace(string(b))
		switch {
		case strings.Contains(name, "KVM"), strings.Contains(name, "Standard PC"):
			return "kvm", false
		case strings.Contains(name, "VMware"):
			return "vmware", false
		case strings.Contains(name, "VirtualBox"):
			return "virtualbox", false
		case strings.Contains(name, "Droplet"):
			return "kvm", false
		}
	}
	return "", false
}

// deviceFor resolves the block device backing a path by finding the longest
// matching mount point in /proc/mounts.
func deviceFor(src Source, path string) string {
	b, err := src.ReadFile("/proc/mounts")
	if err != nil {
		return ""
	}
	best, bestLen := "", -1
	path = filepath.Clean(path)
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || !strings.HasPrefix(fields[0], "/dev/") {
			continue
		}
		mp := filepath.Clean(fields[1])
		if mp == "/" || path == mp || strings.HasPrefix(path, mp+"/") {
			if len(mp) > bestLen {
				best, bestLen = fields[0], len(mp)
			}
		}
	}
	return best
}

// blockBase maps a partition device to its parent block device name:
// /dev/sda1 -> sda, /dev/nvme0n1p3 -> nvme0n1, /dev/vdb -> vdb.
func blockBase(dev string) string {
	name := strings.TrimPrefix(dev, "/dev/")
	if name == "" || strings.HasPrefix(name, "mapper/") {
		return ""
	}
	if i := strings.Index(name, "/"); i >= 0 {
		return ""
	}
	// nvme/mmcblk use a "p<N>" partition suffix; sd/vd/hd use a bare number.
	if strings.HasPrefix(name, "nvme") || strings.HasPrefix(name, "mmcblk") {
		if i := strings.LastIndex(name, "p"); i > 0 && allDigits(name[i+1:]) {
			return name[:i]
		}
		return name
	}
	return strings.TrimRight(name, "0123456789")
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func readInt(src Source, path string) int {
	b, err := src.ReadFile(path)
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0
	}
	return n
}

// extractVersion pulls the first dotted numeric run out of a version banner.
func extractVersion(s string) string {
	start := -1
	for i, r := range s {
		if r >= '0' && r <= '9' {
			if start < 0 {
				start = i
			}
			continue
		}
		if r == '.' && start >= 0 {
			continue
		}
		if start >= 0 {
			return strings.Trim(s[start:i], ".")
		}
	}
	if start >= 0 {
		return strings.Trim(s[start:], ".")
	}
	return ""
}

func splitVersion(v string) (int, int) {
	major, rest, _ := strings.Cut(v, ".")
	minor, _, _ := strings.Cut(rest, ".")
	ma, _ := strconv.Atoi(major)
	mi, _ := strconv.Atoi(minor)
	return ma, mi
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
