package servercollect

import (
	"context"
	"strconv"
	"strings"
	"time"

	"motty/internal/serverassets"
)

// basicScript is one read-only command that prints the data for BasicInfo.
// Each value is emitted as a MOT_* line or inside a marked section so the
// parser can tell fields apart; every sub-command tolerates failure so one
// missing tool does not lose the rest.
const basicScript = `
cat /etc/os-release 2>/dev/null
echo "MOT_KERNEL=$(uname -r 2>/dev/null)"
echo "MOT_ARCH=$(uname -m 2>/dev/null)"
echo "MOT_HOSTNAME=$(hostname 2>/dev/null || cat /proc/sys/kernel/hostname 2>/dev/null)"
echo "MOT_UPTIME=$(cat /proc/uptime 2>/dev/null)"
echo "MOT_BTIME=$(awk '/^btime/ {print $2}' /proc/stat 2>/dev/null)"
echo "MOT_MEMTOTAL_KB=$(awk '/^MemTotal:/ {print $2}' /proc/meminfo 2>/dev/null)"
echo "MOT_CPUINFO_BEGIN"
cat /proc/cpuinfo 2>/dev/null
echo "MOT_CPUINFO_END"
echo "MOT_LSCPU_BEGIN"
lscpu 2>/dev/null
echo "MOT_LSCPU_END"
`

func (c *collector) collectBasic(ctx context.Context, tr transport, _ serverassets.Server, now time.Time) MetricValue {
	data, err := tr.RunCommand(ctx, basicScript, 64<<10)
	if err != nil {
		return unavailable(MetricBasic, now, "basic info command failed: "+c.reason(err))
	}
	info := parseBasic(data, now)
	// Partial success is a success: only when no key field could be read
	// at all is the whole item unavailable (the missing fields stay empty
	// and are never filled with invented values).
	if info.OSName == "" && info.OSID == "" && info.Kernel == "" && info.Hostname == "" && info.TotalMem == 0 && info.CPUModel == "" {
		return unavailable(MetricBasic, now, "no basic system information could be read (os-release, uname, hostname, cpuinfo and meminfo all empty)")
	}
	return success(MetricBasic, now, func(v *MetricValue) { v.Basic = &info })
}

// parseBasic parses basicScript output. os-release is parsed generically
// (NAME/VERSION/ID), so CentOS, Rocky, AlmaLinux, Ubuntu, Debian,
// openEuler, Kylin, UOS and any other distribution with /etc/os-release
// work without per-distribution code; uname supplies arch/kernel.
func parseBasic(data []byte, now time.Time) BasicInfo {
	var info BasicInfo
	section := "os"
	var cpuinfo, lscpu []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimRight(line, "\r")
		switch line {
		case "MOT_CPUINFO_BEGIN":
			section = "cpuinfo"
			continue
		case "MOT_CPUINFO_END":
			section = "os"
			continue
		case "MOT_LSCPU_BEGIN":
			section = "lscpu"
			continue
		case "MOT_LSCPU_END":
			section = "os"
			continue
		}
		switch section {
		case "cpuinfo":
			cpuinfo = append(cpuinfo, line)
		case "lscpu":
			lscpu = append(lscpu, line)
		default:
			if strings.HasPrefix(line, "MOT_") {
				parseBasicMarker(&info, line, now)
				continue
			}
			parseOSReleaseLine(&info, line)
		}
	}
	parseCPUInfo(&info, cpuinfo)
	if info.CPUModel == "" || info.CPUThreads == 0 {
		parseLSCPU(&info, lscpu)
	}
	if info.Arch == "arm64" {
		info.Arch = "aarch64"
	}
	return info
}

