package facts

import (
	"context"
	"strings"
	"testing"
)

func base() MapSource {
	return MapSource{
		"/etc/os-release": `PRETTY_NAME="Ubuntu 24.04.1 LTS"
NAME="Ubuntu"
VERSION_ID="24.04"
ID=ubuntu
ID_LIKE=debian
VERSION_CODENAME=noble
`,
		"/proc/cpuinfo":                   "processor\t: 0\nprocessor\t: 1\nprocessor\t: 2\nprocessor\t: 3\n",
		"/proc/meminfo":                   "MemTotal:       8123456 kB\nMemFree: 100 kB\nMemAvailable:    6000000 kB\nSwapTotal:       2097152 kB\n",
		"/proc/mounts":                    "/dev/vda1 / ext4 rw 0 0\n/dev/vdb1 /var/www ext4 rw 0 0\n",
		"/sys/block/vdb/queue/rotational": "0\n",
	}
}

func TestDetectOS(t *testing.T) {
	f := Detect(base())
	if f.OS.ID != "ubuntu" || f.OS.VersionID != "24.04" || f.OS.Codename != "noble" {
		t.Fatalf("bad os parse: %+v", f.OS)
	}
	if !f.OS.DebianFamily() {
		t.Error("ubuntu should be debian family")
	}
	if !f.OS.VersionAtLeast(22, 4) || f.OS.VersionAtLeast(26, 0) {
		t.Error("version comparison wrong")
	}
}

func TestDetectCPUAndMemory(t *testing.T) {
	f := Detect(base())
	if f.CPUCores != 4 || f.CPUHostCores != 4 {
		t.Fatalf("cpu = %d/%d, want 4/4", f.CPUCores, f.CPUHostCores)
	}
	if f.MemTotalMB != 7933 {
		t.Fatalf("mem = %d MB, want 7933", f.MemTotalMB)
	}
	if f.SwapMB != 2048 {
		t.Fatalf("swap = %d MB, want 2048", f.SwapMB)
	}
}

// A container capped below the host must be tuned for the cap. Getting this
// wrong is how an autotuner OOM-kills a 2 GB container on a 64 GB host.
func TestCgroupV2LimitsWin(t *testing.T) {
	src := base()
	src["/sys/fs/cgroup/memory.max"] = "2147483648\n" // 2 GB
	src["/sys/fs/cgroup/cpu.max"] = "200000 100000\n" // 2 cores

	f := Detect(src)
	if f.MemTotalMB != 2048 {
		t.Errorf("effective mem = %d, want 2048", f.MemTotalMB)
	}
	if f.MemHostMB != 7933 {
		t.Errorf("host mem should still be recorded, got %d", f.MemHostMB)
	}
	if f.CPUCores != 2 {
		t.Errorf("effective cpu = %d, want 2", f.CPUCores)
	}
	if f.CPUHostCores != 4 {
		t.Errorf("host cpu should still be recorded, got %d", f.CPUHostCores)
	}
}

func TestCgroupV2UnlimitedIgnored(t *testing.T) {
	src := base()
	src["/sys/fs/cgroup/memory.max"] = "max\n"
	src["/sys/fs/cgroup/cpu.max"] = "max 100000\n"

	f := Detect(src)
	if f.MemTotalMB != 7933 || f.CPUCores != 4 {
		t.Fatalf("unlimited cgroup should not cap: mem=%d cpu=%d", f.MemTotalMB, f.CPUCores)
	}
}

func TestCgroupV1Limits(t *testing.T) {
	src := base()
	src["/sys/fs/cgroup/memory/memory.limit_in_bytes"] = "1073741824\n" // 1 GB
	src["/sys/fs/cgroup/cpu/cpu.cfs_quota_us"] = "50000\n"
	src["/sys/fs/cgroup/cpu/cpu.cfs_period_us"] = "100000\n"

	f := Detect(src)
	if f.MemTotalMB != 1024 {
		t.Errorf("v1 mem = %d, want 1024", f.MemTotalMB)
	}
	// Half a core rounds up to one; we never tune for zero cores.
	if f.CPUCores != 1 {
		t.Errorf("v1 cpu = %d, want 1", f.CPUCores)
	}
}

// cgroup v1 encodes "unlimited" as a huge page-aligned number, not a sentinel.
func TestCgroupV1UnlimitedSentinel(t *testing.T) {
	src := base()
	src["/sys/fs/cgroup/memory/memory.limit_in_bytes"] = "9223372036854771712\n"
	if f := Detect(src); f.MemTotalMB != 7933 {
		t.Fatalf("v1 unlimited sentinel should be ignored, got %d", f.MemTotalMB)
	}
}

func TestDetectStorageRotational(t *testing.T) {
	src := base()
	src["/sys/block/vdb/queue/rotational"] = "1\n"
	s := DetectStorage(src, "/var/www")
	if s.Device != "/dev/vdb1" {
		t.Fatalf("device = %q, want /dev/vdb1", s.Device)
	}
	if !s.Rotational || !s.Known {
		t.Fatalf("expected known rotational, got %+v", s)
	}
}

// The longest matching mount point must win, not the first or the root.
func TestDetectStoragePicksLongestMount(t *testing.T) {
	src := base()
	src["/proc/mounts"] = "/dev/vda1 / ext4 rw 0 0\n/dev/vdb1 /var ext4 rw 0 0\n/dev/vdc1 /var/www ext4 rw 0 0\n"
	src["/sys/block/vdc/queue/rotational"] = "0\n"
	if s := DetectStorage(src, "/var/www/example"); s.Device != "/dev/vdc1" {
		t.Fatalf("device = %q, want /dev/vdc1", s.Device)
	}
}

