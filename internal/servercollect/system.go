package servercollect

import (
	"context"
	"strconv"
	"strings"
	"time"
)

// systemScript reports uptime, a process count and service state. The
// process count prefers counting numeric /proc entries (no ps dependency);
// ps is the fallback. systemctl is preferred for services; SysV
// `service --status-all` is the fallback and is only counted, not parsed
// per-service.
const systemScript = `
echo "MOT_UPTIME=$(cat /proc/uptime 2>/dev/null)"
echo "MOT_PROC=$(ls -d /proc/[0-9]* 2>/dev/null | wc -l)"
echo "MOT_PS=$(ps -e --no-headers 2>/dev/null | wc -l)"
echo "MOT_HAS_SYSTEMCTL=$(command -v systemctl >/dev/null 2>&1 && echo yes || echo no)"
echo "MOT_SYS_RUNNING=$(systemctl is-system-running 2>/dev/null || echo unknown)"
echo "MOT_FAILED_BEGIN"
systemctl --failed --no-legend 2>/dev/null | head -n 32
echo "MOT_FAILED_END"
echo "MOT_HAS_SERVICE=$(command -v service >/dev/null 2>&1 && echo yes || echo no)"
`

func (c *collector) collectSystem(ctx context.Context, tr transport, now time.Time) MetricValue {
	data, err := tr.RunCommand(ctx, systemScript, 32<<10)
	if err != nil {
		return unavailable(MetricSystem, now, "system status command failed: "+c.reason(err))
	}
	info := parseSystem(data)
	if info.UptimeSecs == 0 && !info.ProcessesOK && info.ServiceManager == "" {
		return unavailable(MetricSystem, now, "no system status could be read (uptime, process count and service manager all unavailable)")
	}
	return success(MetricSystem, now, func(v *MetricValue) { v.System = &info })
}

// parseSystem parses systemScript output. systemctl --failed lines are
// `UNIT LOAD ACTIVE SUB DESCRIPTION`; the ACTIVE column is the state.
// When neither systemd nor SysV is present, ServiceManager stays empty.
func parseSystem(data []byte) SystemInfo {
	var info SystemInfo
	section := ""
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimRight(line, "\r")
		switch line {
		case "MOT_FAILED_BEGIN":
			section = "failed"
			continue
		case "MOT_FAILED_END":
			section = ""
			continue
		}
		if section == "failed" {
			fields := strings.Fields(line)
			if len(fields) >= 3 {
				info.Services = append(info.Services, ServiceStatus{Name: fields[0], State: fields[2]})
			}
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch key {
		case "MOT_UPTIME":
			if fields := strings.Fields(value); len(fields) > 0 {
				if up, err := strconv.ParseFloat(fields[0], 64); err == nil && up >= 0 {
					info.UptimeSecs = up
				}
			}
		case "MOT_PROC":
			if n, err := strconv.Atoi(value); err == nil && n > 0 {
				info.ProcessesTotal = n
				info.ProcessesOK = true
			}
		case "MOT_PS":
			if !info.ProcessesOK {
				if n, err := strconv.Atoi(value); err == nil && n > 0 {
					info.ProcessesTotal = n
					info.ProcessesOK = true
				}
			}
		case "MOT_HAS_SYSTEMCTL":
			if value == "yes" {
				info.ServiceManager = "systemd"
			}
		case "MOT_HAS_SERVICE":
			if info.ServiceManager == "" && value == "yes" {
				info.ServiceManager = "sysv"
			}
		}
	}
	return info
}
