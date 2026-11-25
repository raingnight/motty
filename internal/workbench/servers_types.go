package workbench

// Servers page (phase 2/4): shared helpers over the real serverassets /
// serverconn / serveralerts packages.
//
// The page uses serverassets.Server, serverassets.Filter and
// serverconn.TestResult directly. The only local logic here is the
// abnormal-first ordering (the store itself sorts by name) and the
// in-memory filter the visible list applies on top of the loaded rows, so
// the list and the store cannot drift apart on what a filter means.
//
// Phase 4 orders and filters by health, which serveralerts derives from
// the last check, the latest collected values, the collection errors and
// the active alerts (see refreshServersHealth in servers_state.go). The
// health function passed to the sort/filter helpers is a lookup over that
// cached map, so Paint only reads it and never derives health itself.
//
// Credentials never appear here: passwords and key passphrases live only
// in the vault under serverassets.PasswordCredentialName /
// KeyPassphraseName ("server-credential:<asset UUID>" /
// "server-passphrase:<asset UUID>") — see servers_state.go.

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"motty/internal/serveralerts"
	"motty/internal/serverassets"
	"motty/internal/servercollect"
)

// serverStatusOf classifies a server by its last check: "ok", "failed" or
// "unknown" (never tested).
func serverStatusOf(server serverassets.Server) string {
	if server.LastCheck == nil {
		return "unknown"
	}
	if server.LastCheck.OK {
		return "ok"
	}
	return "failed"
}

// Health filter values for the servers list: "" is all servers.
const (
	serversHealthAll     = ""
	serversHealthOffline = "offline"
	serversHealthAuth    = "auth"
	serversHealthCollect = "collect"
	serversHealthAlert   = "alert"
	serversHealthOK      = "ok"
)

// Alert filter values for the servers list: "" is all servers.
const (
	serversAlertAll  = ""
	serversAlertHas  = "has"
	serversAlertNone = "none"
)

// serverHealthCategory is the one of the spec §14.2 five tiers a health
// falls in: offline (network/SSH unreachable), SSH abnormal (auth),
// collection failed, resource alert (an active alert while the level is
// ok or collect), or normal. A level the tiers do not name (unknown, or
// no health at all) is normal: it is not offline, not an auth failure,
// not a collection failure and carries no alert.
func serverHealthCategory(health serveralerts.ServerHealth) string {
	switch health.Level {
	case serveralerts.HealthNetwork, serveralerts.HealthSSH:
		return serversHealthOffline
	case serveralerts.HealthAuth:
		return serversHealthAuth
	case serveralerts.HealthCollect:
		return serversHealthCollect
	}
	if health.HasAlert {
		return serversHealthAlert
	}
	return serversHealthOK
}

// serverHealthRank is the tier's place in the abnormal-first order.
func serverHealthRank(health serveralerts.ServerHealth) int {
	switch serverHealthCategory(health) {
	case serversHealthOffline:
		return 0
	case serversHealthAuth:
		return 1
	case serversHealthCollect:
		return 2
	case serversHealthAlert:
		return 3
	default:
		return 4
	}
}

// serverFlagsText lists every abnormal identifier a row carries, in spec
// order, joined with " · ". A server with nothing wrong reports "".
// fingerprintChanged is true when the stored fingerprint differs from the
// one a test just saw (or a confirmation is pending for it); expired is
// true when any displayed resource value is past its StaleAfter.
func (state *State) serverFlagsText(server serverassets.Server, health serveralerts.ServerHealth, expired, fingerprintChanged bool) string {
	var flags []string
	switch health.Level {
	case serveralerts.HealthNetwork, serveralerts.HealthSSH:
		flags = append(flags, state.serversText("Offline", "离线"))
	case serveralerts.HealthAuth:
		flags = append(flags, state.serversText("Auth failed", "认证失败"))
	case serveralerts.HealthCollect:
		flags = append(flags, state.serversText("Collection failed", "采集异常"))
	}
	if health.HasAlert {
		flags = append(flags, state.serversText("Resource alert", "资源告警"))
	}
	if expired {
		flags = append(flags, state.serversText("Stale data", "数据过期"))
	}
	if fingerprintChanged {
		flags = append(flags, state.serversText("Key changed", "指纹变化"))
	}
	if server.Deleted {
		flags = append(flags, state.serversText("Deleted", "已删除"))
	}
	return strings.Join(flags, " · ")
}

