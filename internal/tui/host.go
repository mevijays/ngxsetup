package tui

import (
	"os"
	"strconv"
	"strings"

	"ngxsetup/internal/facts"
)

// hostSummary is the header line: what the whole machine is doing, as
// context for the per-site rows below it. A site's CPU% only means something
// next to how many cores the box actually has.
type hostSummary struct {
	CPUCores   int
	MemTotalMB int
	MemUsedMB  int
	Load1      float64
	Load5      float64
}

// readHostSummary gathers the header data. Cheap enough to call on every
// tick — it is a couple of small file reads, not a subprocess.
func readHostSummary() hostSummary {
	f := facts.Detect(facts.OSSource{})
	load1, load5 := readLoadAvg()
	return hostSummary{
		CPUCores:   f.CPUCores,
		MemTotalMB: f.MemTotalMB,
		MemUsedMB:  f.MemTotalMB - f.MemAvailMB,
		Load1:      load1,
		Load5:      load5,
	}
}

func readLoadAvg() (one, five float64) {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0, 0
	}
	fields := strings.Fields(string(b))
	if len(fields) < 2 {
		return 0, 0
	}
	one, _ = strconv.ParseFloat(fields[0], 64)
	five, _ = strconv.ParseFloat(fields[1], 64)
	return one, five
}
