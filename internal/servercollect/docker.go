package servercollect

import (
	"context"
	"strings"
	"time"

	"motty/internal/serverassets"
)

// dockerPrefix returns the command prefix for Docker commands. Only the
// pre-configured NOPASSWD sudo mode uses sudo, and only non-interactively
// (sudo -n): no password is ever collected, stored or typed.
func dockerPrefix(sudoMode string) string {
	if strings.EqualFold(strings.TrimSpace(sudoMode), "nopasswd") {
		return "sudo -n "
	}
	return ""
}

func (c *collector) collectDocker(ctx context.Context, tr transport, server serverassets.Server, now time.Time) MetricValue {
	prefix := dockerPrefix(server.SudoMode)

	// 1. Installed? `command -v` with an explicit marker keeps the exit
	// status 0 either way, so a missing binary is a result, not an error.
	data, err := tr.RunCommand(ctx, "command -v docker >/dev/null 2>&1 && echo MOT_INSTALLED || echo MOT_MISSING", 4096)
	if err != nil {
		return unavailable(MetricDocker, now, "docker check failed: "+c.reason(err))
	}
	if !strings.Contains(string(data), "MOT_INSTALLED") {
		// Not installed is a successful result, not an error (§6.10).
		info := DockerInfo{Installed: false, Running: false}
		return success(MetricDocker, now, func(v *MetricValue) { v.Docker = &info })
	}

	// 2. sudo probe: with NOPASSWD mode a failing sudo must be reported
	// as unavailable with its cause, not mistaken for a stopped daemon.
	if prefix != "" {
		data, err := tr.RunCommand(ctx, prefix+"true >/dev/null 2>&1 && echo MOT_SUDO_OK || echo MOT_SUDO_FAIL", 4096)
		if err != nil {
			return unavailable(MetricDocker, now, "docker sudo check failed: "+c.reason(err))
		}
		if strings.Contains(string(data), "MOT_SUDO_FAIL") {
			return unavailable(MetricDocker, now, "sudo -n failed: NOPASSWD sudo is not configured for this user")
		}
	}

	// 3. Running? docker info first, docker version as fallback; either
	// answering means the daemon is up.
	versionCmd := prefix + `docker info --format '{{.ServerVersion}}' 2>/dev/null || ` +
		prefix + `docker version --format '{{.Server.Version}}' 2>/dev/null || echo MOT_DOWN`
	data, err = tr.RunCommand(ctx, versionCmd, 4096)
	if err != nil {
		return unavailable(MetricDocker, now, "docker version check failed: "+c.reason(err))
	}
	info := DockerInfo{Installed: true}
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" || strings.Contains(trimmed, "MOT_DOWN") {
		// Installed but not running (§6.10): a successful result.
		info.Running = false
		return success(MetricDocker, now, func(v *MetricValue) { v.Docker = &info })
	}
	info.Running = true

	// 4. Container list, capped at 128 rows.
	psCmd := prefix + `docker ps --format '{{.ID}}\t{{.Names}}\t{{.Image}}\t{{.State}}\t{{.Status}}\t{{.CreatedAt}}\t{{.Ports}}' 2>/dev/null | head -n 128`
	data, err = tr.RunCommand(ctx, psCmd, 64<<10)
	if err != nil {
		return unavailable(MetricDocker, now, "docker ps failed: "+c.reason(err))
	}
	info.Containers = parseDockerPS(data)
	return success(MetricDocker, now, func(v *MetricValue) { v.Docker = &info })
}

// parseDockerPS parses tab-separated `docker ps --format` rows.
func parseDockerPS(data []byte) []DockerContainer {
	var out []DockerContainer
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) < 7 {
			continue
		}
		out = append(out, DockerContainer{
			ID:      fields[0],
			Name:    fields[1],
			Image:   fields[2],
			State:   fields[3],
			Status:  fields[4],
			Created: fields[5],
			Ports:   fields[6],
		})
	}
	return out
}
