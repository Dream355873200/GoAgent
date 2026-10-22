package permission

import (
	"strings"
)

// 高风险操作识别。bypass 模式不询问用户，但高风险操作仍要经 YOLO 分类器
// 把关；AcceptEdits 模式也不会自动放行高风险的编辑。识别规则是启发式的，
// 覆盖常见的破坏性、提权、外传与持久化操作；漏判时还有 deny 规则与 Guard
// 兜底，宿主可以用 ask 规则补充自己的高风险操作（bypass 模式下命中 ask
// 规则的调用同样交给分类器）。

// HighRisk 判断一次工具调用是否属于高风险操作，返回原因；空串表示不是。
//
// 判定依据：
//   - 工具声明的权限级别为 RequireApproval 或 Dangerous；
//   - shell 命令（任一子命令）属于破坏性删除、磁盘与系统操作、提权、
//     破坏性 git 操作、上传本地文件、建立原始网络连接、发布软件包、
//     执行内联脚本、把下载内容交给解释器执行等；
//   - 文件工具的路径、命令的重定向目标，或修改文件的命令（cp、mv、
//     rm、chmod、tee、sed -i 等）的参数指向系统目录或裸磁盘；
//   - 任何命令的参数涉及凭据与密钥（~/.ssh、~/.aws、id_rsa、.netrc 等）、
//     shell 启动文件或 .git/hooks。
func HighRisk(toolName, input string, level Level) string {
	if level >= LevelRequireApproval {
		return "工具声明为 " + level.String() + " 级别"
	}
	subj := extractSubject(input)
	if subj.command {
		return commandRisk(subj)
	}
	for _, v := range subj.values {
		if isSensitivePath(v) {
			return "涉及敏感路径 " + v
		}
	}
	return ""
}

// commandRisk 检查 shell 命令的各个子命令。
func commandRisk(subj subject) string {
	var download, interpreterReadsStdin bool
	for _, part := range subj.values {
		words := commandWords(part)
		if len(words) == 0 {
			continue
		}
		name := commandName(words[0])
		args := words[1:]
		if r, ok := riskyCommands[name]; ok && r.match(args) {
			return r.reason + "：" + part
		}
		if strings.HasPrefix(name, "mkfs") {
			return "格式化文件系统：" + part
		}
		if interpreters[name] {
			if inlineScript(name, args) {
				return "执行内联脚本：" + part
			}
			if allFlags(args) {
				interpreterReadsStdin = true
			}
		}
		if downloaders[name] {
			download = true
		}
		mutating := mutatingCommands[name] || (name == "sed" && (hasShortFlag(args, 'i') || hasArgPrefix(args, "--in-place")))
		for _, a := range args {
			if isCredentialPath(a) || (mutating && isSystemPath(a)) {
				return "涉及敏感路径 " + strings.Trim(a, `"'`) + "：" + part
			}
		}
	}
	if download && interpreterReadsStdin {
		return "把下载的内容直接交给解释器执行"
	}
	for _, t := range redirectTargets(subj.raw) {
		if isSensitivePath(t) {
			return "重定向写入敏感路径 " + t
		}
	}
	return ""
}

type riskRule struct {
	reason string
	match  func(args []string) bool
}

func always(reason string) riskRule {
	return riskRule{reason: reason, match: func([]string) bool { return true }}
}

func onSubcommand(reason string, subs ...string) riskRule {
	return riskRule{reason: reason, match: func(a []string) bool {
		s := firstNonFlag(a)
		for _, want := range subs {
			if strings.EqualFold(s, want) {
				return true
			}
		}
		return false
	}}
}

