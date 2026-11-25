package serverbatch

import (
	"path"
	"strings"
)

// Risk classification (§10.3).
//
// A command is parsed by shell structure, not by substring search:
// the tokenizer honours single/double quotes, backslash escapes and
// comments; the splitter breaks on ;, &&, ||, | and newlines at nesting
// depth zero (inside $() and backquotes the content is parsed recursively
// as its own command); each subcommand's argv[0] is taken after stripping
// leading sudo/env prefixes and VAR=... assignments, with any directory
// stripped. A rule then matches argv[0] plus its arguments, so
// `echo rm -rf /` (rm is an argument, not argv[0]), `echo "rm -rf /"`
// (quoted), and `# rm -rf /` (a comment) are all normal, while
// `sudo rm -rf /`, `a; rm -rf /`, `echo $(rm -rf /)` and `rm -rf / | cat`
// are all blocked.
//
// The rule table is in classifySubcommand. Levels, worst first:
//   - RiskBlocked: commands that must never run (root wipe, mkfs, dd to a
//     raw device, fork bomb, writing a raw device, chmod/chown -R on /).
//   - RiskCritical: destructive or service-killing commands that need the
//     ConfirmPhrase.
//   - RiskHigh: disruptive commands that need a second confirmation.
//
// Identification cannot cover every risky command: a command can hide
// behind a variable, a script, an alias, or an unfamiliar spelling, so
// the Reasons of any non-normal assessment carry the disclaimer
// "识别无法覆盖全部风险命令，执行前请核对目标与命令". The UI must show
// the command and the target list before executing, whatever the level.

const riskDisclaimer = "识别无法覆盖全部风险命令，执行前请核对目标与命令"

// ClassifyRisk grades command. An empty command is normal (the executor
// rejects it separately as a validation error).
func ClassifyRisk(command string) RiskAssessment {
	level := RiskNormal
	reasons := []string{}
	add := func(l RiskLevel, reason string) {
		if l > level {
			level = l
		}
		reasons = append(reasons, reason)
	}
	// Fork bomb, matched on the whole command with whitespace removed:
	// splitting would scatter it across segments, so the structured
	// parse cannot see it as one command.
	if strings.Contains(strings.Join(strings.Fields(command), ""), ":(){:|:&};:") {
		add(RiskBlocked, "fork 炸弹：会耗尽系统进程")
	}
	subs := parseSubcommands(command)
	for _, sub := range subs {
		classifySubcommand(sub, add)
	}
	// curl/wget piped into a shell (curl ... | sh): the pipe split
	// separates the two halves, so grade the pipeline as a whole.
	for _, pipe := range parsePipelines(command) {
		hasDownload, hasShell := false, false
		for _, sub := range pipe {
			name, _ := sub.argv0()
			if name == "curl" || name == "wget" {
				hasDownload = true
			}
			if isShell(name) {
				hasShell = true
			}
		}
		if hasDownload && hasShell {
			add(RiskCritical, "管道执行网络下载的脚本：来源未经核对")
		}
	}
	if level == RiskNormal {
		return RiskAssessment{Level: RiskNormal}
	}
	// Deduplicate reasons, keeping first-seen order, then the disclaimer.
	seen := map[string]bool{}
	uniq := make([]string, 0, len(reasons)+1)
	for _, r := range reasons {
		if !seen[r] {
			seen[r] = true
			uniq = append(uniq, r)
		}
	}
	uniq = append(uniq, riskDisclaimer)
	return RiskAssessment{Level: level, Reasons: uniq}
}

// --- shell-structure parsing ---

// subcommand is one parsed pipeline segment: argv words (quotes removed,
// comments dropped), redirect targets, and the raw text.
type subcommand struct {
	argv      []string // words after quote removal; argv[0] is the command
	redirects []string // targets of >, >>, 2>, <, etc.
	raw       string
}

// parseSubcommands splits command into pipeline segments. Separators at
// depth zero are ;, &&, ||, | and newlines; $() and backquote contents are
// parsed recursively and appended as their own subcommands (so a dangerous
// command hidden in a substitution is still graded).
func parseSubcommands(command string) []subcommand {
	var out []subcommand
	segments := splitTopLevel(command)
	for _, seg := range segments {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		out = append(out, parseSegment(seg))
		// Recurse into command substitutions and backquotes.
		for _, inner := range substitutionBodies(seg) {
			out = append(out, parseSubcommands(inner)...)
		}
	}
	return out
}

