// Alert evaluation, health derivation and home-page statistics for
// managed servers (phase 4 of the Motty secondary development).
//
// This file holds the pure logic: thresholds with hysteresis (§8.4),
// SSH/collection-failure alerts (§8.5), the three-layer health status
// (§14.1) and the home-page aggregates (§8.2/§8.3). It never touches a
// database, a network connection or UI state — persistence lives in
// store.go and the background loop in watcher.go — so every function
// here is unit-testable with plain values.
//
// Hard rules, from the spec and the phase-3 contract:
//
//   - A value that is missing, expired (past StaleAfter) or marked
//     Unavailable is not a value: it never triggers an alert, never
//     recovers one, and never feeds a statistic. Nothing is ever
//     treated as zero.
//   - A CPU baseline sample (Percent == nil) is not a value either.
//   - No credential value may appear in a message, a reason or an
//     error returned here.
package serveralerts

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"motty/internal/serverassets"
	"motty/internal/servercollect"
)

// Alert thresholds and recovery thresholds (§8.4). An alert fires at or
// above the alert threshold and only clears below the recovery
// threshold; between the two the previous state is kept.
const (
	CPUAlertThreshold      = 80.0
	CPURecoverThreshold    = 75.0
	MemoryAlertThreshold   = 85.0
	MemoryRecoverThreshold = 80.0
	DiskAlertThreshold     = 85.0
	DiskRecoverThreshold   = 80.0
)

// AlertKind identifies what an alert is about (§8.5).
type AlertKind string

const (
	KindCPU     AlertKind = "cpu"
	KindMemory  AlertKind = "memory"
	KindDisk    AlertKind = "disk"
	KindSSH     AlertKind = "ssh"
	KindCollect AlertKind = "collect"
)

// AlertState is whether an alert is currently firing or has recovered.
// Recovered alerts are kept as history.
type AlertState string

const (
	StateActive    AlertState = "active"
	StateRecovered AlertState = "recovered"
)

