package servercollect

import (
	"context"
	"strconv"
	"strings"

	"motty/internal/remotestats"
)

// cpuFromSample converts a remotestats Sample into CPUInfo.
//
// Sample.CPUPercent is already a percentage (0-100): remotestats.sample
// computes (1 - idleDelta/totalDelta) * 100 from two /proc/stat readings,
// so no rescaling is applied here. Percent is nil only when the sample
// itself reports CPU unavailable (a disabled probe), never on a baseline:
// the worker publishes no warmup reading, so the first Sample received is
// already a real delta.
func cpuFromSample(s remotestats.Sample, details *remotestats.Details) *CPUInfo {
	cpu := &CPUInfo{}
	if !s.CPUUnavailable {
		p := s.CPUPercent
		if p < 0 {
			p = 0
		}
		if p > 100 {
			p = 100
		}
		cpu.Percent = &p
	}
	cpu.Cores = len(s.Cores)
	if details != nil {
		if cpu.Cores == 0 {
			cpu.Cores = details.CPUCount
		}
		applyLoad(cpu, details)
	}
	return cpu
}

// applyLoad fills load averages from a Details result.
func applyLoad(cpu *CPUInfo, details *remotestats.Details) {
	if details == nil || details.LoadUnavailable {
		return
	}
	cpu.Load1, cpu.Load5, cpu.Load15 = details.Load[0], details.Load[1], details.Load[2]
	cpu.LoadOK = true
}

// memoryFromSample converts a remotestats Sample into MemoryInfo per §6.5:
// MemUsed = MemTotal - MemAvailable, MemUsage = MemUsed / MemTotal * 100%.
// The sample reports used/total directly (used = total - available), so
// Available is recovered as Total - Used. Swap is filled in separately by
// applySwap; until then SwapEnabled is false and SwapPercent nil.
func memoryFromSample(s remotestats.Sample) *MemoryInfo {
	mem := &MemoryInfo{TotalBytes: s.MemoryTotalBytes, UsedBytes: s.MemoryUsedBytes}
	if mem.TotalBytes > 0 {
		if mem.UsedBytes > mem.TotalBytes {
			mem.UsedBytes = mem.TotalBytes
		}
		mem.AvailableBytes = mem.TotalBytes - mem.UsedBytes
		p := float64(mem.UsedBytes) * 100 / float64(mem.TotalBytes)
		mem.Percent = &p
	}
	return mem
}

// swapInfo is the SwapTotal/SwapFree pair from /proc/meminfo.
type swapInfo struct {
	total, free uint64
}

// readSwap reads only the swap lines of /proc/meminfo. It is a separate
// one-shot read because remotestats samples carry no swap data.
func readSwap(ctx context.Context, tr transport) (swapInfo, error) {
	data, err := tr.RunCommand(ctx, "grep -E '^(SwapTotal|SwapFree):' /proc/meminfo 2>/dev/null || true", 4096)
	if err != nil {
		return swapInfo{}, err
	}
	var swap swapInfo
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		value, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch fields[0] {
		case "SwapTotal:":
			swap.total = value << 10
		case "SwapFree:":
			swap.free = value << 10
		}
	}
	return swap, nil
}

// applySwap fills swap fields per §6.5. When SwapTotal is 0 the host has no
// swap enabled: SwapEnabled stays false and no division is performed, so
// SwapPercent stays nil instead of a fabricated 0%.
func applySwap(mem *MemoryInfo, swap swapInfo) {
	mem.SwapTotalBytes = swap.total
	if swap.total == 0 {
		mem.SwapEnabled = false
		mem.SwapUsedBytes = 0
		mem.SwapPercent = nil
		return
	}
	mem.SwapEnabled = true
	used := swap.total
	if swap.free <= swap.total {
		used = swap.total - swap.free
	}
	mem.SwapUsedBytes = used
	p := float64(used) * 100 / float64(swap.total)
	mem.SwapPercent = &p
}

// diskUsageFromDetails converts remotestats Details disks into DiskUsageInfo.
//
// parseDetails already flags virtual filesystems (tmpfs/devtmpfs/proc/sysfs
// and mounts under /dev, /sys, /proc, /run, /snap) and sorts real disks
// before virtual ones, but it does NOT dedup the same underlying storage
// reported at several mount points, and it drops the device name, so the
// conversion layer dedups here. The dedup key is (total, used, available):
// two mount points of the same filesystem report identical df numbers,
// while distinct filesystems of the same size differ in used/available.
// The first entry wins, which after parseDetails' sort is the real mount
// ("/" first, then real disks by usage) rather than a virtual alias.
func diskUsageFromDetails(d remotestats.Details) *DiskUsageInfo {
	out := &DiskUsageInfo{}
	seen := make(map[[3]uint64]bool, len(d.Disks))
	for _, disk := range d.Disks {
		if disk.Virtual || disk.Total == 0 {
			continue
		}
		key := [3]uint64{disk.Total, disk.Used, disk.Available}
		if seen[key] {
			continue
		}
		seen[key] = true
		percent := float64(disk.Used) * 100 / float64(disk.Total)
		out.Filesystems = append(out.Filesystems, Filesystem{
			// remotestats Disk carries no device/source name (its df
			// parser drops $1), so Device stays empty rather than
			// repeating the mount point as if it were a device.
			Mount:     disk.Path,
			Total:     disk.Total,
			Used:      disk.Used,
			Available: disk.Available,
			Percent:   percent,
		})
	}
	return out
}

// networkFromDetails converts Details network totals into NetworkInfo.
//
// The first result is the baseline: totals are real, but no rate can be
// computed from a single reading, so HasRate is false and the rate pointers
// stay nil (never zero). The second result (one DetailsWorker interval
// later) carries per-interface rates computed by remotestats.networkRates;
// those are copied only when the interface existed in both readings.
func networkFromDetails(first, second *remotestats.Details) *NetworkInfo {
	if first == nil {
		return nil
	}
	out := &NetworkInfo{}
	for _, net := range first.Networks {
		iface := NetInterface{
			Name:         net.Name,
			ReceiveTotal: net.Received,
			SendTotal:    net.Sent,
			HasRate:      false,
		}
		if second != nil {
			for _, after := range second.Networks {
				if after.Name != net.Name || !after.HasRate {
					continue
				}
				rx, tx := after.ReceivePerSecond, after.SendPerSecond
				iface.ReceivePerSec = &rx
				iface.SendPerSec = &tx
				iface.HasRate = true
				break
			}
		}
		out.Interfaces = append(out.Interfaces, iface)
	}
	if len(out.Interfaces) == 0 {
		return nil
	}
	return out
}