// splitTopLevel splits on ;, &&, ||, | and newlines outside quotes and
// outside $()/backquote nesting. Operators are consumed; the returned
// pieces are pipeline segments.
func splitTopLevel(s string) []string {
	return splitTopLevelMode(s, true)
}

func splitTopLevelMode(s string, splitPipe bool) []string {
	var parts []string
	var cur strings.Builder
	depth := 0
	inSingle, inDouble := false, false
	escaped := false
	flush := func() {
		parts = append(parts, cur.String())
		cur.Reset()
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if escaped {
			cur.WriteByte(c)
			escaped = false
			continue
		}
		switch {
		case c == '\\' && !inSingle:
			escaped = true
			cur.WriteByte(c)
		case c == '\'' && !inDouble:
			inSingle = !inSingle
			cur.WriteByte(c)
		case c == '"' && !inSingle:
			inDouble = !inDouble
			cur.WriteByte(c)
		case !inSingle && !inDouble && c == '$' && i+1 < len(s) && s[i+1] == '(':
			depth++
			cur.WriteByte(c)
			cur.WriteByte('(')
			i++
		case !inSingle && !inDouble && c == '`':
			depth++
			cur.WriteByte(c)
		case !inSingle && !inDouble && depth > 0 && c == ')':
			depth--
			cur.WriteByte(c)
		case !inSingle && !inDouble && depth == 0 && (c == ';' || c == '\n' || c == '&' || (splitPipe && c == '|') || (!splitPipe && c == '|' && i+1 < len(s) && s[i+1] == '|')):
			// && and || are one separator; a lone & backgrounds, also a split.
			// In no-pipe mode a single | stays inside the pipeline, but
			// || is still a separator.
			if (c == '&' || c == '|') && i+1 < len(s) && s[i+1] == c {
				i++
			}
			flush()
		default:
			cur.WriteByte(c)
		}
	}
	flush()
	return parts
}

// substitutionBodies returns the raw insides of $() and backquote spans
// in s (quotes honoured at the outer level).
func substitutionBodies(s string) []string {
	var out []string
	inSingle, inDouble := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && !inSingle:
			i++
		case c == '\'' && !inDouble:
			inSingle = !inSingle
		case c == '"' && !inSingle:
			inDouble = !inDouble
		case !inSingle && !inDouble && c == '$' && i+1 < len(s) && s[i+1] == '(':
			// Find the matching close paren, nesting aware.
			depth := 1
			j := i + 2
			for ; j < len(s) && depth > 0; j++ {
				if s[j] == '(' {
					depth++
				} else if s[j] == ')' {
					depth--
				}
			}
			if depth == 0 {
				out = append(out, s[i+2:j-1])
				out = append(out, substitutionBodies(s[i+2:j-1])...)
				i = j - 1
			}
		case !inSingle && !inDouble && c == '`':
			if j := strings.IndexByte(s[i+1:], '`'); j >= 0 {
				out = append(out, s[i+1:i+1+j])
				i = i + 1 + j
			}
		}
	}
	return out
}

// parseSegment tokenises one pipeline segment into words, honouring
// quotes/escapes/comments, and records redirect targets.
func parseSegment(seg string) subcommand {
	words, redirects := tokenise(seg)
	return subcommand{argv: words, redirects: redirects, raw: seg}
}

