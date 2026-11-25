package servercollect

import (
	"context"
	"strings"
	"testing"
	"time"

	"motty/internal/serverassets"
	"motty/internal/sshconn"
)

const basicOpenEuler = `NAME="openEuler"
VERSION="22.03 LTS"
ID="openEuler"
VERSION_ID="22.03"
MOT_KERNEL=5.10.0-60.18.0.50.oe2203.aarch64
MOT_ARCH=aarch64
MOT_HOSTNAME=ky-uos-01
MOT_UPTIME=98765.43 123456.78
MOT_BTIME=1764020000
MOT_MEMTOTAL_KB=16384000
MOT_CPUINFO_BEGIN
processor	: 0
model name	: Phytium, FT-2000/4
physical id	: 0
core id		: 0
processor	: 1
model name	: Phytium, FT-2000/4
physical id	: 0
core id		: 1
processor	: 2
model name	: Phytium, FT-2000/4
physical id	: 1
core id		: 0
processor	: 3
model name	: Phytium, FT-2000/4
physical id	: 1
core id		: 1
MOT_CPUINFO_END
MOT_LSCPU_BEGIN
Architecture: aarch64
CPU(s): 4
Model name: Phytium, FT-2000/4
MOT_LSCPU_END
`

func TestParseBasicOpenEuler(t *testing.T) {
	info := parseBasic([]byte(basicOpenEuler), time.Unix(1764100000, 0))
	if info.OSName != "openEuler" || info.OSVersion != "22.03 LTS" || info.OSID != "openEuler" {
		t.Fatalf("os = %+v", info)
	}
	if info.Kernel != "5.10.0-60.18.0.50.oe2203.aarch64" || info.Arch != "aarch64" || info.Hostname != "ky-uos-01" {
		t.Fatalf("identity = %+v", info)
	}
	if info.BootTime.Unix() != 1764020000 || info.UptimeSecs != 98765.43 {
		t.Fatalf("boot/uptime = %v %v", info.BootTime, info.UptimeSecs)
	}
	if info.CPUModel != "Phytium, FT-2000/4" || info.CPUThreads != 4 || info.CPUCores != 4 {
		t.Fatalf("cpu = %+v", info)
	}
	if info.TotalMem != 16384000<<10 {
		t.Fatalf("total mem = %d", info.TotalMem)
	}
}

func TestParseBasicFallsBackToLSCPUAndUptime(t *testing.T) {
	now := time.Unix(1764100000, 0)
	data := "NAME=\"Ubuntu\"\nVERSION=\"24.04 LTS\"\nID=ubuntu\n" +
		"MOT_KERNEL=6.8.0-31-generic\nMOT_ARCH=arm64\nMOT_HOSTNAME=u24\n" +
		"MOT_UPTIME=3600.00 7200.00\nMOT_BTIME=\nMOT_MEMTOTAL_KB=8192000\n" +
		"MOT_CPUINFO_BEGIN\nMOT_CPUINFO_END\n" +
		"MOT_LSCPU_BEGIN\nModel name: Neoverse-N1\nCPU(s): 8\nCore(s) per socket: 8\nSocket(s): 1\nMOT_LSCPU_END\n"
	info := parseBasic([]byte(data), now)
	if info.Arch != "aarch64" {
		t.Fatalf("arm64 must normalize to aarch64: %q", info.Arch)
	}
	if info.CPUModel != "Neoverse-N1" || info.CPUThreads != 8 || info.CPUCores != 8 {
		t.Fatalf("lscpu fallback = %+v", info)
	}
	if info.BootTime.Unix() != now.Add(-3600*time.Second).Unix() {
		t.Fatalf("boot time must fall back to now-uptime: %v", info.BootTime)
	}
}

func TestParseBasicAllEmptyIsUnavailable(t *testing.T) {
	c := &collector{itemTimeout: time.Second}
	tr := &fakeTransport{run: func(context.Context, string, int) ([]byte, error) {
		return []byte("MOT_KERNEL=\nMOT_ARCH=\nMOT_HOSTNAME=\n"), nil
	}}
	res := c.collect(context.Background(), tr, serverassets.Server{ID: "srv-1"}, []MetricKind{MetricBasic})
	v, _ := valueByKind(res, MetricBasic)
	if v.Unavailable == "" || v.Basic != nil {
		t.Fatalf("empty basic output must be unavailable: %+v", v)
	}
}

