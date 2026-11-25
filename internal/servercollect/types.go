// Package servercollect implements agentless Linux resource collection for
// managed servers (phase 3 of the Motty secondary development).
//
// Design notes (frozen contract for the phase-3 work split):
//
//   - Collection reuses existing primitives wherever they exist:
//     remotestats streaming workers for fast CPU/memory/network samples,
//     remotestats DetailsWorker for load/disk-capacity/network totals, and
//     sshconn.Conn (which already implements remotestats.Runner and
//     remotestats.DetailRunner) as the transport. New one-shot commands and
//     parsers are only added for data remotestats does not provide
//     (OS identification, disk IO, NIC details, process/service status,
//     Docker).
//   - A Collector is stateless: the caller (scheduler) owns the SSH
//     connection and passes it in. One Collect call is one round for one
//     server; the round context carries the 60s total budget, and each
//     individual item must enforce its own 15s timeout internally.
//   - A metric that cannot be collected is reported as unavailable with a
//     reason (MetricValue.Unavailable), never as a zero value, and never
//     fails the whole round. A failed round keeps the previous successful
//     values; staleness is decided by the reader from CollectedAt and
//     StaleAfter.
//   - No credentials, passwords, private keys or application passwords may
//     appear in logs, errors or returned values.
package servercollect

import (
	"context"
	"time"

	"motty/internal/serverassets"
	"motty/internal/sshconn"
)

// MetricKind identifies one independently scheduled collection item.
type MetricKind string

const (
	MetricBasic        MetricKind = "basic"        // OS/kernel/arch/hostname/CPU model/memory total (24h)
	MetricCPU          MetricKind = "cpu"          // CPU percent (delta) + load averages (1m)
	MetricMemory       MetricKind = "memory"       // memory + swap from /proc/meminfo (1m)
	MetricDiskUsage    MetricKind = "disk-usage"   // persistent filesystem capacity (10m)
	MetricDiskIO       MetricKind = "disk-io"      // disk read/write rates from /proc/diskstats (1m)
	MetricNetwork      MetricKind = "network"      // interface byte rates from /proc/net/dev (1m)
	MetricNIC          MetricKind = "nic"          // NIC names, IPs, MACs (30m)
	MetricSystem       MetricKind = "system"       // uptime, process count, service status (5m)
	MetricDocker       MetricKind = "docker"       // Docker install/run state + containers (5m)
	MetricConnectivity MetricKind = "connectivity" // SSH reachability check (5m)
)

// AllMetricKinds lists every kind in a stable order.
var AllMetricKinds = []MetricKind{
	MetricBasic, MetricCPU, MetricMemory, MetricDiskUsage, MetricDiskIO,
	MetricNetwork, MetricNIC, MetricSystem, MetricDocker, MetricConnectivity,
}

// DefaultIntervals is the per-kind collection period from spec §7.1.
var DefaultIntervals = map[MetricKind]time.Duration{
	MetricBasic:        24 * time.Hour,
	MetricCPU:          time.Minute,
	MetricMemory:       time.Minute,
	MetricDiskUsage:    10 * time.Minute,
	MetricDiskIO:       time.Minute,
	MetricNetwork:      time.Minute,
	MetricNIC:          30 * time.Minute,
	MetricSystem:       5 * time.Minute,
	MetricDocker:       5 * time.Minute,
	MetricConnectivity: 5 * time.Minute,
}

// StaleFactor: a value counts as expired when its age exceeds this multiple
// of its collection interval. Expired values are kept for display (with
// their last success time) but must never feed statistics.
const StaleFactor = 3

// BasicInfo is the 24h system identification result (§6.3).
type BasicInfo struct {
	OSName     string    `json:"os_name,omitempty"`
	OSVersion  string    `json:"os_version,omitempty"`
	OSID       string    `json:"os_id,omitempty"` // /etc/os-release ID, e.g. "ubuntu", "openEuler"
	Kernel     string    `json:"kernel,omitempty"`
	Arch       string    `json:"arch,omitempty"` // x86_64 or aarch64
	Hostname   string    `json:"hostname,omitempty"`
	BootTime   time.Time `json:"boot_time,omitempty"`
	UptimeSecs float64   `json:"uptime_secs"`
	CPUModel   string    `json:"cpu_model,omitempty"`
	CPUCores   int       `json:"cpu_cores"`
	CPUThreads int       `json:"cpu_threads"`
	TotalMem   uint64    `json:"total_mem_bytes"`
}