// tokenise splits seg into words: single quotes literal, double quotes
// allow $ and backslash, backslash escapes the next byte, and an
// unquoted # starts a comment (to end of line). Redirect operators
// (>, >>, 2>, <, &>, etc.) are recognised and their target word is
// recorded in redirects instead of argv.
func tokenise(seg string) (words []string, redirects []string) {
	var cur strings.Builder
	have := false
	inSingle, inDouble := false, false
	escaped := false
	pendingRedirect := false
	flush := func() {
		if !have {
			return
		}
		w := cur.String()
		if pendingRedirect {
			redirects = append(redirects, w)
			pendingRedirect = false
		} else {
			words = append(words, w)
		}
		cur.Reset()
		have = false
	}
	i := 0
	for i < len(seg) {
		c := seg[i]
		if escaped {
			cur.WriteByte(c)
			have = true
			escaped = false
			i++
			continue
		}
		switch {
		case c == '\\' && !inSingle:
			escaped = true
			i++
		case c == '\'' && !inDouble:
			inSingle = !inSingle
			have = true
			i++
		case c == '"' && !inSingle:
			inDouble = !inDouble
			have = true
			i++
		case !inSingle && !inDouble && c == '#':
			// Comment: rest of the segment is ignored.
			flush()
			return words, redirects
		case !inSingle && !inDouble && (c == ' ' || c == '\t' || c == '\r'):
			flush()
			i++
		case !inSingle && !inDouble && (c == '>' || c == '<'):
			flush()
			// Consume the operator: optional fd digits before it, and
			// >, >>, <, <<, &>, >& forms after it.
			for i < len(seg) && (seg[i] == '>' || seg[i] == '<' || seg[i] == '&') {
				i++
			}
			// A heredoc (<< / <<<) has no file target to grade.
			if strings.Contains(seg[max(0, i-3):i], "<<") {
				pendingRedirect = false
			} else if c == '>' {
				pendingRedirect = true
			}
		case !inSingle && !inDouble && c >= '0' && c <= '9' && i+1 < len(seg) && (seg[i+1] == '>' || seg[i+1] == '<'):
			// "2>" style: the digit belongs to the operator.
			flush()
			i++ // digit
			for i < len(seg) && (seg[i] == '>' || seg[i] == '<' || seg[i] == '&') {
				i++
			}
			if seg[i-1] == '>' {
				pendingRedirect = true
			}
		default:
			cur.WriteByte(c)
			have = true
			i++
		}
	}
	flush()
	return words, redirects
}

// argv0 returns the subcommand's executable name: first word after
// stripping leading sudo, env (and its VAR=... args) and VAR=...
// assignments, with any directory stripped.
func (s subcommand) argv0() (name string, args []string) {
	words := s.argv
	i := 0
	for i < len(words) {
		w := words[i]
		if w == "sudo" || w == "command" || w == "builtin" {
			i++
			continue
		}
		if w == "env" {
			i++
			for i < len(words) && strings.Contains(words[i], "=") && !strings.HasPrefix(words[i], "-") {
				i++
			}
			continue
		}
		if isAssignment(w) {
			i++
			continue
		}
		break
	}
	if i >= len(words) {
		return "", nil
	}
	return path.Base(words[i]), words[i+1:]
}