// riskyCommands 按命令名（小写、去掉路径与 .exe）登记的高风险规则。
var riskyCommands = map[string]riskRule{
	"rm": {"删除目录或系统路径", func(a []string) bool {
		return hasShortFlag(a, 'r', 'R') || hasArg(a, "--recursive") || hasRootTarget(a)
	}},
	"shred":    always("粉碎文件"),
	"wipefs":   always("擦除磁盘签名"),
	"fdisk":    always("修改磁盘分区"),
	"sfdisk":   always("修改磁盘分区"),
	"parted":   always("修改磁盘分区"),
	"diskpart": always("修改磁盘分区"),
	"dd": {"dd 写入设备或文件", func(a []string) bool {
		return hasArgPrefix(a, "of=")
	}},
	"sudo":   always("提升权限执行"),
	"su":     always("切换用户执行"),
	"doas":   always("提升权限执行"),
	"pkexec": always("提升权限执行"),
	"runas":  always("以其他用户身份执行"),
	"chmod": {"递归或完全开放地修改权限", func(a []string) bool {
		return hasShortFlag(a, 'R') || hasArg(a, "--recursive", "777", "a+rwx", "ugo+rwx")
	}},
	"chown": {"递归修改属主", func(a []string) bool {
		return hasShortFlag(a, 'R') || hasArg(a, "--recursive")
	}},
	"chgrp": {"递归修改属组", func(a []string) bool {
		return hasShortFlag(a, 'R') || hasArg(a, "--recursive")
	}},
	"shutdown":         always("关机或重启"),
	"reboot":           always("关机或重启"),
	"halt":             always("关机或重启"),
	"poweroff":         always("关机或重启"),
	"stop-computer":    always("关机或重启"),
	"restart-computer": always("关机或重启"),
	"init": {"切换系统运行级别", func(a []string) bool {
		return hasArg(a, "0", "6")
	}},
	"systemctl": onSubcommand("停止或禁用系统服务", "stop", "disable", "mask", "kill", "poweroff", "reboot", "halt"),
	"kill": {"终止全部进程或 init", func(a []string) bool {
		return hasArg(a, "-1", "1")
	}},
	"git":  {"破坏性 git 操作", gitRisk},
	"curl": {"上传本地文件", curlUploads},
	"wget": {"上传本地文件", func(a []string) bool {
		return hasArgPrefix(a, "--post-file", "--body-file")
	}},
	"scp":  always("向远程主机传输文件"),
	"sftp": always("向远程主机传输文件"),
	"rsync": {"向远程主机同步文件", func(a []string) bool {
		for _, s := range a {
			if isRemoteSpec(s) {
				return true
			}
		}
		return false
	}},
	"nc":        always("建立原始网络连接"),
	"ncat":      always("建立原始网络连接"),
	"netcat":    always("建立原始网络连接"),
	"socat":     always("建立原始网络连接"),
	"telnet":    always("建立原始网络连接"),
	"eval":      always("动态执行字符串"),
	"npm":       onSubcommand("发布软件包", "publish", "unpublish"),
	"pnpm":      onSubcommand("发布软件包", "publish"),
	"yarn":      onSubcommand("发布软件包", "publish"),
	"cargo":     onSubcommand("发布软件包", "publish"),
	"twine":     onSubcommand("发布软件包", "upload"),
	"gem":       onSubcommand("发布软件包", "push", "yank"),
	"kubectl":   onSubcommand("删除或驱逐集群资源", "delete", "drain"),
	"helm":      onSubcommand("卸载发布", "uninstall", "delete"),
	"terraform": {"变更或销毁基础设施", terraformRisk},
	"tofu":      {"变更或销毁基础设施", terraformRisk},
	"crontab": {"清空计划任务", func(a []string) bool {
		return hasArg(a, "-r")
	}},
	"iptables":  always("修改防火墙规则"),
	"ip6tables": always("修改防火墙规则"),
	"nft":       always("修改防火墙规则"),
	"ufw":       always("修改防火墙规则"),
	"useradd":   always("修改系统账户"),
	"userdel":   always("修改系统账户"),
	"usermod":   always("修改系统账户"),
	"passwd":    always("修改系统账户"),
	"chpasswd":  always("修改系统账户"),
	"visudo":    always("修改提权配置"),
	"reg":       onSubcommand("修改注册表", "delete", "add", "import"),
	"remove-item": {"递归删除", func(a []string) bool {
		return hasArgFold(a, "-recurse", "-r")
	}},
	"rd":    {"递归删除", winRecursive},
	"rmdir": {"递归删除", winRecursive},
	"del":   {"递归删除", winRecursive},
	"erase": {"递归删除", winRecursive},
}

// interpreters 是可以执行内联脚本或从标准输入读取脚本的解释器。
var interpreters = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "fish": true,
	"python": true, "python3": true, "py": true, "perl": true, "ruby": true,
	"node": true, "php": true, "pwsh": true, "powershell": true,
	"iex": true, "invoke-expression": true,
}

// downloaders 是从网络获取内容的命令。
var downloaders = map[string]bool{
	"curl": true, "wget": true, "iwr": true, "irm": true,
	"invoke-webrequest": true, "invoke-restmethod": true,
}

// wrappers 是只负责启动后续命令的包装命令，判定时跳过它们看真正执行的命令。
var wrappers = map[string]bool{
	"env": true, "nohup": true, "time": true, "nice": true, "ionice": true,
	"timeout": true, "xargs": true, "command": true, "builtin": true,
	"exec": true, "stdbuf": true, "unbuffer": true,
}