// CPUInfo is the 1m CPU result (§6.4). Percent is nil on the baseline-only
// first sample (no rate is invented from a single reading).
type CPUInfo struct {
	Percent *float64 `json:"percent,omitempty"`
	Load1   float64  `json:"load1"`
	Load5   float64  `json:"load5"`
	Load15  float64  `json:"load15"`
	LoadOK  bool     `json:"load_ok"`
	Cores   int      `json:"cores"`
}

// MemoryInfo is the 1m memory/swap result (§6.5).
type MemoryInfo struct {
	TotalBytes     uint64   `json:"total_bytes"`
	UsedBytes      uint64   `json:"used_bytes"`
	AvailableBytes uint64   `json:"available_bytes"`
	Percent        *float64 `json:"percent,omitempty"`
	SwapTotalBytes uint64   `json:"swap_total_bytes"`
	SwapUsedBytes  uint64   `json:"swap_used_bytes"`
	SwapPercent    *float64 `json:"swap_percent,omitempty"`
	SwapEnabled    bool     `json:"swap_enabled"`
}

// Filesystem is one persistent mount (§6.6). Virtual filesystems
// (tmpfs/devtmpfs/proc/sysfs/overlay/...) are excluded by the collector.
type Filesystem struct {
	Device    string  `json:"device"`
	Mount     string  `json:"mount"`
	FSType    string  `json:"fs_type,omitempty"`
	Total     uint64  `json:"total_bytes"`
	Used      uint64  `json:"used_bytes"`
	Available uint64  `json:"available_bytes"`
	Percent   float64 `json:"percent"`
}

// DiskUsageInfo is the 10m disk capacity result (§6.6).
type DiskUsageInfo struct {
	Filesystems []Filesystem `json:"filesystems"`
}

// DiskDevice is one block device's IO rates (§6.7). Rates are nil on the
// baseline-only first sample.
type DiskDevice struct {
	Name             string   `json:"name"`
	Physical         bool     `json:"physical"`
	ReadBytesPerSec  *float64 `json:"read_bytes_per_sec,omitempty"`
	WriteBytesPerSec *float64 `json:"write_bytes_per_sec,omitempty"`
}

// DiskIOInfo is the 1m disk IO result (§6.7).
type DiskIOInfo struct {
	Devices []DiskDevice `json:"devices"`
}

// NetInterface is one interface's traffic (§6.8). Rates are nil on the
// baseline-only first sample.
type NetInterface struct {
	Name          string  `json:"name"`
	ReceivePerSec *uint64 `json:"receive_per_sec,omitempty"`
	SendPerSec    *uint64 `json:"send_per_sec,omitempty"`
	ReceiveTotal  uint64  `json:"receive_total_bytes"`
	SendTotal     uint64  `json:"send_total_bytes"`
	HasRate       bool    `json:"has_rate"`
}

// NetworkInfo is the 1m network traffic result (§6.8).
type NetworkInfo struct {
	Interfaces []NetInterface `json:"interfaces"`
}

// NICDetail is one interface's addressing (§6.8, 30m).
type NICDetail struct {
	Name  string   `json:"name"`
	MAC   string   `json:"mac,omitempty"`
	IPv4  []string `json:"ipv4,omitempty"`
	IPv6  []string `json:"ipv6,omitempty"`
	MTU   int      `json:"mtu"`
	State string   `json:"state,omitempty"`
}

// NICInfo is the 30m NIC inventory result.
type NICInfo struct {
	Interfaces []NICDetail `json:"interfaces"`
}

// ServiceStatus is one service's state (§6.9).
type ServiceStatus struct {
	Name  string `json:"name"`
	State string `json:"state"` // e.g. "active", "inactive", "failed"
}

// SystemInfo is the 5m system status result (§6.9).
type SystemInfo struct {
	UptimeSecs     float64         `json:"uptime_secs"`
	ProcessesTotal int             `json:"processes_total"`
	ProcessesOK    bool            `json:"processes_ok"`
	ServiceManager string          `json:"service_manager,omitempty"` // "systemd", "sysv", "" = unknown
	Services       []ServiceStatus `json:"services,omitempty"`
}

// DockerContainer is one container (§6.10).
type DockerContainer struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Image     string `json:"image"`
	State     string `json:"state"` // e.g. "running", "exited"
	Status    string `json:"status,omitempty"`
	Created   string `json:"created,omitempty"`
	StartedAt string `json:"started_at,omitempty"`
	Ports     string `json:"ports,omitempty"`
}

// DockerInfo is the 5m Docker result (§6.10).
type DockerInfo struct {
	Installed  bool              `json:"installed"`
	Running    bool              `json:"running"`
	Containers []DockerContainer `json:"containers,omitempty"`
}