// Alert is one alert incident. ID is a deterministic dedup key
// (AlertID): one active incident per server, kind and mount. A
// re-trigger after a recovery reuses the same ID with a fresh
// CreatedAt; the Store preserves the first CreatedAt it saw for an
// ID (see store.go Save).
type Alert struct {
	ID            string     `json:"id"`
	ServerID      string     `json:"server_id"`
	ServerName    string     `json:"server_name,omitempty"`
	Kind          AlertKind  `json:"kind"`
	Mount         string     `json:"mount,omitempty"`
	State         AlertState `json:"state"`
	Message       string     `json:"message"`
	Value         float64    `json:"value"`
	Threshold     float64    `json:"threshold"`
	Read          bool       `json:"read"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	RecoveredAt   *time.Time `json:"recovered_at,omitempty"`
	RecoverReason string     `json:"recover_reason,omitempty"`
}

// AlertID is the deterministic dedup key for an incident: server,
// kind and mount (mount is "" except for disk alerts, where it is the
// mount point, and collect alerts, where it is the failing kind name).
func AlertID(serverID string, kind AlertKind, mount string) string {
	return serverID + "|" + string(kind) + "|" + mount
}

// HealthLevel is the three-layer online status (§14.1).
type HealthLevel string

const (
	HealthUnknown HealthLevel = "unknown" // never checked
	HealthOK      HealthLevel = "ok"      // SSH connected, collection fine
	HealthNetwork HealthLevel = "network" // network unreachable
	HealthSSH     HealthLevel = "ssh"     // SSH service unavailable
	HealthAuth    HealthLevel = "auth"    // SSH authentication failed
	HealthCollect HealthLevel = "collect" // SSH fine but collection abnormal
)

// ServerHealth is one server's derived status, used by the home page,
// the server list sorting/filters and the alert centre.
type ServerHealth struct {
	Level        HealthLevel
	Detail       string
	Stale        bool
	HasAlert     bool
	ActiveAlerts []string
}

// HealthInput is everything DeriveHealth needs for one server.
type HealthInput struct {
	Server       serverassets.Server
	Latest       servercollect.Latest
	HasLatest    bool
	LastErrors   map[servercollect.MetricKind]string
	ActiveAlerts []Alert
	Now          time.Time
}

// HighDisk is one filesystem at or above the disk alert threshold.
type HighDisk struct {
	ServerID   string
	ServerName string
	Mount      string
	Percent    float64
	Total      uint64
	Used       uint64
}

// Stats is the home-page aggregate (§8.2/§8.3). Averages are nil when
// no server has a fresh value — the caller shows "暂无有效数据",
// never zero.
type Stats struct {
	Total           int
	Online          int
	Offline         int
	Abnormal        int
	CPUAvg          *float64
	CPUCount        int
	MemAvg          *float64
	MemCount        int
	DiskPercent     *float64
	DiskTotalBytes  uint64
	DiskUsedBytes   uint64
	HighDisks       []HighDisk
	HighDiskServers int
}

// --- freshness helpers ---

// freshValue reports whether a stored value may be treated as current:
// present, collected, not marked unavailable, and not past StaleAfter.
func freshValue(v servercollect.MetricValue, now time.Time) bool {
	if v.Unavailable != "" || v.CollectedAt.IsZero() {
		return false
	}
	return !v.Expired(now)
}

// freshPercent extracts a usable percent from a fresh value, or reports
// none. A nil Percent (for example a CPU baseline sample) is no value.
func freshPercent(v servercollect.MetricValue, now time.Time) (float64, bool) {
	if !freshValue(v, now) {
		return 0, false
	}
	switch v.Kind {
	case servercollect.MetricCPU:
		if v.CPU == nil || v.CPU.Percent == nil {
			return 0, false
		}
		return *v.CPU.Percent, true
	case servercollect.MetricMemory:
		if v.Memory == nil || v.Memory.Percent == nil {
			return 0, false
		}
		return *v.Memory.Percent, true
	}
	return 0, false
}

// freshKind reports whether latest holds a fresh (non-expired, collected)
// value for kind. The payload itself is not inspected: a baseline
// sample still proves the collector ran.
func freshKind(latest servercollect.Latest, kind servercollect.MetricKind, now time.Time) bool {
	v, ok := latest.Get(kind)
	return ok && freshValue(v, now)
}

// diskPercent is one filesystem's usage percent, computed from its
// capacities when available and falling back to the recorded percent.
func diskPercent(fs servercollect.Filesystem) float64 {
	if fs.Total > 0 {
		return float64(fs.Used) / float64(fs.Total) * 100
	}
	return fs.Percent
}

// --- alert construction ---

func newAlert(server serverassets.Server, kind AlertKind, mount, message string, value, threshold float64, now time.Time) Alert {
	return Alert{
		ID:         AlertID(server.ID, kind, mount),
		ServerID:   server.ID,
		ServerName: server.Name,
		Kind:       kind,
		Mount:      mount,
		State:      StateActive,
		Message:    message,
		Value:      value,
		Threshold:  threshold,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
}

// refreshAlert updates a still-firing alert with the newest value and
// message, keeping its ID and CreatedAt.
func refreshAlert(prev Alert, message string, value float64, now time.Time) Alert {
	prev.Message = message
	prev.Value = value
	prev.UpdatedAt = now
	return prev
}

// recoverAlert closes an active alert, recording when and why.
func recoverAlert(prev Alert, reason string, now time.Time) Alert {
	prev.State = StateRecovered
	prev.RecoveredAt = &now
	prev.RecoverReason = reason
	prev.UpdatedAt = now
	return prev
}

func cpuMessage(serverName string, value float64) string {
	return fmt.Sprintf("%s CPU 使用率 %.0f%%，达到告警阈值 %.0f%%", serverName, value, CPUAlertThreshold)
}

func cpuUpdateMessage(serverName string, value float64) string {
	return fmt.Sprintf("%s CPU 使用率 %.0f%%（告警阈值 %.0f%%，恢复阈值 %.0f%%）", serverName, value, CPUAlertThreshold, CPURecoverThreshold)
}

func memMessage(serverName string, value float64) string {
	return fmt.Sprintf("%s 内存使用率 %.0f%%，达到告警阈值 %.0f%%", serverName, value, MemoryAlertThreshold)
}

func memUpdateMessage(serverName string, value float64) string {
	return fmt.Sprintf("%s 内存使用率 %.0f%%（告警阈值 %.0f%%，恢复阈值 %.0f%%）", serverName, value, MemoryAlertThreshold, MemoryRecoverThreshold)
}

func diskMessage(serverName, mount string, value float64) string {
	return fmt.Sprintf("%s 磁盘 %s 使用率 %.0f%%，达到告警阈值 %.0f%%", serverName, mount, value, DiskAlertThreshold)
}

func diskUpdateMessage(serverName, mount string, value float64) string {
	return fmt.Sprintf("%s 磁盘 %s 使用率 %.0f%%（告警阈值 %.0f%%，恢复阈值 %.0f%%）", serverName, mount, value, DiskAlertThreshold, DiskRecoverThreshold)
}

// kindLabel is the Chinese label for a metric kind, for messages.
func kindLabel(kind servercollect.MetricKind) string {
	switch kind {
	case servercollect.MetricBasic:
		return "基础信息"
	case servercollect.MetricCPU:
		return "CPU"
	case servercollect.MetricMemory:
		return "内存"
	case servercollect.MetricDiskUsage:
		return "磁盘容量"
	case servercollect.MetricDiskIO:
		return "磁盘 IO"
	case servercollect.MetricNetwork:
		return "网络流量"
	case servercollect.MetricNIC:
		return "网卡信息"
	case servercollect.MetricSystem:
		return "系统状态"
	case servercollect.MetricDocker:
		return "Docker"
	case servercollect.MetricConnectivity:
		return "连通性"
	}
	return string(kind)
}

// handleThreshold applies the hysteresis rule for one resource value:
// below the alert threshold with no active alert does nothing; at or
// above it with no active alert creates one; with an active alert,
// below the recovery threshold recovers it and anything at or above
// the recovery threshold refreshes it. A missing value is never a
// decision, so callers only invoke this with a fresh percent.
func handleThreshold(prev map[string]Alert, server serverassets.Server, kind AlertKind, mount string, value, alertAt, recoverBelow float64, makeMsg, makeUpdateMsg func() string, makeRecoverReason func() string, now time.Time) (created, updated, recovered []Alert) {
	id := AlertID(server.ID, kind, mount)
	prevAlert, active := prev[id]
	if !active {
		if value >= alertAt {
			created = append(created, newAlert(server, kind, mount, makeMsg(), value, alertAt, now))
		}
		return created, updated, recovered
	}
	if value < recoverBelow {
		recovered = append(recovered, recoverAlert(prevAlert, makeRecoverReason(), now))
		return created, updated, recovered
	}
	updated = append(updated, refreshAlert(prevAlert, makeUpdateMsg(), value, now))
	return created, updated, recovered
}

// --- connectivity classification ---

// classifyConnFailure maps an engine last-error reason for the
// connectivity kind to a health level and a short detail. The reason
// strings come from the scheduler: dial failures embed the
// serverconn.FailureKind ("network_unreachable", "ssh_unavailable",
// "auth_failed", "fingerprint_changed", "fingerprint_unknown",
// "jump_host_failed", "timeout"), per-kind failures carry a
// "(stage tcp|handshake|auth)" suffix. Fingerprint and jump-host
// failures are checked first: they name the cause even when a stage
// marker is also present. A timeout names no stage, so it is reported
// as network unreachable with the uncertainty kept in the detail.
func classifyConnFailure(reason string) (HealthLevel, string) {
	switch {
	case strings.Contains(reason, "fingerprint_changed"):
		return HealthSSH, "主机指纹已变化"
	case strings.Contains(reason, "fingerprint_unknown"):
		return HealthSSH, "主机指纹未确认"
	case strings.Contains(reason, "jump_host_failed"):
		return HealthSSH, "跳板机连接失败"
	case strings.Contains(reason, "network_unreachable"):
		return HealthNetwork, "网络不可达"
	case strings.Contains(reason, "(stage tcp)"):
		return HealthNetwork, "网络不可达"
	case strings.Contains(reason, "ssh_unavailable"):
		return HealthSSH, "SSH 服务不可用"
	case strings.Contains(reason, "(stage handshake)"):
		return HealthSSH, "SSH 服务不可用"
	case strings.Contains(reason, "auth_failed"):
		return HealthAuth, "SSH 认证失败"
	case strings.Contains(reason, "(stage auth)"):
		return HealthAuth, "SSH 认证失败"
	case strings.Contains(reason, "timeout"):
		return HealthNetwork, "连接超时（TCP 或握手阶段）"
	default:
		return HealthSSH, "SSH 连接失败"
	}
}

// --- Evaluate ---

// Evaluate decides what happens to one server's alerts given its latest
// values, the scheduler's per-kind last errors and its currently active
// alerts. It is pure: it reads nothing else and writes nothing. The
// three result slices are disjoint: created holds newly firing
// incidents, updated holds still-firing ones with fresh values,
// recovered holds incidents that just cleared. An alert whose input is
// missing or expired appears in none of the three — no decision is
// taken for it either way.
func Evaluate(server serverassets.Server, latest servercollect.Latest, hasLatest bool, failures map[servercollect.MetricKind]string, prev []Alert, now time.Time) (created, updated, recovered []Alert) {
	_ = hasLatest // freshness is decided per value, not by this flag.
	active := map[string]Alert{}
	for _, alert := range prev {
		if alert.State == StateActive {
			active[alert.ID] = alert
		}
	}
	append3 := func(c, u, r []Alert) {
		created = append(created, c...)
		updated = append(updated, u...)
		recovered = append(recovered, r...)
	}

	// Resource thresholds (§8.4), each on its own fresh value.
	if v, ok := latest.Get(servercollect.MetricCPU); ok {
		if pct, fresh := freshPercent(v, now); fresh {
			c, u, r := handleThreshold(active, server, KindCPU, "", pct, CPUAlertThreshold, CPURecoverThreshold,
				func() string { return cpuMessage(server.Name, pct) },
				func() string { return cpuUpdateMessage(server.Name, pct) },
				func() string {
					return fmt.Sprintf("CPU 使用率降至 %.0f%%，低于恢复阈值 %.0f%%", pct, CPURecoverThreshold)
				}, now)
			append3(c, u, r)
		}
	}
	if v, ok := latest.Get(servercollect.MetricMemory); ok {
		if pct, fresh := freshPercent(v, now); fresh {
			c, u, r := handleThreshold(active, server, KindMemory, "", pct, MemoryAlertThreshold, MemoryRecoverThreshold,
				func() string { return memMessage(server.Name, pct) },
				func() string { return memUpdateMessage(server.Name, pct) },
				func() string {
					return fmt.Sprintf("内存使用率降至 %.0f%%，低于恢复阈值 %.0f%%", pct, MemoryRecoverThreshold)
				}, now)
			append3(c, u, r)
		}
	}
	if v, ok := latest.Get(servercollect.MetricDiskUsage); ok && freshValue(v, now) && v.DiskUsage != nil {
		for _, fs := range v.DiskUsage.Filesystems {
			pct := diskPercent(fs)
			c, u, r := handleThreshold(active, server, KindDisk, fs.Mount, pct, DiskAlertThreshold, DiskRecoverThreshold,
				func() string { return diskMessage(server.Name, fs.Mount, pct) },
				func() string { return diskUpdateMessage(server.Name, fs.Mount, pct) },
				func() string {
					return fmt.Sprintf("磁盘 %s 使用率降至 %.0f%%，低于恢复阈值 %.0f%%", fs.Mount, pct, DiskRecoverThreshold)
				}, now)
			append3(c, u, r)
		}
	}

	// Connectivity (§8.5): a fresh successful value recovers the SSH
	// alert; a recorded failure without one (re)fires it. The dedup
	// key carries no stage, so a stage change refreshes the same
	// incident instead of opening a second one.
	sshID := AlertID(server.ID, KindSSH, "")
	freshConnOK := false
	if v, ok := latest.Get(servercollect.MetricConnectivity); ok && freshValue(v, now) && v.Connectivity != nil && v.Connectivity.OK {
		freshConnOK = true
	}
	connFailure := failures[servercollect.MetricConnectivity]
	if freshConnOK {
		if prevAlert, ok := active[sshID]; ok {
			recovered = append(recovered, recoverAlert(prevAlert, "SSH 连接已恢复", now))
		}
	} else if connFailure != "" {
		_, detail := classifyConnFailure(connFailure)
		if prevAlert, ok := active[sshID]; ok {
			updated = append(updated, refreshAlert(prevAlert, fmt.Sprintf("%s SSH 连接异常：%s", server.Name, detail), 0, now))
		} else {
			created = append(created, newAlert(server, KindSSH, "", fmt.Sprintf("%s SSH 连接异常：%s", server.Name, detail), 0, 0, now))
		}
	}

	// Collection failures (§8.5): only while SSH itself is healthy —
	// when SSH is down a collection failure is a consequence of that,
	// not a separate incident. One incident per failing kind, keyed by
	// the kind name; it recovers when the kind has a fresh value again.
	if freshConnOK {
		for _, kind := range servercollect.AllMetricKinds {
			if kind == servercollect.MetricConnectivity {
				continue
			}
			id := AlertID(server.ID, KindCollect, string(kind))
			prevAlert, isActive := active[id]
			if freshKind(latest, kind, now) {
				if isActive {
					recovered = append(recovered, recoverAlert(prevAlert, fmt.Sprintf("%s 指标采集已恢复", kindLabel(kind)), now))
				}
				continue
			}
			if failures[kind] == "" {
				continue
			}
			if isActive {
				updated = append(updated, refreshAlert(prevAlert, fmt.Sprintf("%s 采集失败：%s 指标无数据", server.Name, kindLabel(kind)), 0, now))
			} else {
				created = append(created, newAlert(server, KindCollect, string(kind), fmt.Sprintf("%s 采集失败：%s 指标无数据", server.Name, kindLabel(kind)), 0, 0, now))
			}
		}
	}
	return created, updated, recovered
}

// --- DeriveHealth ---

// DeriveHealth computes one server's three-layer status (§14.1) from
// its latest values, the scheduler's per-kind last errors and the
// manual test record (Server.LastCheck). A fresh connectivity success
// means SSH works; without one, the connectivity last error is
// classified, then the manual test, then unknown. A collection
// failure only counts while SSH works and the kind has no fresh value:
// a stale last-error entry next to a fresh value means the failure
// was already superseded. Stale is true when the server has no stored
// values at all or any stored value is expired; a kind that was never
// collected while other values are fresh does not by itself make the
// server stale.
func DeriveHealth(in HealthInput) ServerHealth {
	now := in.Now
	health := ServerHealth{}
	for _, alert := range in.ActiveAlerts {
		if alert.State == StateActive {
			health.HasAlert = true
			health.ActiveAlerts = append(health.ActiveAlerts, alert.ID)
		}
	}
	stale := true
	if len(in.Latest.Values) > 0 {
		stale = false
		for _, v := range in.Latest.Values {
			if v.Expired(now) {
				stale = true
				break
			}
		}
	}
	health.Stale = stale

	// Fresh connectivity success: SSH works. Any non-connectivity kind
	// with a recorded failure and no fresh value is a collection
	// problem; otherwise the server is healthy.
	if v, ok := in.Latest.Get(servercollect.MetricConnectivity); ok && freshValue(v, now) && v.Connectivity != nil {
		if v.Connectivity.OK {
			for kind, reason := range in.LastErrors {
				if kind == servercollect.MetricConnectivity || reason == "" {
					continue
				}
				if freshKind(in.Latest, kind, now) {
					continue
				}
				health.Level = HealthCollect
				health.Detail = fmt.Sprintf("采集异常：%s 指标无数据", kindLabel(kind))
				return health
			}
			health.Level = HealthOK
			health.Detail = "在线"
			return health
		}
		// A fresh value that says not-OK (not produced by the current
		// collector, which stores failures as Unavailable) is still a
		// connectivity failure; classify it by its stage.
		stage := v.Connectivity.Stage
		switch stage {
		case "tcp":
			health.Level, health.Detail = HealthNetwork, "网络不可达"
		case "auth":
			health.Level, health.Detail = HealthAuth, "SSH 认证失败"
		default:
			health.Level, health.Detail = HealthSSH, "SSH 服务不可用"
		}
		return health
	}

	// No fresh connectivity: classify the recorded failure.
	if reason, ok := in.LastErrors[servercollect.MetricConnectivity]; ok && reason != "" {
		health.Level, health.Detail = classifyConnFailure(reason)
		return health
	}

	// Fall back to the manual connection test.
	if lc := in.Server.LastCheck; lc != nil {
		if lc.OK {
			health.Level, health.Detail = HealthOK, "上次手动测试：连接正常"
			return health
		}
		switch lc.Stage {
		case "tcp":
			health.Level, health.Detail = HealthNetwork, "上次手动测试：网络不可达"
		case "handshake":
			health.Level, health.Detail = HealthSSH, "上次手动测试：SSH 服务不可用"
		case "auth":
			health.Level, health.Detail = HealthAuth, "上次手动测试：SSH 认证失败"
		default:
			health.Level, health.Detail = HealthSSH, "上次手动测试：连接失败"
		}
		return health
	}

	health.Level = HealthUnknown
	health.Detail = "尚未检测"
	return health
}

// --- ComputeStats ---

// ComputeStats aggregates the home-page numbers (§8.2/§8.3) over the
// given servers. Only fresh values count; a missing or expired value
// is skipped, never zero. CPU and memory are arithmetic means over the
// servers with a fresh value; disk is capacity-weighted over fresh
// filesystems. Online counts HealthOK and HealthCollect (SSH works);
// Offline counts HealthNetwork and HealthSSH; Abnormal counts the
// concrete problem levels (network, SSH, auth, collect) and any server
// with an active alert — a never-checked server is unknown, not
// abnormal. Deleted servers are skipped defensively.
func ComputeStats(servers []serverassets.Server, latest map[string]servercollect.Latest, health map[string]ServerHealth, now time.Time) Stats {
	var st Stats
	var cpuSum, memSum float64
	var diskTotal, diskUsed uint64
	highServers := map[string]bool{}
	for _, srv := range servers {
		if srv.Deleted {
			continue
		}
		st.Total++
		h := health[srv.ID]
		switch h.Level {
		case HealthOK, HealthCollect:
			st.Online++
		case HealthNetwork, HealthSSH:
			st.Offline++
		}
		switch h.Level {
		case HealthNetwork, HealthSSH, HealthAuth, HealthCollect:
			st.Abnormal++
		default:
			if h.HasAlert {
				st.Abnormal++
			}
		}
		lat, ok := latest[srv.ID]
		if !ok {
			continue
		}
		if v, ok := lat.Get(servercollect.MetricCPU); ok {
			if pct, fresh := freshPercent(v, now); fresh {
				cpuSum += pct
				st.CPUCount++
			}
		}
		if v, ok := lat.Get(servercollect.MetricMemory); ok {
			if pct, fresh := freshPercent(v, now); fresh {
				memSum += pct
				st.MemCount++
			}
		}
		if v, ok := lat.Get(servercollect.MetricDiskUsage); ok && freshValue(v, now) && v.DiskUsage != nil {
			for _, fs := range v.DiskUsage.Filesystems {
				diskTotal += fs.Total
				diskUsed += fs.Used
				if pct := diskPercent(fs); pct >= DiskAlertThreshold {
					st.HighDisks = append(st.HighDisks, HighDisk{
						ServerID: srv.ID, ServerName: srv.Name, Mount: fs.Mount,
						Percent: pct, Total: fs.Total, Used: fs.Used,
					})
					highServers[srv.ID] = true
				}
			}
		}
	}
	if st.CPUCount > 0 {
		avg := cpuSum / float64(st.CPUCount)
		st.CPUAvg = &avg
	}
	if st.MemCount > 0 {
		avg := memSum / float64(st.MemCount)
		st.MemAvg = &avg
	}
	if diskTotal > 0 {
		st.DiskTotalBytes = diskTotal
		st.DiskUsedBytes = diskUsed
		pct := float64(diskUsed) / float64(diskTotal) * 100
		st.DiskPercent = &pct
	}
	st.HighDiskServers = len(highServers)
	sort.Slice(st.HighDisks, func(i, j int) bool {
		if st.HighDisks[i].ServerName != st.HighDisks[j].ServerName {
			return st.HighDisks[i].ServerName < st.HighDisks[j].ServerName
		}
		return st.HighDisks[i].Mount < st.HighDisks[j].Mount
	})
	return st
}