const diskstatsSample = `MOT_T1=1000000000000
MOT_DS1_BEGIN
   8       0 sda 100 0 10000 100 50 0 20000 50 0 0 0 0 0 0
   8       1 sda1 60 0 6000 60 30 0 12000 30 0 0 0 0 0 0
   8       2 sda2 40 0 4000 40 20 0 8000 20 0 0 0 0 0 0
 259       0 nvme0n1 10 0 1000 10 5 0 500 5 0 0 0 0 0 0
 259       1 nvme0n1p1 10 0 1000 10 5 0 500 5 0 0 0 0 0 0
 253       0 dm-0 5 0 500 5 2 0 200 2 0 0 0 0 0 0
   7       0 loop0 99 0 9900 99 99 0 9900 99 0 0 0 0 0 0
   1       0 ram0 7 0 700 7 7 0 700 7 0 0 0 0 0 0
MOT_DS1_END
MOT_T2=1001000000000
MOT_DS2_BEGIN
   8       0 sda 200 0 12000 200 100 0 24000 100 0 0 0 0 0 0
   8       1 sda1 120 0 7200 120 60 0 14400 60 0 0 0 0 0 0
   8       2 sda2 80 0 4800 80 40 0 9600 40 0 0 0 0 0 0
 259       0 nvme0n1 20 0 3000 20 10 0 1500 10 0 0 0 0 0 0
 259       1 nvme0n1p1 20 0 3000 20 10 0 1500 10 0 0 0 0 0 0
 253       0 dm-0 10 0 1000 10 4 0 400 4 0 0 0 0 0 0
   7       0 loop0 199 0 19900 199 199 0 19900 199 0 0 0 0 0 0
   1       0 ram0 14 0 1400 14 14 0 1400 14 0 0 0 0 0 0
MOT_DS2_END
`

func TestParseDiskIOAggregatesPartitions(t *testing.T) {
	info, err := parseDiskIO([]byte(diskstatsSample))
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]DiskDevice{}
	for _, d := range info.Devices {
		byName[d.Name] = d
	}
	// sda = own counters + sda1 + sda2: read (2000+1200+800)*512, write (4000+2400+1600)*512
	sda := byName["sda"]
	if !sda.Physical || sda.ReadBytesPerSec == nil || *sda.ReadBytesPerSec != 4000*512 || *sda.WriteBytesPerSec != 8000*512 {
		t.Fatalf("sda = %+v", sda)
	}
	// nvme0n1 = own + p1: read (2000+2000)*512, write (1000+1000)*512
	nvme := byName["nvme0n1"]
	if !nvme.Physical || *nvme.ReadBytesPerSec != 4000*512 || *nvme.WriteBytesPerSec != 2000*512 {
		t.Fatalf("nvme0n1 = %+v", nvme)
	}
	dm := byName["dm-0"]
	if dm.Physical || *dm.ReadBytesPerSec != 500*512 || *dm.WriteBytesPerSec != 200*512 {
		t.Fatalf("dm-0 must be a separate logical device: %+v", dm)
	}
	for _, name := range []string{"sda1", "sda2", "nvme0n1p1", "loop0", "ram0"} {
		if _, ok := byName[name]; ok {
			t.Fatalf("device %s must not be listed separately", name)
		}
	}
	if len(info.Devices) != 3 {
		t.Fatalf("devices = %+v", info.Devices)
	}
}

func TestParseDiskIORejectsMissingReadings(t *testing.T) {
	if _, err := parseDiskIO([]byte("nothing here\n")); err == nil {
		t.Fatal("missing diskstats readings must fail, not fabricate rates")
	}
}