func TestDetectStorageUnknownDevice(t *testing.T) {
	src := base()
	src["/proc/mounts"] = "/dev/mapper/vg0-root / ext4 rw 0 0\n"
	s := DetectStorage(src, "/var/www")
	if s.Known {
		t.Fatal("device-mapper volumes must be reported as unknown, not guessed")
	}
}

func TestBlockBase(t *testing.T) {
	cases := map[string]string{
		"/dev/sda1":       "sda",
		"/dev/sda":        "sda",
		"/dev/vdb3":       "vdb",
		"/dev/nvme0n1p2":  "nvme0n1",
		"/dev/nvme0n1":    "nvme0n1",
		"/dev/mmcblk0p1":  "mmcblk0",
		"/dev/mapper/x-y": "",
	}
	for in, want := range cases {
		if got := blockBase(in); got != want {
			t.Errorf("blockBase(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDetectContainer(t *testing.T) {
	src := base()
	src["/.dockerenv"] = ""
	f := Detect(src)
	if !f.Container || f.Virt != "docker" {
		t.Fatalf("expected docker container, got virt=%q container=%v", f.Virt, f.Container)
	}
}

func TestExtractVersion(t *testing.T) {
	cases := map[string]string{
		"nginx version: nginx/1.24.0":                               "1.24.0",
		"mysqld  Ver 8.0.39-0ubuntu0.24.04.2 for Linux on x86_64":   "8.0.39",
		"mariadbd  Ver 10.11.8-MariaDB-0ubuntu0.24.04.1 for debian": "10.11.8",
		"": "",
	}
	for in, want := range cases {
		if got := extractVersion(in); got != want {
			t.Errorf("extractVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

// Missing files must degrade gracefully; setup runs on machines where these
// paths do not exist yet.
func TestDetectWithEmptySource(t *testing.T) {
	f := Detect(MapSource{})
	if f.CPUCores < 1 {
		t.Error("cpu cores must never be zero")
	}
	if f.MemTotalMB != 0 {
		t.Error("unknown memory should report zero, not a guess")
	}
}

// fakeRunner drives DetectSoftware in tests. Each binary's output is fixed
// per stream, so a test can reproduce a tool that writes its version banner
// to stderr exactly as a real one does.
type fakeRunner struct {
	installed map[string]bool
	stdout    map[string]string
	stderr    map[string]string
}

func (f fakeRunner) Look(name string) bool { return f.installed[name] }

func (f fakeRunner) Output(ctx context.Context, name string, args ...string) (string, error) {
	return f.stdout[name], nil
}

func (f fakeRunner) CombinedOutput(ctx context.Context, name string, args ...string) (string, error) {
	return strings.TrimSpace(f.stdout[name] + f.stderr[name]), nil
}

// This is the exact bug found testing against a real machine: `nginx -v`
// writes its version banner to stderr, not stdout. A Runner (or a
// DetectSoftware implementation) that only reads stdout leaves NginxVersion
// silently empty on every real install — which in turn disables
// ssl_reject_handshake and the HTTP/2 directive-style selection downstream,
// both of which key off the detected version. Reproduced here with a fake
// nginx that behaves exactly like the real one: nothing on stdout, the
// banner on stderr.
func TestDetectSoftwareReadsNginxVersionFromStderr(t *testing.T) {
	r := fakeRunner{
		installed: map[string]bool{"nginx": true},
		stdout:    map[string]string{"nginx": ""},
		stderr:    map[string]string{"nginx": "nginx version: nginx/1.24.0 (Ubuntu)\n"},
	}
	var f Facts
	f.DetectSoftware(context.Background(), r)
	if f.NginxVersion != "1.24.0" {
		t.Errorf("NginxVersion = %q, want 1.24.0 (nginx -v output lives on stderr)", f.NginxVersion)
	}
}

// PHP and the database clients are not affected — their version output is on
// stdout — but the detector must still get it right for them so the stderr
// fix does not become a stdout regression.
func TestDetectSoftwareReadsPHPAndDBVersionFromStdout(t *testing.T) {
	r := fakeRunner{
		installed: map[string]bool{"php": true, "mariadbd": true},
		stdout: map[string]string{
			"php":      "8.3",
			"mariadbd": "mariadbd  Ver 10.11.8-MariaDB-0ubuntu0.24.04.1 for debian",
		},
	}
	var f Facts
	f.DetectSoftware(context.Background(), r)
	if f.PHPVersion != "8.3" {
		t.Errorf("PHPVersion = %q, want 8.3", f.PHPVersion)
	}
	if f.DBVersion != "10.11.8" || f.DBFlavor != DBMariaDB {
		t.Errorf("DB = %q/%q, want 10.11.8/mariadb", f.DBVersion, f.DBFlavor)
	}
}

func TestDetectSoftwareLeavesAbsentComponentsEmpty(t *testing.T) {
	r := fakeRunner{installed: map[string]bool{}}
	var f Facts
	f.DetectSoftware(context.Background(), r)
	if f.NginxVersion != "" || f.PHPVersion != "" || f.DBFlavor != DBNone {
		t.Errorf("expected everything empty on a bare machine, got nginx=%q php=%q db=%q",
			f.NginxVersion, f.PHPVersion, f.DBFlavor)
	}
}

// versionAtLeast-driven decisions must fail closed on an empty version rather
// than guessing — an empty NginxVersion must not be treated as "recent
// enough."
func TestVersionAtLeastOnEmptyString(t *testing.T) {
	var f Facts
	if f.PHPVersionAtLeast(8, 0) {
		t.Error("an undetected PHP version must not satisfy a minimum-version check")
	}
	if f.DBVersionAtLeast(8, 0) {
		t.Error("an undetected DB version must not satisfy a minimum-version check")
	}
}