// serversResourceCells formats one server's latest CPU/memory/disk values
// for the servers table (phase 3). A value with no measurement yet stays
// "—": a baseline-only first sample has no percent, and a server that was
// never collected successfully shows no number at all — never 0. Disk is
// the capacity-weighted overall usage across the collected filesystems
// (the collector already dedups repeated mounts, so the sum does not
// double-count one underlying store). expired is true when any displayed
// value is past its StaleAfter, so the row can flag it; values that are
// missing entirely are not "expired", they are just absent.
func serversResourceCells(latest servercollect.Latest, now time.Time) (cpu, mem, disk string, expired bool) {
	cpu, mem, disk = "—", "—", "—"
	if v, ok := latest.Get(servercollect.MetricCPU); ok && v.CPU != nil && v.CPU.Percent != nil {
		cpu = fmt.Sprintf("%.0f%%", *v.CPU.Percent)
		expired = expired || v.Expired(now)
	}
	if v, ok := latest.Get(servercollect.MetricMemory); ok && v.Memory != nil && v.Memory.Percent != nil {
		mem = fmt.Sprintf("%.0f%%", *v.Memory.Percent)
		expired = expired || v.Expired(now)
	}
	if v, ok := latest.Get(servercollect.MetricDiskUsage); ok && v.DiskUsage != nil {
		var total, used uint64
		for _, fs := range v.DiskUsage.Filesystems {
			total += fs.Total
			used += fs.Used
		}
		if total > 0 {
			disk = fmt.Sprintf("%.0f%%", float64(used)/float64(total)*100)
			expired = expired || v.Expired(now)
		}
	}
	return cpu, mem, disk, expired
}

// filterManagedServers applies a serverassets.Filter to an already-loaded
// list, with the same rules the store applies server-side, plus the phase 4
// health and alert filters. healthFilter is one of the serversHealth*
// values, alertFilter one of the serversAlert* values; a nil health
// lookup treats every server as unknown (normal tier, no alert).
func filterManagedServers(servers []serverassets.Server, filter serverassets.Filter, healthFilter, alertFilter string, health func(serverassets.Server) serveralerts.ServerHealth) []serverassets.Server {
	query := strings.ToLower(strings.TrimSpace(filter.Query))
	copied := append([]serverassets.Server(nil), servers...)
	out := copied[:0]
	for _, server := range copied {
		if !filter.IncludeDeleted && server.Deleted {
			continue
		}
		if filter.GroupID != "" && server.GroupID != filter.GroupID {
			continue
		}
		if filter.Tag != "" && !slicesContainsFold(server.Tags, filter.Tag) {
			continue
		}
		if filter.OS != "" && !strings.EqualFold(server.OS, filter.OS) {
			continue
		}
		if filter.Status != "" && serverStatusOf(server) != filter.Status {
			continue
		}
		if healthFilter != "" || alertFilter != "" {
			h := serveralerts.ServerHealth{Level: serveralerts.HealthUnknown}
			if health != nil {
				h = health(server)
			}
			if healthFilter != "" && serverHealthCategory(h) != healthFilter {
				continue
			}
			if alertFilter == serversAlertHas && !h.HasAlert {
				continue
			}
			if alertFilter == serversAlertNone && h.HasAlert {
				continue
			}
		}
		if query != "" && !strings.Contains(strings.ToLower(server.Name+" "+server.Hostname+" "+server.Address), query) {
			continue
		}
		out = append(out, server)
	}
	return out
}

func slicesContainsFold(values []string, want string) bool {
	for _, value := range values {
		if strings.EqualFold(value, want) {
			return true
		}
	}
	return false
}

// sortManagedServers orders servers abnormal-first in the spec §14.2 five
// tiers — offline, SSH abnormal, collection failed, resource alert,
// normal — each tier by name, stable. A nil health lookup treats every
// server as unknown (normal tier), so the order is name order.
func sortManagedServers(servers []serverassets.Server, health func(serverassets.Server) serveralerts.ServerHealth) {
	sort.SliceStable(servers, func(i, j int) bool {
		var hi, hj serveralerts.ServerHealth
		if health != nil {
			hi, hj = health(servers[i]), health(servers[j])
		} else {
			hi, hj = serveralerts.ServerHealth{Level: serveralerts.HealthUnknown}, serveralerts.ServerHealth{Level: serveralerts.HealthUnknown}
		}
		if ri, rj := serverHealthRank(hi), serverHealthRank(hj); ri != rj {
			return ri < rj
		}
		return strings.ToLower(servers[i].Name) < strings.ToLower(servers[j].Name)
	})
}

func serverPortOrDefault(port int) int {
	if port <= 0 {
		return 22
	}
	return port
}

// serversPageSizes are the servers list's page sizes; 0 is "all".
var serversPageSizes = [...]int{20, 50, 100, 0}