// commandWords 把子命令拆成词，跳过开头的环境变量赋值、分组符号与包装命令。
func commandWords(part string) []string {
	words := strings.Fields(part)
	for len(words) > 0 {
		w := strings.TrimLeft(words[0], "({!")
		switch {
		case w == "":
			words = words[1:]
		case isEnvAssignment(w):
			words = words[1:]
		case wrappers[commandName(w)]:
			words = words[1:]
			// 包装命令自己的选项与数值参数（xargs -0、nice -n 10、timeout 30）。
			for len(words) > 0 && (strings.HasPrefix(words[0], "-") || isNumeric(words[0])) {
				words = words[1:]
			}
		default:
			words[0] = w
			return words
		}
	}
	return nil
}

// commandName 返回命令名：去掉引号、路径与 .exe 后缀，转小写。
func commandName(w string) string {
	w = strings.Trim(w, `"'`)
	if i := strings.LastIndexAny(w, `/\`); i >= 0 {
		w = w[i+1:]
	}
	return strings.TrimSuffix(strings.ToLower(w), ".exe")
}

func isEnvAssignment(w string) bool {
	i := strings.IndexByte(w, '=')
	if i <= 0 {
		return false
	}
	for j, c := range w[:i] {
		if !(c == '_' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || j > 0 && c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func hasArg(a []string, want ...string) bool {
	for _, s := range a {
		s = strings.Trim(s, `"'`)
		for _, w := range want {
			if s == w {
				return true
			}
		}
	}
	return false
}

func hasArgFold(a []string, want ...string) bool {
	for _, s := range a {
		for _, w := range want {
			if strings.EqualFold(s, w) {
				return true
			}
		}
	}
	return false
}

func hasArgPrefix(a []string, prefixes ...string) bool {
	for _, s := range a {
		for _, p := range prefixes {
			if strings.HasPrefix(s, p) {
				return true
			}
		}
	}
	return false
}

// hasShortFlag 报告是否有短选项簇（如 -rf、-R）包含给定字母之一。
func hasShortFlag(a []string, letters ...rune) bool {
	for _, s := range a {
		if len(s) < 2 || s[0] != '-' || s[1] == '-' {
			continue
		}
		for _, l := range letters {
			if strings.ContainsRune(s[1:], l) {
				return true
			}
		}
	}
	return false
}

func firstNonFlag(a []string) string {
	for _, s := range a {
		if !strings.HasPrefix(s, "-") {
			return strings.Trim(s, `"'`)
		}
	}
	return ""
}

func allFlags(a []string) bool {
	for _, s := range a {
		if s != "-" && !strings.HasPrefix(s, "-") {
			return false
		}
	}
	return true
}

// hasRootTarget 报告 rm 的目标里是否有根目录、家目录、通配全部或敏感路径。
func hasRootTarget(a []string) bool {
	for _, s := range a {
		if strings.HasPrefix(s, "-") {
			continue
		}
		t := strings.Trim(s, `"'`)
		switch t {
		case "/", "/*", "~", "~/", "~/*", "$HOME", "${HOME}", "*", ".", "..", "./*":
			return true
		}
		if isSensitivePath(t) {
			return true
		}
	}
	return false
}

// leadingFlags 返回第一个非选项参数之前的选项；遇到 -m（python 模块）即止。
func leadingFlags(a []string) []string {
	for i, s := range a {
		if !strings.HasPrefix(s, "-") || s == "-m" {
			return a[:i]
		}
	}
	return a
}

// findRisk 识别 find 的 -delete，以及 -exec/-ok 批量执行删除、改权限或解释器。
func findRisk(a []string) bool {
	if hasArg(a, "-delete") {
		return true
	}
	for i, s := range a {
		if s != "-exec" && s != "-execdir" && s != "-ok" && s != "-okdir" {
			continue
		}
		if i+1 >= len(a) {
			continue
		}
		name := commandName(a[i+1])
		if findExecRisky[name] || interpreters[name] {
			return true
		}
		if r, ok := riskyCommands[name]; ok && r.match(a[i+2:]) {
			return true
		}
	}
	return false
}

// find 的规则引用 riskyCommands 本身（检查 -exec 执行的命令），在 init 中登记。
func init() {
	riskyCommands["find"] = riskRule{"find 批量删除或修改文件", findRisk}
}

// findExecRisky 是被 find 批量套用到大量文件上时具有破坏性的命令。
var findExecRisky = map[string]bool{
	"rm": true, "shred": true, "mv": true, "chmod": true, "chown": true, "chgrp": true,
	"dd": true, "truncate": true, "unlink": true,
}

