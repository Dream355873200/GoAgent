package permission

import (
	"encoding/json"
	"strings"
)

// subject 是规则匹配的对象：从工具入参里提取出的命令 / 路径 / URL 等。
type subject struct {
	// values 是参与匹配的值：shell 命令被拆成的各个子命令（含命令替换
	// 内部的命令）；非命令入参只有一个值。
	values []string
	// raw 是提取出的原始值（拆分前），用于不带通配符模式的子串匹配。
	raw string
	// command 表示值来自 shell 命令字段。
	command bool
	// risky 表示命令含有无法静态判定的结构（命令替换、进程替换、输出
	// 重定向）：这样的命令不会被 allow 规则或"始终允许"自动放行。
	risky bool
}

// commandKeys 与 valueKeys 是提取匹配对象时依次查找的入参字段。
var (
	commandKeys = []string{"command", "cmd", "script"}
	valueKeys   = []string{"file_path", "path", "notebook_path", "url", "pattern", "query"}
)

// extractSubject 解析工具入参。入参不是 JSON 对象时按 shell 命令处理
// （Guard/规则的调用方可能直接传命令文本）。
func extractSubject(input string) subject {
	var obj map[string]any
	if err := json.Unmarshal([]byte(input), &obj); err != nil || obj == nil {
		return commandSubject(input)
	}
	for _, k := range commandKeys {
		if s, ok := obj[k].(string); ok {
			return commandSubject(s)
		}
	}
	for _, k := range valueKeys {
		if s, ok := obj[k].(string); ok {
			return subject{values: []string{s}, raw: s}
		}
	}
	return subject{values: []string{input}, raw: input}
}

func commandSubject(cmd string) subject {
	parts, risky := splitShell(cmd, 0)
	if len(parts) == 0 {
		parts = []string{strings.TrimSpace(cmd)}
	}
	return subject{values: parts, raw: cmd, command: true, risky: risky}
}

// splitShell 按 shell 控制操作符（; && || | & 换行）把命令拆成子命令，
// 引号内的内容不拆；命令替换 $(...)、`...`、<(...) 的内部命令作为额外的
// 子命令一并返回。返回的 risky 表示出现了替换或输出重定向。
func splitShell(cmd string, depth int) (parts []string, risky bool) {
	var cur strings.Builder
	flush := func() {
		if s := strings.TrimSpace(cur.String()); s != "" {
			parts = append(parts, s)
		}
		cur.Reset()
	}
	var quote byte // 当前所处的引号（0 = 不在引号内）
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		switch {
		case c == '\\' && quote != '\'' && i+1 < len(cmd):
			cur.WriteByte(c)
			cur.WriteByte(cmd[i+1])
			i++
			continue
		case quote != 0:
			if c == quote {
				quote = 0
			} else if quote == '"' && (c == '`' || (c == '$' && i+1 < len(cmd) && cmd[i+1] == '(')) {
				// 双引号内的命令替换照样会执行。
				inner, end := substitution(cmd, i)
				risky = true
				parts = append(parts, nested(inner, depth)...)
				cur.WriteString(cmd[i:end])
				i = end - 1
				continue
			}
			cur.WriteByte(c)
			continue
		case c == '\'' || c == '"':
			quote = c
			cur.WriteByte(c)
			continue
		case c == '`' || ((c == '$' || c == '<' || c == '>') && i+1 < len(cmd) && cmd[i+1] == '('):
			inner, end := substitution(cmd, i)
			risky = true
			parts = append(parts, nested(inner, depth)...)
			cur.WriteString(cmd[i:end])
			i = end - 1
			continue
		case c == '\n' || c == ';':
			flush()
			continue
		case c == '&' || c == '|':
			if c == '&' && i > 0 && (cmd[i-1] == '>' || cmd[i-1] == '<') {
				cur.WriteByte(c) // 2>&1 这类重定向的一部分
				continue
			}
			flush()
			if i+1 < len(cmd) && cmd[i+1] == c {
				i++
			}
			continue
		case c == '>':
			if !(i+1 < len(cmd) && cmd[i+1] == '&') {
				risky = true
			}
		}
		cur.WriteByte(c)
	}
	flush()
	return parts, risky
}