const nicIPOutput = `MOT_IP_BEGIN
1: lo: <LOOPBACK,UP,LOWER_UP> mtu 65536 qdisc noqueue state UNKNOWN mode DEFAULT group default qlen 1000
    link/loopback 00:00:00:00:00:00 brd 00:00:00:00:00:00
2: eth0: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1500 qdisc fq_codel state UP mode DEFAULT group default qlen 1000
    link/ether 52:54:00:12:34:56 brd ff:ff:ff:ff:ff:ff
2: eth0    inet 10.0.0.5/24 brd 10.0.0.255 scope global eth0
2: eth0    inet6 fe80::5054:ff:fe12:3456/64 scope link
3: eth1: <BROADCAST,MULTICAST> mtu 9000 qdisc noop state DOWN mode DEFAULT group default qlen 1000
    link/ether 52:54:00:ab:cd:ef brd ff:ff:ff:ff:ff:ff
MOT_IP_END
MOT_SYSFS_BEGIN
IFACE eth0 MAC=52:54:00:12:34:56 MTU=1500 STATE=up
IFACE eth1 MAC=52:54:00:ab:cd:ef MTU=9000 STATE=down
MOT_SYSFS_END
`

func TestParseNICFromIP(t *testing.T) {
	info, err := parseNIC([]byte(nicIPOutput))
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Interfaces) != 2 {
		t.Fatalf("interfaces = %+v", info.Interfaces)
	}
	eth0 := info.Interfaces[0]
	if eth0.Name != "eth0" || eth0.MAC != "52:54:00:12:34:56" || eth0.MTU != 1500 || eth0.State != "UP" {
		t.Fatalf("eth0 = %+v", eth0)
	}
	if len(eth0.IPv4) != 1 || eth0.IPv4[0] != "10.0.0.5" || len(eth0.IPv6) != 1 || eth0.IPv6[0] != "fe80::5054:ff:fe12:3456" {
		t.Fatalf("eth0 addresses = %+v", eth0)
	}
	if info.Interfaces[1].State != "DOWN" || info.Interfaces[1].MTU != 9000 {
		t.Fatalf("eth1 = %+v", info.Interfaces[1])
	}
}

func TestParseNICFallsBackToSysfs(t *testing.T) {
	data := "MOT_IP_BEGIN\nMOT_IP_END\nMOT_SYSFS_BEGIN\n" +
		"IFACE eth0 MAC=52:54:00:12:34:56 MTU=1500 STATE=up\nMOT_SYSFS_END\n"
	info, err := parseNIC([]byte(data))
	if err != nil || len(info.Interfaces) != 1 || info.Interfaces[0].MAC != "52:54:00:12:34:56" {
		t.Fatalf("sysfs fallback = %+v, err=%v", info, err)
	}
	if len(info.Interfaces[0].IPv4) != 0 {
		t.Fatal("sysfs fallback has no addresses and must not invent any")
	}
}

const systemSystemd = `MOT_UPTIME=123456.78 987654.32
MOT_PROC=231
MOT_PS=229
MOT_HAS_SYSTEMCTL=yes
MOT_SYS_RUNNING=running
MOT_FAILED_BEGIN
foo.service loaded failed failed Foo Daemon
bar.service loaded failed failed Bar Daemon
MOT_FAILED_END
MOT_HAS_SERVICE=yes
`

func TestParseSystemSystemd(t *testing.T) {
	info := parseSystem([]byte(systemSystemd))
	if info.UptimeSecs != 123456.78 || info.ProcessesTotal != 231 || !info.ProcessesOK {
		t.Fatalf("system = %+v", info)
	}
	if info.ServiceManager != "systemd" || len(info.Services) != 2 {
		t.Fatalf("services = %+v", info)
	}
	if info.Services[0] != (ServiceStatus{Name: "foo.service", State: "failed"}) {
		t.Fatalf("service[0] = %+v", info.Services[0])
	}
}

func TestParseSystemSysVFallback(t *testing.T) {
	data := "MOT_UPTIME=60.00 120.00\nMOT_PROC=0\nMOT_PS=42\n" +
		"MOT_HAS_SYSTEMCTL=no\nMOT_SYS_RUNNING=unknown\nMOT_FAILED_BEGIN\nMOT_FAILED_END\nMOT_HAS_SERVICE=yes\n"
	info := parseSystem([]byte(data))
	if info.ServiceManager != "sysv" || len(info.Services) != 0 {
		t.Fatalf("sysv = %+v", info)
	}
	if info.ProcessesTotal != 42 || !info.ProcessesOK {
		t.Fatalf("ps fallback count = %+v", info)
	}
}