// mutatingCommands 是会修改其参数所指文件的命令；它们的参数指向系统目录
// 时视为高风险（只读命令如 ls、cat 访问系统目录不算）。
var mutatingCommands = map[string]bool{
	"cp": true, "mv": true, "ln": true, "install": true, "tee": true, "touch": true,
	"truncate": true, "mkdir": true, "rmdir": true, "rm": true, "unlink": true,
	"chmod": true, "chown": true, "chgrp": true, "rsync": true, "patch": true,
	"copy-item": true, "move-item": true, "set-content": true, "out-file": true,
	"new-item": true, "remove-item": true, "copy": true, "move": true, "del": true,
}

// winRecursive 报告 cmd.exe 的 rd/rmdir/del 是否带 /s（递归）。
func winRecursive(a []string) bool {
	return hasArgFold(a, "/s")
}

// inlineScript 报告解释器是否以内联脚本方式执行（sh -c、python -c、node -e 等）。
// 只看脚本或模块名之前的选项：之后的参数属于脚本本身（如 python -m pytest -c x.ini）。
func inlineScript(name string, a []string) bool {
	a = leadingFlags(a)
	switch name {
	case "sh", "bash", "zsh", "dash", "ksh", "fish", "python", "python3", "py":
		return hasShortFlag(a, 'c')
	case "perl", "ruby":
		return hasShortFlag(a, 'e', 'E')
	case "node":
		return hasShortFlag(a, 'e', 'p') || hasArg(a, "--eval", "--print")
	case "php":
		return hasShortFlag(a, 'r')
	case "pwsh", "powershell":
		return hasArgFold(a, "-c", "-command", "-e", "-ec", "-encodedcommand")
	}
	return false
}

// gitRisk 识别丢弃工作区修改、改写或删除远端历史等破坏性 git 操作。
func gitRisk(a []string) bool {
	// 跳过 git 的全局选项（-C <dir>、-c <k=v>、--git-dir=… 等）。
	i := 0
	for i < len(a) && strings.HasPrefix(a[i], "-") {
		if a[i] == "-C" || a[i] == "-c" {
			i++
		}
		i++
	}
	if i >= len(a) {
		return false
	}
	sub, rest := a[i], a[i+1:]
	switch sub {
	case "push":
		if hasArg(rest, "--force", "-f", "--force-with-lease", "--delete", "-d", "--mirror", "--prune") ||
			hasArgPrefix(rest, "--force-with-lease=") {
			return true
		}
		for _, s := range rest {
			if !strings.HasPrefix(s, "-") && (strings.HasPrefix(s, "+") || strings.HasPrefix(s, ":")) {
				return true
			}
		}
	case "reset":
		return hasArg(rest, "--hard")
	case "clean":
		return hasShortFlag(rest, 'f') || hasArg(rest, "--force")
	case "branch":
		return hasArg(rest, "-D") || (hasArg(rest, "-d", "--delete") && hasArg(rest, "-f", "--force"))
	case "checkout":
		return hasArg(rest, "-f", "--force", ".")
	case "restore":
		return hasArg(rest, ".")
	case "stash":
		s := firstNonFlag(rest)
		return s == "drop" || s == "clear"
	case "filter-branch", "filter-repo":
		return true
	case "update-ref":
		return hasArg(rest, "-d")
	case "reflog":
		s := firstNonFlag(rest)
		return s == "expire" || s == "delete"
	}
	return false
}

// curlUploads 识别 curl 把本地文件作为请求体或表单上传。
func curlUploads(a []string) bool {
	for i, s := range a {
		switch {
		case s == "-T" || s == "--upload-file" || strings.HasPrefix(s, "--upload-file="):
			return true
		case s == "-d" || s == "--data" || s == "--data-binary" || s == "--data-ascii" || s == "--data-urlencode":
			if i+1 < len(a) && strings.Contains(strings.Trim(a[i+1], `"'`), "@") {
				return true
			}
		case s == "-F" || s == "--form":
			if i+1 < len(a) {
				v := strings.Trim(a[i+1], `"'`)
				if strings.Contains(v, "=@") || strings.Contains(v, "=<") {
					return true
				}
			}
		case strings.HasPrefix(s, "-d@") || strings.HasPrefix(s, "--data=@") || strings.HasPrefix(s, "--data-binary=@"):
			return true
		}
	}
	return false
}

func terraformRisk(a []string) bool {
	switch firstNonFlag(a) {
	case "destroy":
		return true
	case "apply":
		return hasArg(a, "-auto-approve", "--auto-approve", "-destroy")
	}
	return false
}

