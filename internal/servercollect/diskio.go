package servercollect

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// diskIOScript reads /proc/diskstats twice, one second apart, inside a
// single command, so the rates in this round need no cross-round baseline.
// Timestamps bracket the reads so the real elapsed interval is used.
const diskIOScript = `
echo "MOT_T1=$(date +%s%N 2>/dev/null)"
echo "MOT_DS1_BEGIN"
cat /proc/diskstats 2>/dev/null
echo "MOT_DS1_END"
sleep 1
echo "MOT_T2=$(date +%s%N 2>/dev/null)"
echo "MOT_DS2_BEGIN"
cat /proc/diskstats 2>/dev/null
echo "MOT_DS2_END"
`

// diskSectorBytes is the Linux sector size for /proc/diskstats counters.
const diskSectorBytes = 512

func (c *collector) collectDiskIO(ctx context.Context, tr transport, now time.Time) MetricValue {
	data, err := tr.RunCommand(ctx, diskIOScript, 64<<10)
	if err != nil {
		return unavailable(MetricDiskIO, now, "disk IO command failed: "+c.reason(err))
	}
	info, err := parseDiskIO(data)
	if err != nil {
		return unavailable(MetricDiskIO, now, "disk IO parse failed: "+err.Error())
	}
	return success(MetricDiskIO, now, func(v *MetricValue) { v.DiskIO = &info })
}

// diskCounters is one /proc/diskstats reading: sectors read/written per device.
type diskCounters map[string][2]uint64

// parseDiskIO computes read/write rates from the two readings in one round.
// Partitions are folded into their parent physical disk (sda1 -> sda,
// nvme0n1p1 -> nvme0n1) and never listed separately, so the same bytes are
// not counted twice. Loop and ram devices are excluded. Device-mapper
// devices (dm-*) are logical: they are reported as their own entry with
// Physical=false rather than folded into a physical disk they do not name.
func parseDiskIO(data []byte) (DiskIOInfo, error) {
	var first, second diskCounters
	var t1, t2 int64
	section := ""
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimRight(line, "\r")
		switch line {
		case "MOT_DS1_BEGIN":
			section = "ds1"
			first = diskCounters{}
			continue
		case "MOT_DS1_END", "MOT_DS2_END":
			section = ""
			continue
		case "MOT_DS2_BEGIN":
			section = "ds2"
			second = diskCounters{}
			continue
		}
		if strings.HasPrefix(line, "MOT_T1=") {
			t1, _ = strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(line, "MOT_T1=")), 10, 64)
			continue
		}
		if strings.HasPrefix(line, "MOT_T2=") {
			t2, _ = strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(line, "MOT_T2=")), 10, 64)
			continue
		}
		if section == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 14 {
			continue
		}
		readSectors, err1 := strconv.ParseUint(fields[5], 10, 64)
		writeSectors, err2 := strconv.ParseUint(fields[9], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		name := fields[2]
		if section == "ds1" {
			first[name] = [2]uint64{readSectors, writeSectors}
		} else {
			second[name] = [2]uint64{readSectors, writeSectors}
		}
	}
	if len(first) == 0 || len(second) == 0 {
		return DiskIOInfo{}, errNoDiskstats
	}
	interval := float64(t2-t1) / 1e9
	if interval <= 0 {
		interval = 1 // timestamps unavailable: the script sleeps 1s between reads
	}
	// Aggregate sector deltas per reported device.
	type agg struct {
		read, write uint64
		physical    bool
	}
	byDevice := make(map[string]*agg)
	for name, before := range first {
		after, ok := second[name]
		if !ok {
			continue // device appeared/vanished mid-round: no delta
		}
		parent, physical, skip := diskParent(name)
		if skip {
			continue
		}
		entry, ok := byDevice[parent]
		if !ok {
			entry = &agg{physical: physical}
			byDevice[parent] = entry
		}
		if after[0] >= before[0] {
			entry.read += after[0] - before[0]
		}
		if after[1] >= before[1] {
			entry.write += after[1] - before[1]
		}
	}
	out := DiskIOInfo{}
	for name, entry := range byDevice {
		readRate := float64(entry.read*diskSectorBytes) / interval
		writeRate := float64(entry.write*diskSectorBytes) / interval
		out.Devices = append(out.Devices, DiskDevice{
			Name:             name,
			Physical:         entry.physical,
			ReadBytesPerSec:  &readRate,
			WriteBytesPerSec: &writeRate,
		})
	}
	// Deterministic order for display and tests.
	for i := 0; i < len(out.Devices); i++ {
		for j := i + 1; j < len(out.Devices); j++ {
			if out.Devices[j].Name < out.Devices[i].Name {
				out.Devices[i], out.Devices[j] = out.Devices[j], out.Devices[i]
			}
		}
	}
	return out, nil
}

var (
	// nvme0n1p1 / mmcblk0p1 -> nvme0n1 / mmcblk0
	partitionSuffixed = regexp.MustCompile(`^((?:nvme\d+n\d+|mmcblk\d+))p\d+$`)
	// sda1 / vda15 / xvda2 -> sda / vda / xvda
	partitionNumbered = regexp.MustCompile(`^((?:sd|hd|vd|xvd)[a-z]+)\d+$`)
	dmDevice          = regexp.MustCompile(`^dm-\d+$`)
	loopOrRamDevice   = regexp.MustCompile(`^(?:loop|ram)\d+$`)
)

// diskParent maps a /proc/diskstats device name to the device it is reported
// under. It returns the parent name, whether that parent is physical, and
// whether the device is excluded entirely (loop/ram).
func diskParent(name string) (parent string, physical bool, skip bool) {
	if loopOrRamDevice.MatchString(name) {
		return "", false, true
	}
	if dmDevice.MatchString(name) {
		return name, false, false
	}
	if m := partitionSuffixed.FindStringSubmatch(name); m != nil {
		return m[1], true, false
	}
	if m := partitionNumbered.FindStringSubmatch(name); m != nil {
		return m[1], true, false
	}
	return name, true, false
}

var errNoDiskstats = errorString("no /proc/diskstats readings in output")

type errorString string

func (e errorString) Error() string { return string(e) }