func dockerFake(installed, running bool, sudoFail bool) *fakeTransport {
	return &fakeTransport{run: func(ctx context.Context, cmd string, limit int) ([]byte, error) {
		switch {
		case strings.Contains(cmd, "command -v docker"):
			if installed {
				return []byte("MOT_INSTALLED\n"), nil
			}
			return []byte("MOT_MISSING\n"), nil
		case strings.Contains(cmd, "sudo -n true"):
			if sudoFail {
				return []byte("MOT_SUDO_FAIL\n"), nil
			}
			return []byte("MOT_SUDO_OK\n"), nil
		case strings.Contains(cmd, "docker info"):
			if running {
				return []byte("27.1.0\n"), nil
			}
			return []byte("MOT_DOWN\n"), nil
		case strings.Contains(cmd, "docker ps"):
			return []byte("abc123\tweb\tnginx:latest\trunning\tUp 2 hours\t2026-11-25 10:00:00 +0800 CST\t80/tcp\n" +
				"def456\tdb\tpostgres:16\texited\tExited (0) 1 hour ago\t2026-11-25 09:00:00 +0800 CST\t5432/tcp\n"), nil
		default:
			return nil, errUnexpectedCommand
		}
	}}
}

var errUnexpectedCommand = errorString("fake: unexpected docker command")

func TestDockerBranches(t *testing.T) {
	c := &collector{itemTimeout: time.Second}
	server := serverassets.Server{ID: "srv-1"}

	notInstalled := c.collect(context.Background(), dockerFake(false, false, false), server, []MetricKind{MetricDocker})
	v, _ := valueByKind(notInstalled, MetricDocker)
	if v.Unavailable != "" || v.Docker == nil || v.Docker.Installed || v.Docker.Running || len(v.Docker.Containers) != 0 {
		t.Fatalf("not installed must succeed as Installed=false: %+v", v)
	}

	notRunning := c.collect(context.Background(), dockerFake(true, false, false), server, []MetricKind{MetricDocker})
	v, _ = valueByKind(notRunning, MetricDocker)
	if v.Unavailable != "" || v.Docker == nil || !v.Docker.Installed || v.Docker.Running {
		t.Fatalf("installed but down must succeed as Running=false: %+v", v)
	}

	running := c.collect(context.Background(), dockerFake(true, true, false), server, []MetricKind{MetricDocker})
	v, _ = valueByKind(running, MetricDocker)
	if v.Unavailable != "" || v.Docker == nil || !v.Docker.Running || len(v.Docker.Containers) != 2 {
		t.Fatalf("running docker = %+v", v)
	}
	if v.Docker.Containers[0].Name != "web" || v.Docker.Containers[0].State != "running" || v.Docker.Containers[1].State != "exited" {
		t.Fatalf("containers = %+v", v.Docker.Containers)
	}
}

func TestDockerSudoFailureIsUnavailable(t *testing.T) {
	c := &collector{itemTimeout: time.Second}
	server := serverassets.Server{ID: "srv-1", SudoMode: "nopasswd"}
	res := c.collect(context.Background(), dockerFake(true, true, true), server, []MetricKind{MetricDocker})
	v, _ := valueByKind(res, MetricDocker)
	if v.Unavailable == "" || !strings.Contains(v.Unavailable, "sudo") || v.Docker != nil {
		t.Fatalf("sudo failure must be unavailable: %+v", v)
	}
}

func TestDockerSudoPrefixUsedWhenConfigured(t *testing.T) {
	c := &collector{itemTimeout: time.Second}
	server := serverassets.Server{ID: "srv-1", SudoMode: "nopasswd"}
	res := c.collect(context.Background(), dockerFake(true, true, false), server, []MetricKind{MetricDocker})
	v, _ := valueByKind(res, MetricDocker)
	if v.Unavailable != "" || v.Docker == nil || !v.Docker.Running {
		t.Fatalf("nopasswd docker must succeed: %+v", v)
	}
}

var _ = sshconn.PhaseConnected
