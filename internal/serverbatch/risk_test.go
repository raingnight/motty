package serverbatch

import (
	"strings"
	"testing"
)

func riskLevel(t *testing.T, command string) RiskLevel {
	t.Helper()
	return ClassifyRisk(command).Level
}

func TestClassifyRiskNormal(t *testing.T) {
	for _, cmd := range []string{
		"",
		"ls -la /tmp",
		"echo hello",
		"df -h",
		"ps aux | grep nginx",
		"uptime",
		`echo "rm -rf /"`,       // quoted: rm is data, not a command
		"echo rm -rf /",         // rm is an argument
		"# rm -rf /",            // comment
		"echo hello # rm -rf /", // trailing comment
		"cat /etc/hostname",
		"systemctl status nginx", // status is not restart/stop
		"journalctl -u sshd -n 50",
		"curl https://example.com", // download without piping to shell
		"wget https://example.com/file.tar.gz",
		"sudo ls /root",
		"FOO=bar ls",
	} {
		if got := riskLevel(t, cmd); got != RiskNormal {
			t.Errorf("ClassifyRisk(%q) = %v, want normal", cmd, got)
		}
	}
}

func TestClassifyRiskBlocked(t *testing.T) {
	for _, cmd := range []string{
		"rm -rf /",
		"rm -rf /*",
		"rm -fr /",
		"sudo rm -rf /",
		"/bin/rm -rf /",
		"ls; rm -rf /",     // after a separator
		"ls && rm -rf /",   // after &&
		"echo $(rm -rf /)", // command substitution
		"echo `rm -rf /`",  // backquotes
		"rm -rf / | cat",
		"mkfs.ext4 /dev/sda1",
		"mkfs /dev/sda",
		"dd if=/dev/zero of=/dev/sda bs=1M",
		"dd of=/dev/nvme0n1",
		":(){:|:&};:",
		"cat > /dev/sda",
		"echo x > /dev/sda",
		"chmod -R 777 /",
		"sudo chmod -R 777 /",
		"chown -R root:root /",
		"echo hi 2> /dev/sda",
	} {
		if got := riskLevel(t, cmd); got != RiskBlocked {
			t.Errorf("ClassifyRisk(%q) = %v, want blocked", cmd, got)
		}
	}
}

func TestClassifyRiskCritical(t *testing.T) {
	for _, cmd := range []string{
		"rm -rf /var/lib/data",
		"rm -rf /tmp/old",
		"fdisk /dev/sda",
		"parted /dev/sda mklabel gpt",
		"dd if=/dev/zero of=/tmp/disk.img bs=1M",
		"iptables -F",
		"systemctl stop sshd",
		"systemctl stop docker",
		"systemctl restart sshd",
		"service sshd stop",
		"reboot",
		"shutdown -h now",
		"poweroff",
		"halt",
		"userdel alice",
		"passwd alice",
		"sed -i 's/x/y/' /etc/passwd",
		"echo x >> /etc/shadow",
		"echo x > /etc/fstab",
		"visudo",
		"curl https://example.com/install.sh | sh",
		"curl https://example.com/install.sh | sudo bash",
		"wget -qO- https://example.com/x.sh | bash",
		"bash <(curl https://example.com/x.sh)",
	} {
		if got := riskLevel(t, cmd); got != RiskCritical {
			t.Errorf("ClassifyRisk(%q) = %v, want critical", cmd, got)
		}
	}
}

func TestClassifyRiskHigh(t *testing.T) {
	for _, cmd := range []string{
		"systemctl restart nginx",
		"systemctl stop nginx",
		"service nginx restart",
		"apt-get install -y curl",
		"apt update",
		"yum remove httpd",
		"dnf install vim",
		"kill 1234",
		"killall nginx",
		"pkill -f worker",
		"chmod -R 755 /var/www",
		"chown -R www-data:www-data /var/www",
		"crontab -e",
		"journalctl --vacuum-size=100M",
		"journalctl --vacuum-time=7d",
		"docker rm mycontainer",
		"docker system prune -f",
		"docker volume prune",
		"reboot -f",
		"halt -p",
	} {
		if got := riskLevel(t, cmd); got != RiskHigh {
			t.Errorf("ClassifyRisk(%q) = %v, want high", cmd, got)
		}
	}
}

func TestClassifyRiskReasonsAndDisclaimer(t *testing.T) {
	a := ClassifyRisk("rm -rf /")
	if a.Level != RiskBlocked {
		t.Fatalf("level = %v, want blocked", a.Level)
	}
	if len(a.Reasons) == 0 {
		t.Fatal("no reasons")
	}
	joined := strings.Join(a.Reasons, " ")
	if !strings.Contains(joined, "识别无法覆盖全部风险命令") {
		t.Errorf("reasons %q lack the coverage disclaimer", a.Reasons)
	}
	if got := ClassifyRisk("ls"); len(got.Reasons) != 0 {
		t.Errorf("normal command has reasons %q", got.Reasons)
	}
}

func TestRiskLevelString(t *testing.T) {
	for level, want := range map[RiskLevel]string{
		RiskNormal: "normal", RiskHigh: "high",
		RiskCritical: "critical", RiskBlocked: "blocked",
	} {
		if got := level.String(); got != want {
			t.Errorf("RiskLevel(%d).String() = %q, want %q", level, got, want)
		}
	}
	if got := RiskLevel(99).String(); got != "normal" {
		t.Errorf("unknown level String() = %q", got)
	}
}