func parseBasicMarker(info *BasicInfo, line string, now time.Time) {
	key, value, _ := strings.Cut(line, "=")
	switch key {
	case "MOT_KERNEL":
		info.Kernel = strings.TrimSpace(value)
	case "MOT_ARCH":
		info.Arch = strings.TrimSpace(value)
	case "MOT_HOSTNAME":
		info.Hostname = strings.TrimSpace(value)
	case "MOT_UPTIME":
		if fields := strings.Fields(value); len(fields) > 0 {
			if up, err := strconv.ParseFloat(fields[0], 64); err == nil && up >= 0 {
				info.UptimeSecs = up
			}
		}
	case "MOT_BTIME":
		if sec, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil && sec > 0 {
			info.BootTime = time.Unix(sec, 0)
		}
	case "MOT_MEMTOTAL_KB":
		if kb, err := strconv.ParseUint(strings.TrimSpace(value), 10, 64); err == nil {
			info.TotalMem = kb << 10
		}
	}
	// Boot time falls back to now minus uptime when /proc/stat has no btime.
	if info.BootTime.IsZero() && info.UptimeSecs > 0 {
		info.BootTime = now.Add(-time.Duration(info.UptimeSecs * float64(time.Second)))
	}
}

// parseOSReleaseLine reads one /etc/os-release KEY=VALUE line. Values may
// be quoted; quotes are stripped, anything else is kept verbatim.
func parseOSReleaseLine(info *BasicInfo, line string) {
	key, value, ok := strings.Cut(line, "=")
	if !ok {
		return
	}
	value = strings.Trim(strings.TrimSpace(value), `"'`)
	switch key {
	case "NAME":
		info.OSName = value
	case "VERSION":
		info.OSVersion = value
	case "ID":
		info.OSID = value
	}
}

// parseCPUInfo reads /proc/cpuinfo: the first model name, the thread count
// (processor entries) and the core count (distinct physical id + core id
// pairs; when the kernel does not report them, cores fall back to threads).
func parseCPUInfo(info *BasicInfo, lines []string) {
	type coreKey struct{ physical, core string }
	cores := make(map[coreKey]bool)
	physical, core := "", ""
	flush := func() {
		if physical != "" || core != "" {
			cores[coreKey{physical, core}] = true
		}
		physical, core = "", ""
	}
	for _, line := range lines {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		switch key {
		case "processor":
			info.CPUThreads++
			flush()
		case "model name":
			if info.CPUModel == "" {
				info.CPUModel = value
			}
		case "Hardware", "Model":
			if info.CPUModel == "" {
				info.CPUModel = value
			}
		case "physical id":
			physical = value
		case "core id":
			core = value
		}
	}
	flush()
	if len(cores) > 0 {
		info.CPUCores = len(cores)
	} else if info.CPUThreads > 0 {
		info.CPUCores = info.CPUThreads
	}
}

// parseLSCPU is the fallback when /proc/cpuinfo is missing or empty
// (model name, total CPUs, cores per socket x sockets).
func parseLSCPU(info *BasicInfo, lines []string) {
	var cpus, coresPerSocket, sockets int
	for _, line := range lines {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		switch key {
		case "Model name":
			if info.CPUModel == "" {
				info.CPUModel = value
			}
		case "CPU(s)":
			cpus, _ = strconv.Atoi(firstField(value))
		case "Core(s) per socket":
			coresPerSocket, _ = strconv.Atoi(firstField(value))
		case "Socket(s)":
			sockets, _ = strconv.Atoi(firstField(value))
		}
	}
	if info.CPUThreads == 0 && cpus > 0 {
		info.CPUThreads = cpus
	}
	if info.CPUCores == 0 && coresPerSocket > 0 && sockets > 0 {
		info.CPUCores = coresPerSocket * sockets
	}
	if info.CPUCores == 0 && info.CPUThreads > 0 {
		info.CPUCores = info.CPUThreads
	}
}

// firstField returns the first whitespace-separated field, or "".
func firstField(value string) string {
	if fields := strings.Fields(value); len(fields) > 0 {
		return fields[0]
	}
	return ""
}
