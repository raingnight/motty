package servercollect

import (
	"context"
	"strconv"
	"strings"
	"time"
)

// nicScript prefers `ip -o` and falls back to /sys/class/net when ip is
// missing. The loopback interface is excluded by the parser.
const nicScript = `
echo "MOT_IP_BEGIN"
ip -o link 2>/dev/null
ip -o addr 2>/dev/null
echo "MOT_IP_END"
echo "MOT_SYSFS_BEGIN"
for d in /sys/class/net/*; do
  [ -e "$d" ] || continue
  n=$(basename "$d")
  echo "IFACE $n MAC=$(cat $d/address 2>/dev/null) MTU=$(cat $d/mtu 2>/dev/null) STATE=$(cat $d/operstate 2>/dev/null)"
done
echo "MOT_SYSFS_END"
`

func (c *collector) collectNIC(ctx context.Context, tr transport, now time.Time) MetricValue {
	data, err := tr.RunCommand(ctx, nicScript, 64<<10)
	if err != nil {
		return unavailable(MetricNIC, now, "NIC command failed: "+c.reason(err))
	}
	info, err := parseNIC(data)
	if err != nil {
		return unavailable(MetricNIC, now, "NIC parse failed: "+err.Error())
	}
	return success(MetricNIC, now, func(v *MetricValue) { v.NIC = &info })
}

// parseNIC parses nicScript output. `ip -o link` lines give name/MTU/state/
// MAC, `ip -o addr` lines give IPv4/IPv6 addresses; the /sys/class/net
// fallback gives name/MAC/MTU/state without addresses.
func parseNIC(data []byte) (NICInfo, error) {
	byName := make(map[string]*NICDetail)
	order := []string{}
	get := func(name string) *NICDetail {
		if d, ok := byName[name]; ok {
			return d
		}
		d := &NICDetail{Name: name}
		byName[name] = d
		order = append(order, name)
		return d
	}
	section := "ip"
	var last *NICDetail
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimRight(line, "\r")
		switch line {
		case "MOT_IP_BEGIN":
			section = "ip"
			continue
		case "MOT_IP_END":
			section = ""
			continue
		case "MOT_SYSFS_BEGIN":
			section = "sysfs"
			continue
		case "MOT_SYSFS_END":
			section = ""
			continue
		}
		if line == "" {
			continue
		}
		switch section {
		case "ip":
			last = parseIPLine(get, last, line)
		case "sysfs":
			parseSysfsLine(get, line)
		}
	}
	out := NICInfo{}
	for _, name := range order {
		if name == "lo" {
			continue
		}
		out.Interfaces = append(out.Interfaces, *byName[name])
	}
	if len(out.Interfaces) == 0 {
		return NICInfo{}, errorString("no network interfaces found")
	}
	return out, nil
}

// parseIPLine handles one `ip -o link` or `ip -o addr` line and returns the
// interface it belongs to (for continuation lines such as a wrapped
// `link/ether` row, which carry the MAC of the previous interface).
func parseIPLine(get func(string) *NICDetail, last *NICDetail, line string) *NICDetail {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return last
	}
	// Continuation line of the previous interface's link row.
	if fields[0] == "link/ether" || fields[0] == "link/loopback" {
		if last != nil && fields[0] == "link/ether" {
			last.MAC = strings.ToLower(fields[1])
		}
		return last
	}
	name := strings.TrimSuffix(fields[1], ":")
	if name == "" || name == "lo" {
		return last
	}
	d := get(name)
	if len(fields) >= 3 && (fields[2] == "inet" || fields[2] == "inet6") {
		if len(fields) >= 4 {
			addr := strings.SplitN(fields[3], "/", 2)[0]
			if fields[2] == "inet" {
				d.IPv4 = appendUnique(d.IPv4, addr)
			} else {
				d.IPv6 = appendUnique(d.IPv6, addr)
			}
		}
		return d
	}
	// Link line: `2: eth0: <...> mtu 1500 ... state UP ... link/ether aa:..`
	for i := 0; i < len(fields); i++ {
		switch fields[i] {
		case "mtu":
			if i+1 < len(fields) {
				d.MTU, _ = strconv.Atoi(fields[i+1])
			}
		case "state":
			if i+1 < len(fields) {
				d.State = strings.ToUpper(fields[i+1])
			}
		case "link/ether":
			if i+1 < len(fields) {
				d.MAC = strings.ToLower(fields[i+1])
			}
		}
	}
	return d
}

// parseSysfsLine handles one `IFACE <name> MAC=.. MTU=.. STATE=..` line.
func parseSysfsLine(get func(string) *NICDetail, line string) {
	fields := strings.Fields(line)
	if len(fields) < 2 || fields[0] != "IFACE" {
		return
	}
	name := fields[1]
	if name == "lo" {
		return
	}
	d := get(name)
	for _, field := range fields[2:] {
		key, value, ok := strings.Cut(field, "=")
		if !ok {
			continue
		}
		switch key {
		case "MAC":
			if value != "00:00:00:00:00:00" {
				d.MAC = strings.ToLower(value)
			}
		case "MTU":
			d.MTU, _ = strconv.Atoi(value)
		case "STATE":
			d.State = strings.ToUpper(value)
		}
	}
}

func appendUnique(list []string, value string) []string {
	if value == "" {
		return list
	}
	for _, item := range list {
		if item == value {
			return list
		}
	}
	return append(list, value)
}