func isAssignment(w string) bool {
	eq := strings.IndexByte(w, '=')
	if eq <= 0 {
		return false
	}
	name := w[:eq]
	if name == "" {
		return false
	}
	for _, r := range name {
		if !(r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

// hasFlag reports whether args contain flag, in -x, -xyz combined or
// --long form. short is a single letter (e.g. 'r'), long the long name.
func hasFlag(args []string, short byte, long string) bool {
	for _, a := range args {
		if long != "" && (a == "--"+long || strings.HasPrefix(a, "--"+long+"=")) {
			return true
		}
		if len(a) > 1 && a[0] == '-' && a[1] != '-' {
			if strings.IndexByte(a[1:], short) >= 0 {
				return true
			}
		}
	}
	return false
}

func hasAnyFlag(args []string, flags ...string) bool {
	for _, a := range args {
		for _, f := range flags {
			if a == f {
				return true
			}
		}
	}
	return false
}

// isRawDevice reports a device path that is a whole disk or partition
// (writing it destroys data outside any filesystem).
func isRawDevice(p string) bool {
	if !strings.HasPrefix(p, "/dev/") {
		return false
	}
	rest := strings.TrimPrefix(p, "/dev/")
	for _, prefix := range []string{"sd", "hd", "vd", "nvme", "xvd", "dm-", "mapper/"} {
		if strings.HasPrefix(rest, prefix) {
			return true
		}
	}
	return false
}

var sensitiveFiles = []string{
	"/etc/passwd", "/etc/shadow", "/etc/gshadow", "/etc/fstab",
	"/etc/sudoers", "/etc/sudoers.d/",
}

// touchesSensitiveFile reports whether any argument or redirect target
// names a sensitive system file.
func touchesSensitiveFile(s subcommand, args []string) bool {
	check := func(p string) bool {
		for _, f := range sensitiveFiles {
			if p == f || strings.HasPrefix(p, f) {
				return true
			}
		}
		return false
	}
	for _, a := range args {
		if check(a) {
			return true
		}
	}
	for _, r := range s.redirects {
		if check(r) {
			return true
		}
	}
	return false
}

func isShell(name string) bool {
	switch name {
	case "sh", "bash", "zsh", "dash", "fish", "ksh":
		return true
	}
	return false
}

// classifySubcommand grades one pipeline segment and calls add for each
// matching rule (worst level wins in ClassifyRisk).
// parsePipelines splits command into pipelines (lists of segments
// joined by |), for rules that span a whole pipeline.
func parsePipelines(command string) [][]subcommand {
	var out [][]subcommand
	for _, chunk := range splitTopLevelNoPipe(command) {
		chunk = strings.TrimSpace(chunk)
		if chunk == "" {
			continue
		}
		var pipe []subcommand
		for _, seg := range splitTopLevel(chunk) {
			seg = strings.TrimSpace(seg)
			if seg == "" {
				continue
			}
			pipe = append(pipe, parseSegment(seg))
		}
		if len(pipe) > 0 {
			out = append(out, pipe)
		}
	}
	return out
}

// splitTopLevelNoPipe is splitTopLevel without splitting on | (used to
// keep pipelines intact for pipeline-level rules).
func splitTopLevelNoPipe(s string) []string {
	return splitTopLevelMode(s, false)
}

func classifySubcommand(s subcommand, add func(RiskLevel, string)) {
	// Writing a raw disk device through a redirect (e.g. cat > /dev/sda).
	for _, target := range s.redirects {
		if isRawDevice(target) {
			add(RiskBlocked, "直接写磁盘设备 "+target+"：会破坏磁盘数据")
		}
		if target == "/" || target == "/*" {
			add(RiskBlocked, "写入根目录会破坏系统")
		}
	}

	name, args := s.argv0()
	if name == "" {
		return
	}
	// A shell reading from a curl/wget download (curl ... | sh, or
	// sh <(curl ...)): the pipeline split already separates the pipe,
	// so this catches the process-substitution form.
	if isShell(name) {
		for _, a := range args {
			if strings.Contains(a, "curl") || strings.Contains(a, "wget") {
				add(RiskCritical, "管道执行网络下载的脚本：来源未经核对")
			}
		}
	}

	switch {
	case name == "rm":
		recursive := hasFlag(args, 'r', "recursive")
		targets := nonFlagArgs(args)
		rootTarget := false
		for _, t := range targets {
			if t == "/" || t == "/*" || t == "/." {
				rootTarget = true
			}
		}
		if recursive && rootTarget {
			add(RiskBlocked, "rm -rf 根目录：会删除整个系统")
			return
		}
		if recursive {
			add(RiskCritical, "rm -rf 会递归删除目录，请核对目标路径")
			return
		}
		// Plain rm of a sensitive file is still destructive.
		if touchesSensitiveFile(s, args) {
			add(RiskCritical, "删除系统关键文件")
			return
		}
	case strings.HasPrefix(name, "mkfs"):
		add(RiskBlocked, "格式化文件系统会清空磁盘数据")
	case name == "dd":
		for _, a := range args {
			if strings.HasPrefix(a, "of=") {
				target := strings.TrimPrefix(a, "of=")
				if isRawDevice(target) {
					add(RiskBlocked, "dd 写入磁盘设备 "+target+"：会覆盖磁盘数据")
				} else {
					add(RiskCritical, "dd 写盘会覆盖目标数据")
				}
				return
			}
		}
		// dd without of= writes to stdout; still a raw read, not a write.
	case name == "chmod":
		if hasFlag(args, 'R', "recursive") {
			targets := nonFlagArgs(args)
			for _, t := range targets {
				if t == "/" {
					add(RiskBlocked, "chmod -R 修改根目录权限会破坏系统")
					return
				}
			}
			add(RiskHigh, "chmod -R 递归修改权限")
			return
		}
	case name == "chown":
		if hasFlag(args, 'R', "recursive") {
			targets := nonFlagArgs(args)
			for _, t := range targets {
				if t == "/" {
					add(RiskBlocked, "chown -R 修改根目录属主会破坏系统")
					return
				}
			}
			add(RiskHigh, "chown -R 递归修改属主")
			return
		}
	case name == "fdisk", name == "parted", name == "sfdisk", name == "mkswap",
		name == "pvcreate", name == "vgcreate", name == "lvcreate", name == "mkfs.ext4":
		add(RiskCritical, "分区/格式化操作会改变磁盘结构")
	case name == "iptables", name == "ip6tables", name == "nft":
		if hasAnyFlag(args, "-F", "--flush") {
			add(RiskCritical, "清空防火墙规则会放行全部流量")
		}
	case name == "systemctl":
		if len(args) > 0 {
			action, target := args[0], ""
			if len(args) > 1 {
				target = args[len(args)-1]
			}
			switch action {
			case "stop":
				if isCriticalService(target) {
					add(RiskCritical, "停止关键服务 "+target+" 可能导致失联")
				} else {
					add(RiskHigh, "停止服务 "+target)
				}
			case "restart":
				if isCriticalService(target) {
					add(RiskCritical, "重启关键服务 "+target+" 可能导致失联")
				} else {
					add(RiskHigh, "重启服务 "+target)
				}
			}
		}
	case name == "service":
		if len(args) >= 2 && (args[1] == "restart" || args[1] == "stop") {
			if isCriticalService(args[0]) {
				add(RiskCritical, "重启/停止关键服务 "+args[0]+" 可能导致失联")
			} else {
				add(RiskHigh, "重启/停止服务 "+args[0])
			}
		}
	case name == "reboot", name == "shutdown", name == "poweroff":
		if hasFlag(args, 'f', "force") {
			add(RiskHigh, "强制重启/关机可能丢失未保存数据")
		} else {
			add(RiskCritical, "重启/关机将中断服务器连接")
		}
	case name == "halt":
		if hasAnyFlag(args, "-f", "-p", "--force", "--poweroff") {
			add(RiskHigh, "强制停机可能丢失未保存数据")
		} else {
			add(RiskCritical, "停机将中断服务器连接")
		}
	case name == "userdel":
		add(RiskCritical, "删除用户账号不可恢复")
	case name == "passwd":
		add(RiskCritical, "修改用户密码")
	case name == "usermod":
		if touchesSensitiveFile(s, args) {
			add(RiskCritical, "修改系统账号文件")
		}
	case name == "visudo", name == "vipw":
		add(RiskCritical, "修改系统账号/权限文件")
	case name == "crontab":
		add(RiskHigh, "修改定时任务")
	case name == "kill", name == "killall", name == "pkill":
		add(RiskHigh, "终止进程可能中断服务")
	case name == "apt", name == "apt-get", name == "yum", name == "dnf", name == "zypper":
		if len(args) > 0 {
			switch args[0] {
			case "install", "remove", "purge", "update", "upgrade", "autoremove":
				add(RiskHigh, "软件包安装/卸载/更新会改变系统软件")
			}
		}
	case name == "journalctl":
		if hasFlag(args, 0, "vacuum-size") || hasFlag(args, 0, "vacuum-time") || hasFlag(args, 0, "vacuum-files") {
			add(RiskHigh, "清理系统日志会删除历史日志")
		}
	case name == "docker":
		if len(args) > 0 {
			switch args[0] {
			case "rm", "rmi", "prune":
				add(RiskHigh, "删除容器/镜像会丢失数据")
			case "system":
				if len(args) > 1 && args[1] == "prune" {
					add(RiskHigh, "清理 Docker 资源会删除容器与镜像")
				}
			case "volume":
				if len(args) > 1 && (args[1] == "rm" || args[1] == "prune") {
					add(RiskHigh, "删除 Docker 卷会丢失数据")
				}
			}
		}
	}

	// Sensitive-file writes via any command (sed -i /etc/passwd,
	// echo x > /etc/shadow, ...): critical unless already graded.
	if touchesSensitiveFile(s, args) {
		add(RiskCritical, "修改系统关键文件")
	}
	// Cron file edits.
	for _, a := range args {
		if a == "/etc/crontab" || strings.HasPrefix(a, "/etc/cron.") {
			add(RiskHigh, "修改定时任务配置")
		}
	}
	for _, r := range s.redirects {
		if r == "/etc/crontab" || strings.HasPrefix(r, "/etc/cron.") {
			add(RiskHigh, "修改定时任务配置")
		}
	}
}

// nonFlagArgs returns args that are not option flags (and skips the value
// of options that take one, best-effort: only -o/--output style pairs
// named in the caller's rules are not modelled; flags with "=" are
// skipped as a whole).
func nonFlagArgs(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			out = append(out, args[i+1:]...)
			return out
		}
		if strings.HasPrefix(a, "-") && a != "-" {
			// Options that take a separate value in the graded
			// commands (chmod --reference, chown --from): skip next.
			if a == "--reference" || a == "--from" {
				i++
			}
			continue
		}
		out = append(out, a)
	}
	return out
}

func isCriticalService(name string) bool {
	switch strings.ToLower(strings.TrimSuffix(name, ".service")) {
	case "sshd", "ssh", "docker", "containerd", "network", "networkmanager",
		"networking", "systemd-networkd", "firewalld":
		return true
	}
	return false
}