// ConnectivityInfo is the 5m SSH reachability result (§14.1).
type ConnectivityInfo struct {
	OK    bool   `json:"ok"`
	Stage string `json:"stage,omitempty"` // tcp / handshake / auth
	Error string `json:"error,omitempty"`
}

// MetricValue is one metric kind's latest result. Exactly one of the payload
// pointers is set on success; on failure to collect that item, Unavailable
// carries the reason and all payload pointers are nil. A nil payload with an
// empty Unavailable must never be produced.
type MetricValue struct {
	Kind         MetricKind        `json:"kind"`
	CollectedAt  time.Time         `json:"collected_at"`
	StaleAfter   time.Duration     `json:"stale_after_ns"`
	Unavailable  string            `json:"unavailable,omitempty"`
	Basic        *BasicInfo        `json:"basic,omitempty"`
	CPU          *CPUInfo          `json:"cpu,omitempty"`
	Memory       *MemoryInfo       `json:"memory,omitempty"`
	DiskUsage    *DiskUsageInfo    `json:"disk_usage,omitempty"`
	DiskIO       *DiskIOInfo       `json:"disk_io,omitempty"`
	Network      *NetworkInfo      `json:"network,omitempty"`
	NIC          *NICInfo          `json:"nic,omitempty"`
	System       *SystemInfo       `json:"system,omitempty"`
	Docker       *DockerInfo       `json:"docker,omitempty"`
	Connectivity *ConnectivityInfo `json:"connectivity,omitempty"`
}

// Expired reports whether the value is too old to be treated as current at
// now. Expired values are kept (with CollectedAt) for display, but must not
// feed statistics or alert decisions.
func (v MetricValue) Expired(now time.Time) bool {
	if v.CollectedAt.IsZero() {
		return true
	}
	return now.After(v.CollectedAt.Add(v.StaleAfter))
}

// RoundResult is one Collect call's outcome for one server. Values holds one
// entry per requested kind (success, unavailable, or carried over as stale
// only by the store layer — the collector never fabricates a value).
// Interrupted is set when the round was cut short (timeout or sleep/resume);
// Err is a round-level error (e.g. connection failure) and does not by
// itself invalidate values collected before the failure.
type RoundResult struct {
	ServerID    string
	StartedAt   time.Time
	FinishedAt  time.Time
	Values      []MetricValue
	Interrupted bool
	Err         error
}

// Collector runs one collection round for one server on an already-dialed
// connection owned by the caller. ctx carries the round budget (60s); each
// item must additionally enforce its own 15s timeout. Implementations must
// never block the caller's UI goroutines and must return promptly when ctx
// is cancelled.
type Collector interface {
	Collect(ctx context.Context, conn *sshconn.Conn, server serverassets.Server, kinds []MetricKind) (RoundResult, error)
}

// Template configures which metrics are collected and how often (§7.1).
// A nil/empty Intervals entry falls back to DefaultIntervals.
type Template struct {
	ID        string                       `json:"id"`
	Name      string                       `json:"name"`
	Intervals map[MetricKind]time.Duration `json:"intervals,omitempty"`
}

// DefaultTemplate is the built-in template with the spec §7.1 periods.
func DefaultTemplate() Template {
	intervals := make(map[MetricKind]time.Duration, len(DefaultIntervals))
	for k, v := range DefaultIntervals {
		intervals[k] = v
	}
	return Template{ID: "default", Name: "Default", Intervals: intervals}
}

// Interval returns the collection period for a kind, falling back to the
// spec default.
func (t Template) Interval(kind MetricKind) time.Duration {
	if t.Intervals != nil {
		if d, ok := t.Intervals[kind]; ok && d > 0 {
			return d
		}
	}
	if d, ok := DefaultIntervals[kind]; ok {
		return d
	}
	return 5 * time.Minute
}

// StaleAfter returns how long a value of this kind stays current.
func (t Template) StaleAfter(kind MetricKind) time.Duration {
	return StaleFactor * t.Interval(kind)
}

// Latest is one server's newest successful value per metric kind. Only
// successful values are stored; failures leave the previous value in place
// (to be shown as expired once StaleAfter passes).
type Latest struct {
	ServerID  string                     `json:"server_id"`
	Values    map[MetricKind]MetricValue `json:"values"`
	UpdatedAt time.Time                  `json:"updated_at"`
}

// Get returns the latest value for a kind, if any.
func (l Latest) Get(kind MetricKind) (MetricValue, bool) {
	v, ok := l.Values[kind]
	return v, ok
}

// ExpiredKind reports whether the kind has no value or its value is expired.
func (l Latest) ExpiredKind(kind MetricKind, now time.Time) bool {
	v, ok := l.Values[kind]
	if !ok {
		return true
	}
	return v.Expired(now)
}