// isRemoteSpec 识别 rsync 的远程地址（host:path、user@host:path、rsync://）。
func isRemoteSpec(s string) bool {
	s = strings.Trim(s, `"'`)
	if strings.HasPrefix(s, "-") {
		return false
	}
	if strings.HasPrefix(s, "rsync://") {
		return true
	}
	i := strings.IndexByte(s, ':')
	// 单字母前缀是 Windows 盘符（C:\…），不是主机名。
	return i > 1 && !strings.ContainsAny(s[:i], `/\`)
}

// redirectTargets 返回命令中输出重定向（>、>>、&>）的目标。
func redirectTargets(raw string) []string {
	var out []string
	for i := 0; i < len(raw); i++ {
		if raw[i] != '>' {
			continue
		}
		j := i + 1
		if j < len(raw) && raw[j] == '>' {
			j++
		}
		if j < len(raw) && (raw[j] == '&' || raw[j] == '(') {
			continue // >&2、>( … ) 不是文件
		}
		for j < len(raw) && (raw[j] == ' ' || raw[j] == '\t') {
			j++
		}
		k := j
		for k < len(raw) && !strings.ContainsRune(" \t\n;&|<>()", rune(raw[k])) {
			k++
		}
		if k > j {
			out = append(out, strings.Trim(raw[j:k], `"'`))
		}
		i = k - 1
	}
	return out
}

// systemDirs 是写入或删除即可能破坏系统的目录。
var systemDirs = []string{
	"/etc", "/usr", "/bin", "/sbin", "/boot", "/lib", "/lib64", "/root",
	"/var/lib", "/var/spool/cron", "/system", "/library",
	"/windows", "/program files", "/program files (x86)", "/programdata",
}

// deviceDirs 是裸磁盘设备的路径前缀。
var deviceDirs = []string{"/dev/sd", "/dev/hd", "/dev/nvme", "/dev/disk", "/dev/mapper", "/dev/mmcblk"}

// credentialDirs 是凭据与密钥所在的目录名，以及会在 git 操作时执行的 hooks 目录。
var credentialDirs = []string{".ssh", ".aws", ".gnupg", ".kube", ".docker", ".git/hooks"}

// sensitiveFiles 是 shell 启动文件、凭据文件与提权配置的文件名。
var sensitiveFiles = map[string]bool{
	".bashrc": true, ".bash_profile": true, ".bash_login": true, ".profile": true,
	".zshrc": true, ".zprofile": true, ".zshenv": true,
	".netrc": true, ".pgpass": true, ".pypirc": true, ".npmrc": true,
	"authorized_keys": true, "id_rsa": true, "id_ecdsa": true, "id_ed25519": true,
	"sudoers": true,
}

// isSensitivePath 报告路径是否属于凭据路径或系统路径（用于文件工具的路径、
// rm 的目标与重定向目标）。
func isSensitivePath(p string) bool {
	return isCredentialPath(p) || isSystemPath(p)
}

// normalizePath 去掉引号，统一为小写与正斜杠；URL 返回空串。
func normalizePath(p string) string {
	p = strings.ToLower(strings.ReplaceAll(strings.Trim(p, `"'`), `\`, "/"))
	if strings.Contains(p, "://") {
		return ""
	}
	return p
}

// isCredentialPath 报告路径是否指向凭据与密钥目录、shell 启动文件、提权
// 配置或 .git/hooks——读取可能泄露密钥，写入可能留下持久化后门。
func isCredentialPath(p string) bool {
	p = normalizePath(p)
	if p == "" {
		return false
	}
	slashed := "/" + strings.TrimPrefix(p, "/")
	for _, d := range credentialDirs {
		if strings.Contains(slashed, "/"+d+"/") || strings.HasSuffix(slashed, "/"+d) {
			return true
		}
	}
	base := p
	if i := strings.LastIndexByte(base, '/'); i >= 0 {
		base = base[i+1:]
	}
	return sensitiveFiles[base]
}

// isSystemPath 报告路径是否指向系统目录或裸磁盘设备。相对路径只在经 ..
// 回溯时按系统目录判断。
func isSystemPath(p string) bool {
	p = normalizePath(p)
	switch {
	case p == "":
		return false
	case len(p) > 2 && p[1] == ':' && p[2] == '/':
		p = p[2:]
	case strings.HasPrefix(p, "/"):
	case strings.HasPrefix(p, "../"):
		for strings.HasPrefix(p, "../") {
			p = p[3:]
		}
		p = "/" + p
	default:
		return false
	}
	for _, d := range deviceDirs {
		if strings.HasPrefix(p, d) {
			return true
		}
	}
	for _, d := range systemDirs {
		if p == d || strings.HasPrefix(p, d+"/") {
			return true
		}
	}
	return false
}