// substitution 返回从 start 开始的命令替换的内部命令和结束位置（不含）。
func substitution(cmd string, start int) (inner string, end int) {
	if cmd[start] == '`' {
		if j := strings.IndexByte(cmd[start+1:], '`'); j >= 0 {
			return cmd[start+1 : start+1+j], start + 2 + j
		}
		return cmd[start+1:], len(cmd)
	}
	// $( / <( / >(：按括号配对找到结尾。
	level := 0
	for j := start + 1; j < len(cmd); j++ {
		switch cmd[j] {
		case '(':
			level++
		case ')':
			level--
			if level == 0 {
				return cmd[start+2 : j], j + 1
			}
		}
	}
	return cmd[start+2:], len(cmd)
}

func nested(inner string, depth int) []string {
	if depth >= 4 {
		return []string{strings.TrimSpace(inner)}
	}
	parts, _ := splitShell(inner, depth+1)
	return parts
}

// splitToolPattern 把 "Bash(git *)" 拆成工具名模式 "Bash" 和入参模式 "git *"。
func splitToolPattern(p string) (name, input string) {
	if i := strings.Index(p, "("); i >= 0 && strings.HasSuffix(p, ")") {
		return p[:i], strings.TrimSpace(p[i+1 : len(p)-1])
	}
	return p, ""
}

// matchToolName 匹配工具名："*" 匹配全部，"Read*" 前缀匹配，其余精确匹配。
func matchToolName(pattern, tool string) bool {
	switch {
	case pattern == "*":
		return true
	case strings.HasSuffix(pattern, "*"):
		return strings.HasPrefix(tool, strings.TrimSuffix(pattern, "*"))
	default:
		return pattern == tool
	}
}

// matchValue 判断单个值是否匹配入参模式：
//   - 含 * 的模式按通配符整体匹配（* 匹配任意字符），"git *" 也匹配单独的 "git"；
//   - 不含 * 的模式要求值等于模式，或以"模式 + 空格"开头（按词前缀，
//     "rm" 不会匹配 "rmdir"）。
func matchValue(pattern, v string) bool {
	if strings.Contains(pattern, "*") {
		if strings.HasSuffix(pattern, " *") && v == strings.TrimSuffix(pattern, " *") {
			return true
		}
		return wildcard(pattern, v)
	}
	return v == pattern || strings.HasPrefix(v, pattern+" ")
}

// wildcard 实现只含 * 的通配符整体匹配。
func wildcard(pattern, s string) bool {
	p, i := 0, 0
	star, mark := -1, 0
	for i < len(s) {
		switch {
		case p < len(pattern) && pattern[p] == '*':
			star, mark = p, i
			p++
		case p < len(pattern) && pattern[p] == s[i]:
			p++
			i++
		case star >= 0:
			p = star + 1
			mark++
			i = mark
		default:
			return false
		}
	}
	for p < len(pattern) && pattern[p] == '*' {
		p++
	}
	return p == len(pattern)
}

// alwaysAllowKey 计算"始终允许"授权的记忆键：普通工具按工具名；shell
// 命令按"工具名 + 命令前一到两个词"，复合命令或含替换/重定向的命令不
// 记忆（ok=false）——批准一次 `ls` 不能顺带放行 `rm -rf /`。
func alwaysAllowKey(toolName, input string) (key string, ok bool) {
	keys := alwaysAllowCandidates(toolName, input)
	if len(keys) == 0 {
		return "", false
	}
	return keys[len(keys)-1], true
}

// alwaysAllowCandidates 返回查询"始终允许"时要检查的键（由宽到窄）：
// 批准过 `ls -la`（键为 ls）后 `ls src` 也命中；批准过 `git status`
// （键为 git status）后 `git push` 不命中。
func alwaysAllowCandidates(toolName, input string) []string {
	subj := extractSubject(input)
	if !subj.command {
		return []string{toolName}
	}
	if subj.risky || len(subj.values) != 1 {
		return nil
	}
	fields := strings.Fields(subj.values[0])
	if len(fields) == 0 {
		return nil
	}
	keys := []string{toolName + "\x00" + fields[0]}
	if len(fields) > 1 && !strings.HasPrefix(fields[1], "-") && !strings.ContainsAny(fields[1], `/\.=:"'`) {
		keys = append(keys, toolName+"\x00"+fields[0]+" "+fields[1])
	}
	return keys
}
